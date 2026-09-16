// Package mta is the New York City Subway provider. Real-time data is the
// MTA's keyless GTFS-Realtime feeds (one per line group); stop and route
// names come from the static GTFS zip, downloaded weekly.
package mta

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/gtfs"
	"github.com/bancsdan/eta/internal/gtfsrt"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const (
	DefaultFeedURL   = "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs"
	DefaultStaticURL = "https://rrgtfsfeeds.s3.amazonaws.com/gtfs_subway.zip"
	staticTTL        = 7 * 24 * time.Hour
)

var info = transit.Info{
	ID:       "newyork",
	Name:     "New York City Subway (MTA)",
	Provider: "mta",
	Country:  "usa",
	TZ:       "America/New_York",
	Realtime: true,
	Notes:    "subway only; first run downloads the static timetable (5 MB); no scheduled fallback",
	Probe:    transit.Probe{Route: "L", Query: "bedford av"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "usa", Name: "United States", Aliases: nil, Providers: []string{"mta"}})
	registry.Register(registry.Entry{Info: info, Aliases: []string{"nyc", "mta", "nyc-subway"}, New: New})
}

// feedSuffixes maps a route to the feed that carries it; routes not listed
// (1-7, GS) are in the base feed.
var feedSuffixes = map[string]string{
	"A": "-ace", "C": "-ace", "E": "-ace", "H": "-ace", "FS": "-ace",
	"B": "-bdfm", "D": "-bdfm", "F": "-bdfm", "FX": "-bdfm", "M": "-bdfm",
	"G": "-g", "J": "-jz", "Z": "-jz",
	"N": "-nqrw", "Q": "-nqrw", "R": "-nqrw", "W": "-nqrw",
	"L": "-l", "SI": "-si",
}

var allSuffixes = []string{"", "-ace", "-bdfm", "-g", "-jz", "-nqrw", "-l", "-si"}

type Provider struct {
	FeedURL string
	Static  *gtfs.Feed

	http  *httpx.Client
	cache *cache.Cache
	now   func() time.Time
	loc   *time.Location

	mu     sync.Mutex
	routes []transit.Route
}

func New(d registry.Deps) transit.Provider {
	h := httpx.New("mta", d.HTTP)
	dir := ""
	if d.Cache != nil {
		dir = d.Cache.Dir
	}
	if dir == "" {
		dir = cache.Default(info.ID, staticTTL).Dir
	}
	return &Provider{
		FeedURL: DefaultFeedURL,
		Static:  &gtfs.Feed{URL: DefaultStaticURL, Path: filepath.Join(dir, "gtfs", "subway.zip"), TTL: staticTTL, HTTP: h, Now: d.Clock()},
		http:    h,
		cache:   d.Cache,
		now:     d.Clock(),
		loc:     info.Location(),
	}
}

func (p *Provider) Info() transit.Info { return info }

// Cold reports whether the static timetable still has to be downloaded.
func (p *Provider) Cold() bool { return p.Static.Cold() }

func (p *Provider) allRoutes(ctx context.Context) ([]transit.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes != nil {
		return p.routes, nil
	}
	rs, err := p.Static.Routes(ctx)
	if err != nil {
		return nil, err
	}
	p.routes = rs
	return rs, nil
}

// FindRoutes matches the public letter or number; the three shuttles all
// carry "S", so a query for S returns all of them.
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	rs, err := p.allRoutes(ctx)
	if err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var out []transit.Route
	var others []string
	for _, r := range rs {
		if match.Normalize(r.ShortName) == want || match.Normalize(r.ID) == want {
			out = append(out, r)
			continue
		}
		others = append(others, r.ShortName)
	}
	if len(out) == 0 {
		return nil, transit.UnknownRoute(short, others)
	}
	return out, nil
}

// RouteStops derives the patterns from stop_times.txt, which is slow (half
// a million rows), so the result is cached for a day.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	key := "patterns-" + r.ID
	var ps []transit.Pattern
	if p.cache.Load(key, &ps) && len(ps) > 0 {
		return ps, nil
	}
	ps, err := p.Static.Patterns(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	// Patterns list platforms (L01N); the rider-facing stop is the parent
	// station, whose id is what Departures expands back to platforms.
	for i := range ps {
		for j, s := range ps[i].Stops {
			if s.ParentID != "" {
				ps[i].Stops[j] = transit.Stop{ID: s.ParentID, Name: s.Name, Lat: s.Lat, Lon: s.Lon}
			}
		}
	}
	_ = p.cache.Store(key, ps)
	return ps, nil
}

// SearchStops matches station names from stops.txt (parent stations only).
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	stops, err := p.Static.Stops(ctx)
	if err != nil {
		return nil, err
	}
	var stations []match.Stop
	byID := map[string]transit.Stop{}
	for _, s := range stops {
		if s.ParentID != "" {
			continue
		}
		stations = append(stations, match.Stop{ID: s.ID, Name: s.Name})
		byID[s.ID] = s
	}
	cands := match.Find(query, stations)
	if len(cands) == 0 {
		cands = match.Closest(query, stations, 8)
	}
	var out []transit.Stop
	for _, c := range cands {
		for _, id := range c.IDs {
			out = append(out, byID[id])
		}
	}
	return out, nil
}

// Departures expands stations to their platforms, fetches the feeds the
// requested routes live in (all eight without a route), and labels each
// update with the trip's last stop as headsign. The feeds carry no scheduled
// times, so every entry is Live.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now()}
	if len(stopIDs) == 0 {
		return out, nil
	}
	stops, err := p.Static.Stops(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(stops))
	platforms := map[string]bool{}
	want := map[string]bool{}
	for _, id := range stopIDs {
		want[id] = true
	}
	for _, s := range stops {
		names[s.ID] = s.Name
		if want[s.ID] || want[s.ParentID] {
			platforms[s.ID] = true
		}
	}
	suffixes := allSuffixes
	if len(routes) > 0 {
		seen := map[string]bool{}
		suffixes = nil
		for _, r := range routes {
			sfx := feedSuffixes[r.ID]
			if !seen[sfx] {
				seen[sfx] = true
				suffixes = append(suffixes, sfx)
			}
		}
	}
	trips, _ := p.Static.Trips(ctx) // headsign/direction enrichment only

	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, len(suffixes))
	var now time.Time
	for i, sfx := range suffixes {
		wg.Add(1)
		go func(i int, sfx string) {
			defer wg.Done()
			msg, err := gtfsrt.Fetch(ctx, p.http, p.FeedURL+sfx)
			if err != nil {
				errs[i] = err
				return
			}
			ups := gtfsrt.StopUpdates(msg, platforms)
			mu.Lock()
			defer mu.Unlock()
			if ts := gtfsrt.Timestamp(msg); !ts.IsZero() && ts.After(now) {
				now = ts
			}
			for _, u := range ups {
				out.Departures = append(out.Departures, p.toDeparture(u, names, trips))
			}
		}(i, sfx)
	}
	wg.Wait()
	failed := 0
	var first error
	for _, e := range errs {
		if e != nil {
			failed++
			if first == nil {
				first = e
			}
		}
	}
	if failed == len(suffixes) {
		return nil, first
	}
	if !now.IsZero() {
		out.Now = now.In(p.loc)
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

func (p *Provider) toDeparture(u gtfsrt.Update, names map[string]string, trips map[string]gtfs.Trip) transit.Departure {
	d := transit.Departure{
		At:        u.At.In(p.loc),
		Live:      true,
		Cancelled: u.Cancelled,
		Line:      shortName(u.RouteID),
		RouteID:   u.RouteID,
		TripID:    u.TripID,
		StopID:    u.StopID,
		Headsign:  names[u.LastStopID],
	}
	// Platform ids end in N or S: the feed's own direction flag.
	if n := len(u.StopID); n > 0 {
		switch u.StopID[n-1] {
		case 'N':
			d.DirectionID = "N"
		case 'S':
			d.DirectionID = "S"
		}
	}
	if t, ok := trips[u.TripID]; ok && t.Headsign != "" {
		d.Headsign = t.Headsign
	}
	if d.Headsign == "" {
		d.Headsign = map[string]string{"N": "Uptown", "S": "Downtown"}[d.DirectionID]
	}
	return d
}

// shortName maps a route id to the label riders know; the shuttles are "S".
func shortName(routeID string) string {
	switch routeID {
	case "GS", "FS", "H":
		return "S"
	case "SI":
		return "SIR"
	}
	return strings.ToUpper(routeID)
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
