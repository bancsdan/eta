// Package mta is the New York MTA provider: the subway, the Long Island
// Rail Road and Metro-North, all from the MTA's keyless GTFS-Realtime feeds
// with names from the static GTFS zips (downloaded weekly). "newyork" is
// the subway; any other town is matched against the station names of all
// three systems, which cover Long Island, the Hudson Valley and Connecticut.
package mta

import (
	"context"
	"fmt"
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
	feedBase  = "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/"
	staticTTL = 7 * 24 * time.Hour
)

// system is one MTA network with its own static timetable and feeds.
type system struct {
	id        string
	name      string
	staticURL string
	feeds     func(routeID string) []string // feed URLs (relative to feedBase) carrying the route; "" for all
	static    *gtfs.Feed
	http      *httpx.Client
}

var subwaySuffixes = map[string]string{
	"A": "-ace", "C": "-ace", "E": "-ace", "H": "-ace", "FS": "-ace",
	"B": "-bdfm", "D": "-bdfm", "F": "-bdfm", "FX": "-bdfm", "M": "-bdfm",
	"G": "-g", "J": "-jz", "Z": "-jz",
	"N": "-nqrw", "Q": "-nqrw", "R": "-nqrw", "W": "-nqrw",
	"L": "-l", "SI": "-si",
}

var allSubwayFeeds = []string{"nyct%2Fgtfs", "nyct%2Fgtfs-ace", "nyct%2Fgtfs-bdfm", "nyct%2Fgtfs-g", "nyct%2Fgtfs-jz", "nyct%2Fgtfs-nqrw", "nyct%2Fgtfs-l", "nyct%2Fgtfs-si"}

func subwayFeeds(routeID string) []string {
	if routeID == "" {
		return allSubwayFeeds
	}
	return []string{"nyct%2Fgtfs" + subwaySuffixes[routeID]}
}

var systemSpecs = []struct {
	id, name, staticURL string
	feeds               func(string) []string
}{
	{"subway", "New York City Subway", "https://rrgtfsfeeds.s3.amazonaws.com/gtfs_subway.zip", subwayFeeds},
	{"lirr", "Long Island Rail Road", "https://rrgtfsfeeds.s3.amazonaws.com/gtfslirr.zip", func(string) []string { return []string{"lirr%2Fgtfs-lirr"} }},
	{"mnr", "Metro-North Railroad", "https://rrgtfsfeeds.s3.amazonaws.com/gtfsmnr.zip", func(string) []string { return []string{"mnr%2Fgtfs-mnr"} }},
}

var newYork = transit.Info{
	ID:       "newyork",
	Name:     "New York City Subway (MTA)",
	Country:  "usa",
	Provider: "mta",
	TZ:       "America/New_York",
	Realtime: true,
	Notes:    "subway only; first run downloads the static timetable (5 MB); no scheduled fallback",
	Probe:    transit.Probe{Route: "L", Query: "bedford av"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "usa", Name: "United States", Aliases: []string{"us", "united-states", "america"}, Providers: []string{"mta"},
		Coverage: "Boston; New York City, Long Island, the Hudson Valley and Connecticut (any MTA station)", AnyTown: NewTown})
	registry.Register(registry.Entry{Info: newYork, Aliases: []string{"nyc", "mta", "nyc-subway"}, New: New})
}

type Provider struct {
	// FeedBase is the prefix of every real-time feed URL; tests override it.
	FeedBase string
	Systems  []*system

	info  transit.Info
	town  string // set for an unregistered town: stations are matched on the name
	cache *cache.Cache
	now   func() time.Time
	loc   *time.Location

	mu     sync.Mutex
	routes map[string][]transit.Route // per system id
}

// New builds the subway provider for New York City.
func New(d registry.Deps) transit.Provider { return build(d, newYork, "", "subway") }

// NewTown builds a provider for any town with an MTA station: the subway,
// LIRR and Metro-North stop names are searched for it.
func NewTown(d registry.Deps, town string) transit.Provider {
	info := transit.Info{
		ID:       strings.ReplaceAll(match.Normalize(town), " ", "-"),
		Name:     town + " (MTA)",
		Country:  "usa",
		Provider: "mta",
		TZ:       "America/New_York",
		Realtime: true,
		Notes:    "subway, LIRR and Metro-North stations; first run downloads three timetables (13 MB); no scheduled fallback",
	}
	return build(d, info, town, "subway", "lirr", "mnr")
}

func build(d registry.Deps, info transit.Info, town string, systemIDs ...string) *Provider {
	h := httpx.New("mta", d.HTTP)
	// Static zips are shared by every town of the provider, so they live
	// under the provider's own cache directory rather than the town's.
	dir := filepath.Join(cache.Default("mta", staticTTL).Dir, "gtfs")
	p := &Provider{FeedBase: feedBase, info: info, town: town, cache: d.Cache, now: d.Clock(), loc: info.Location(), routes: map[string][]transit.Route{}}
	for _, spec := range systemSpecs {
		for _, id := range systemIDs {
			if spec.id != id {
				continue
			}
			p.Systems = append(p.Systems, &system{
				id: spec.id, name: spec.name, staticURL: spec.staticURL, feeds: spec.feeds, http: h,
				static: &gtfs.Feed{URL: spec.staticURL, Path: filepath.Join(dir, spec.id+".zip"), TTL: staticTTL, HTTP: h, Now: d.Clock()},
			})
		}
	}
	return p
}

func (p *Provider) Info() transit.Info { return p.info }

// Redirect points every system's static zip and feed at base, for tests.
func (p *Provider) Redirect(feedBase, staticBase, dir string) {
	p.FeedBase = feedBase
	for _, s := range p.Systems {
		s.static.URL = staticBase + "/gtfs_" + s.id + ".zip"
		s.static.Path = filepath.Join(dir, s.id+".zip")
	}
}

// Cold reports whether any static timetable still has to be downloaded.
func (p *Provider) Cold() bool {
	for _, s := range p.Systems {
		if s.static.Cold() {
			return true
		}
	}
	return false
}

// Ids are qualified as "<system>:<id>" when the provider spans several
// systems, since stop and route ids repeat between them.
func (p *Provider) qualify(s *system, id string) string {
	if len(p.Systems) == 1 {
		return id
	}
	return s.id + ":" + id
}

func (p *Provider) split(id string) (*system, string) {
	if len(p.Systems) == 1 {
		return p.Systems[0], id
	}
	sys, rest, ok := strings.Cut(id, ":")
	if ok {
		for _, s := range p.Systems {
			if s.id == sys {
				return s, rest
			}
		}
	}
	return p.Systems[0], id
}

func (p *Provider) routesOf(ctx context.Context, s *system) ([]transit.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rs, ok := p.routes[s.id]; ok {
		return rs, nil
	}
	rs, err := s.static.Routes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rs {
		rs[i].ID = p.qualify(s, rs[i].ID)
		rs[i].ShortName = shortName(s, rs[i])
	}
	p.routes[s.id] = rs
	return rs, nil
}

// shortName is the label riders use: subway letters and numbers, "S" for
// the three shuttles, "SIR", and branch or line names for the railroads
// ("Babylon", "New Haven").
func shortName(s *system, r transit.Route) string {
	_, id, _ := strings.Cut(r.ID, ":")
	if s.id == "subway" {
		switch id {
		case "GS", "FS", "H":
			return "S"
		case "SI":
			return "SIR"
		}
		return strings.ToUpper(r.ShortName)
	}
	name := r.LongName
	if name == "" {
		name = r.ShortName
	}
	for _, suf := range []string{" Branch", " Line"} {
		name = strings.TrimSuffix(name, suf)
	}
	return name
}

// FindRoutes matches the public name across the provider's systems: exact
// on the short name (or on the long name for the railroads), then a unique
// substring match ("port jeff" → Port Jefferson Branch).
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	want := match.Normalize(short)
	var exact, partial []transit.Route
	var others []string
	for _, s := range p.Systems {
		rs, err := p.routesOf(ctx, s)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			_, id, _ := strings.Cut(r.ID, ":")
			n := match.Normalize(r.ShortName)
			switch {
			case n == want || match.Normalize(id) == want && s.id == "subway":
				exact = append(exact, r)
			case want != "" && strings.Contains(n, want):
				partial = append(partial, r)
			default:
				others = append(others, r.ShortName)
			}
		}
	}
	switch {
	case len(exact) > 0:
		return exact, nil
	case len(partial) == 1:
		return partial, nil
	case len(partial) > 1:
		for _, r := range partial {
			others = append([]string{r.ShortName}, others...)
		}
	}
	return nil, transit.UnknownRoute(short, others)
}

// RouteStops derives the patterns from stop_times.txt, which is slow, so the
// result is cached for a day. Subway platforms are reported as their parent
// station, which Departures expands back.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	key := "patterns-" + r.ID
	var ps []transit.Pattern
	if p.cache.Load(key, &ps) && len(ps) > 0 {
		return ps, nil
	}
	s, id := p.split(r.ID)
	ps, err := s.static.Patterns(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		for j, st := range ps[i].Stops {
			if st.ParentID != "" {
				st = transit.Stop{ID: st.ParentID, Name: st.Name, Lat: st.Lat, Lon: st.Lon}
			}
			st.ID = p.qualify(s, st.ID)
			ps[i].Stops[j] = st
		}
	}
	_ = p.cache.Store(key, ps)
	return ps, nil
}

// SearchStops matches station names across the systems; for an
// unregistered town only stations whose name carries the town count.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	var all []match.Stop
	byID := map[string]transit.Stop{}
	town := match.Normalize(p.town)
	for _, s := range p.Systems {
		stops, err := s.static.Stops(ctx)
		if err != nil {
			return nil, err
		}
		for _, st := range stops {
			if st.ParentID != "" {
				continue // platforms; the station is what riders name
			}
			if town != "" && !strings.Contains(match.Normalize(st.Name), town) {
				continue
			}
			st.ID = p.qualify(s, st.ID)
			all = append(all, match.Stop{ID: st.ID, Name: st.Name})
			byID[st.ID] = st
		}
	}
	if len(all) == 0 && town != "" {
		return nil, fmt.Errorf("mta: no station named %q on the subway, LIRR or Metro-North", p.town)
	}
	cands := match.Find(query, all)
	if len(cands) == 0 {
		cands = match.Closest(query, all, 8)
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
// requested routes live in (or every feed of the stations' systems) and
// labels each update with the trip headsign or, failing that, its last
// stop. The feeds carry no scheduled times, so every entry is Live.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now()}
	if len(stopIDs) == 0 {
		return out, nil
	}
	// Group the requested stops by system.
	bySys := map[*system]map[string]bool{}
	for _, id := range stopIDs {
		s, raw := p.split(id)
		if bySys[s] == nil {
			bySys[s] = map[string]bool{}
		}
		bySys[s][raw] = true
	}
	routeIDs := map[*system][]string{}
	for _, r := range routes {
		s, raw := p.split(r.ID)
		routeIDs[s] = append(routeIDs[s], raw)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var errs []error
	var fetched int
	var newest time.Time
	for s, want := range bySys {
		stops, err := s.static.Stops(ctx)
		if err != nil {
			return nil, err
		}
		names := make(map[string]string, len(stops))
		platforms := map[string]bool{}
		for _, st := range stops {
			names[st.ID] = st.Name
			if want[st.ID] || want[st.ParentID] {
				platforms[st.ID] = true
			}
		}
		trips, _ := s.static.Trips(ctx)
		lines, _ := p.routesOf(ctx, s)
		lineName := map[string]string{}
		for _, r := range lines {
			_, raw, _ := strings.Cut(r.ID, ":")
			lineName[raw] = r.ShortName
		}
		feeds := map[string]bool{}
		if len(routeIDs[s]) == 0 {
			for _, f := range s.feeds("") {
				feeds[f] = true
			}
		} else {
			for _, rid := range routeIDs[s] {
				for _, f := range s.feeds(rid) {
					feeds[f] = true
				}
			}
		}
		for f := range feeds {
			wg.Add(1)
			fetched++
			go func(s *system, f string) {
				defer wg.Done()
				msg, err := gtfsrt.Fetch(ctx, s.http, p.FeedBase+f)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				ts := gtfsrt.Timestamp(msg)
				if ts.After(newest) {
					newest = ts
				}
				if ts.IsZero() {
					ts = p.now()
				}
				// The railroad feeds carry a running train's passed stops
				// with their actual times; only what is still to come is a
				// departure.
				for _, u := range gtfsrt.StopUpdates(msg, platforms) {
					if u.At.Before(ts.Add(-time.Minute)) || (window > 0 && u.At.After(ts.Add(window))) {
						continue
					}
					if u.StopID == u.LastStopID {
						continue // the trip terminates here: an arrival, not a departure
					}
					out.Departures = append(out.Departures, p.toDeparture(s, u, names, trips, lineName))
				}
			}(s, f)
		}
	}
	wg.Wait()
	if len(errs) > 0 && len(errs) == fetched {
		return nil, errs[0]
	}
	if !newest.IsZero() {
		out.Now = newest.In(p.loc)
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

func (p *Provider) toDeparture(s *system, u gtfsrt.Update, names map[string]string, trips map[string]gtfs.Trip, lineName map[string]string) transit.Departure {
	d := transit.Departure{
		At:          u.At.In(p.loc),
		Live:        true,
		Cancelled:   u.Cancelled,
		Line:        lineName[u.RouteID],
		RouteID:     p.qualify(s, u.RouteID),
		TripID:      p.qualify(s, u.TripID),
		StopID:      p.qualify(s, u.StopID),
		DirectionID: u.DirectionID,
		Headsign:    names[u.LastStopID],
	}
	if d.Line == "" {
		d.Line = u.RouteID
	}
	if s.id == "subway" {
		// Platform ids end in N or S: the feed's own direction flag.
		if n := len(u.StopID); n > 0 && (u.StopID[n-1] == 'N' || u.StopID[n-1] == 'S') {
			d.DirectionID = string(u.StopID[n-1])
		}
	}
	if t, ok := trips[u.TripID]; ok {
		if t.Headsign != "" {
			d.Headsign = t.Headsign
		}
		if d.DirectionID == "" {
			d.DirectionID = t.DirectionID
		}
	}
	if d.Headsign == "" {
		d.Headsign = map[string]string{"N": "Uptown", "S": "Downtown"}[d.DirectionID]
	}
	return d
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
