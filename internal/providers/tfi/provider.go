// Package tfi is the Transport for Ireland provider: every bus, Luas, DART
// and Irish Rail departure in the national GTFS feed, with realtime from
// the NTA's GTFS-Realtime API (free key). The realtime feed reports delays
// against the schedule, so departures are the static timetable adjusted by
// the latest delay; a stop without any update yet shows scheduled times.
// Stops carry no town, so a town scopes the search geographically: the
// five cities are built in, any other town is located with Nominatim.
package tfi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/geocode"
	"github.com/bancsdan/eta/internal/gtfs"
	"github.com/bancsdan/eta/internal/gtfsrt"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const (
	staticURL     = "https://www.transportforireland.ie/transitData/Data/GTFS_Realtime.zip"
	feedURL       = "https://api.nationaltransport.ie/gtfsr/v2/TripUpdates"
	keyEnv        = "ETA_TFI_API_KEY"
	signupURL     = "https://developer.nationaltransport.ie"
	staticTTL     = 7 * 24 * time.Hour
	defaultWindow = time.Hour
	notes         = "buses, Luas, DART and Irish Rail; first run downloads the national timetable (144 MB) and indexes it"
)

type city struct {
	info    transit.Info
	aliases []string
	place   geocode.Place
}

func info(id, name string, probe transit.Probe) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name + " (TFI)",
		Country:  "ireland",
		Provider: "tfi",
		TZ:       "Europe/Dublin",
		Keys:     []transit.KeySpec{{Env: keyEnv, Req: transit.KeyRequired, SignupURL: signupURL}},
		Realtime: true,
		Notes:    notes,
		Probe:    probe,
	}
}

var cities = []city{
	{info("dublin", "Dublin", transit.Probe{Route: "1", Query: "o'connell st lwr"}), []string{"bac"}, geocode.Place{Name: "Dublin", Lat: 53.3494, Lon: -6.2606, RadiusKm: 18}},
	{info("cork", "Cork", transit.Probe{Route: "220", Query: "patrick st"}), []string{"corcaigh"}, geocode.Place{Name: "Cork", Lat: 51.8985, Lon: -8.4726, RadiusKm: 12}},
	{info("galway", "Galway", transit.Probe{Route: "401", Query: "eyre square"}), []string{"gaillimh"}, geocode.Place{Name: "Galway", Lat: 53.2744, Lon: -9.0491, RadiusKm: 10}},
	{info("limerick", "Limerick", transit.Probe{Route: "304", Query: "william st"}), []string{"luimneach"}, geocode.Place{Name: "Limerick", Lat: 52.6638, Lon: -8.6267, RadiusKm: 10}},
	{info("waterford", "Waterford", transit.Probe{Route: "W1", Query: "clock tower"}), []string{"portlairge"}, geocode.Place{Name: "Waterford", Lat: 52.2593, Lon: -7.1101, RadiusKm: 8}},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "ireland", Name: "Ireland", Aliases: []string{"ie", "irl", "eire"}, Providers: []string{"tfi"}, AnyTown: NewTown})
	for _, c := range cities {
		c := c
		registry.Register(registry.Entry{Info: c.info, Aliases: c.aliases, New: func(d registry.Deps) transit.Provider { return build(d, c.info, c.info.Name, &c.place) }})
	}
}

type Provider struct {
	// FeedURL is the realtime endpoint; Geocoder locates unregistered
	// towns. Tests point both at a local server.
	FeedURL  string
	Geocoder *geocode.Nominatim

	info  transit.Info
	town  string
	place *geocode.Place
	feed  *gtfs.Feed
	index *gtfs.Index
	live  *httpx.Client
	key   string
	now   func() time.Time
	loc   *time.Location

	mu       sync.Mutex
	routes   []transit.Route
	patterns map[string][]transit.Pattern
	scoped   []transit.Route
}

// NewTown builds a provider for any Irish town, located on first use.
func NewTown(d registry.Deps, town string) transit.Provider {
	i := info(strings.ReplaceAll(match.Normalize(town), " ", "-"), town, transit.Probe{})
	return build(d, i, town, nil)
}

func build(d registry.Deps, info transit.Info, town string, place *geocode.Place) *Provider {
	h := httpx.New("tfi", d.HTTP)
	live := httpx.New("tfi", d.HTTP)
	key := d.KeyFor(keyEnv)
	if key != "" {
		live.Header.Set("x-api-key", key)
		live.Redact = append(live.Redact, key)
	}
	path := filepath.Join(cache.Default("tfi", staticTTL).Dir, "gtfs", "tfi.zip")
	feed := &gtfs.Feed{URL: staticURL, Path: path, TTL: staticTTL, HTTP: h, Progress: os.Stderr, Now: d.Clock()}
	return &Provider{
		FeedURL:  feedURL,
		Geocoder: &geocode.Nominatim{HTTP: httpx.New("nominatim", d.HTTP)},
		info:     info,
		town:     town,
		place:    place,
		feed:     feed,
		index:    &gtfs.Index{Feed: feed, Progress: os.Stderr},
		live:     live,
		key:      key,
		now:      d.Clock(),
		loc:      info.Location(),
	}
}

func (p *Provider) Info() transit.Info { return p.info }

// Redirect points the static zip, the realtime feed and the geocoder at
// base and keeps the zip under dir, for tests.
func (p *Provider) Redirect(base, dir string) {
	p.feed.URL = base + "/gtfs.zip"
	p.feed.Path = filepath.Join(dir, "tfi.zip")
	p.feed.Progress, p.index.Progress = nil, nil
	p.FeedURL = base + "/TripUpdates"
	p.Geocoder.BaseURL = base
}

// Cold reports whether the timetable must be downloaded or indexed.
func (p *Provider) Cold() bool { return p.index.Cold() }

func (p *Provider) area(ctx context.Context) (*geocode.Place, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.place != nil {
		return p.place, nil
	}
	pl, err := p.Geocoder.Locate(ctx, p.town, "ie")
	if err != nil {
		return nil, err
	}
	p.place = &pl
	return p.place, nil
}

func (p *Provider) routesOf(ctx context.Context) ([]transit.Route, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes != nil {
		return p.routes, nil
	}
	rs, err := p.feed.Routes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rs {
		// Irish Rail labels most lines just "rail"; the long name
		// ("Dublin - Sligo") is what tells them apart.
		if strings.EqualFold(rs[i].ShortName, "rail") && rs[i].LongName != "" {
			rs[i].ShortName = rs[i].LongName
		}
	}
	p.routes = rs
	return rs, nil
}

func (p *Provider) patternsOf(ctx context.Context) (map[string][]transit.Pattern, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.patterns != nil {
		return p.patterns, nil
	}
	ps, err := p.index.Patterns(ctx)
	if err != nil {
		return nil, err
	}
	p.patterns = ps
	return ps, nil
}

// scopedRoutes are the routes with at least one stop in the town.
func (p *Provider) scopedRoutes(ctx context.Context) ([]transit.Route, error) {
	rs, err := p.routesOf(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := p.patternsOf(ctx)
	if err != nil {
		return nil, err
	}
	pl, err := p.area(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scoped != nil {
		return p.scoped, nil
	}
	out := []transit.Route{}
	for _, r := range rs {
		if touches(ps[r.ID], pl) {
			out = append(out, r)
		}
	}
	p.scoped = out
	return out, nil
}

func touches(ps []transit.Pattern, pl *geocode.Place) bool {
	for _, pat := range ps {
		for _, s := range pat.Stops {
			if pl.Contains(s.Lat, s.Lon) {
				return true
			}
		}
	}
	return false
}

// FindRoutes matches the public route name among the routes serving the
// town: exact first, then a unique partial match on the short or long name
// ("sligo" → Dublin - Sligo, "howth" → DART).
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	rs, err := p.scopedRoutes(ctx)
	if err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var exact, partial []transit.Route
	var names []string
	for _, r := range rs {
		n := match.Normalize(r.ShortName)
		switch {
		case n == want:
			exact = append(exact, r)
		case wordy(want) && (strings.Contains(n, want) || strings.Contains(match.Normalize(r.LongName), want)):
			partial = append(partial, r)
		}
		names = append(names, r.ShortName)
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(partial) == 1 {
		return partial, nil
	}
	sort.Strings(names)
	names = dedupe(names)
	if len(rs) == 0 {
		return nil, fmt.Errorf("tfi: no route serves %s: %w", p.town, transit.ErrNotFound)
	}
	return nil, transit.UnknownRoute(short, names)
}

// wordy reports whether a query is a name rather than a route number, so
// "sligo" may match "Dublin - Sligo" but "1" never matches "401".
func wordy(q string) bool {
	if len(q) < 3 {
		return false
	}
	for _, r := range q {
		if r >= 'a' && r <= 'z' {
			return true
		}
	}
	return false
}

func dedupe(xs []string) []string {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || xs[i-1] != x {
			out = append(out, x)
		}
	}
	return out
}

// RouteStops returns the full route, not just its stops in the town.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	ps, err := p.patternsOf(ctx)
	if err != nil {
		return nil, err
	}
	if len(ps[r.ID]) == 0 {
		return nil, fmt.Errorf("tfi: route %s: %w", r.ShortName, transit.ErrNotFound)
	}
	return ps[r.ID], nil
}

// SearchStops matches names among the stops within the town.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	stops, err := p.feed.Stops(ctx)
	if err != nil {
		return nil, err
	}
	pl, err := p.area(ctx)
	if err != nil {
		return nil, err
	}
	var all []match.Stop
	byID := map[string]transit.Stop{}
	for _, s := range stops {
		if !pl.Contains(s.Lat, s.Lon) {
			continue
		}
		all = append(all, match.Stop{ID: s.ID, Name: s.Name})
		byID[s.ID] = s
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("tfi: no stops within %.0f km of %s", pl.RadiusKm, pl.Name)
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

type call struct {
	stopID string
	trip   string
	date   string
	seq    int
	at     time.Time
}

// Departures lists the timetable's calls at the stops for the window,
// then applies the realtime feed's delays and cancellations.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now().In(p.loc)}
	if len(stopIDs) == 0 {
		return out, nil
	}
	if p.key == "" {
		return nil, transit.ErrNoKey
	}
	if window <= 0 {
		window = defaultWindow
	}
	calls, err := p.scheduled(ctx, stopIDs, routes, window)
	if err != nil {
		return nil, err
	}
	msg, err := gtfsrt.Fetch(ctx, p.live, p.FeedURL)
	if err != nil {
		return nil, err
	}
	if ts := gtfsrt.Timestamp(msg); !ts.IsZero() {
		out.Now = ts.In(p.loc)
	}
	updates := gtfsrt.TripUpdates(msg)
	trips, _ := p.feed.Trips(ctx)
	rs, _ := p.routesOf(ctx)
	lineName := make(map[string]string, len(rs))
	for _, r := range rs {
		lineName[r.ID] = r.ShortName
	}
	cutoff := p.now().Add(-time.Minute)
	for _, c := range calls {
		t := trips[c.trip]
		d := transit.Departure{
			At:          c.at,
			Scheduled:   c.at,
			Headsign:    t.Headsign,
			DirectionID: t.DirectionID,
			Line:        lineName[t.RouteID],
			RouteID:     t.RouteID,
			TripID:      c.trip + "@" + c.date,
			StopID:      c.stopID,
		}
		if d.Line == "" {
			d.Line = t.RouteID
		}
		if u := updates[c.trip]; u != nil && (u.StartDate == "" || u.StartDate == c.date) {
			if u.Cancelled {
				d.Live, d.Cancelled = true, true
			} else if at, ok, skipped := u.Predict(c.seq, c.stopID, c.at); ok {
				d.Live, d.Cancelled = true, skipped
				d.At = at.In(p.loc)
			}
		}
		if d.At.Before(cutoff) {
			continue
		}
		out.Departures = append(out.Departures, d)
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

// scheduled reads the timetable's calls at the stops within the window,
// on today's and yesterday's service days (trips past midnight belong to
// the day they started).
func (p *Provider) scheduled(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) ([]call, error) {
	cal, err := p.feed.Calendar(ctx)
	if err != nil {
		return nil, err
	}
	trips, err := p.feed.Trips(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, r := range routes {
		want[r.ID] = true
	}
	now := p.now().In(p.loc)
	from, to := now.Add(-time.Minute), now.Add(window)
	type day struct {
		start  time.Time
		date   string
		active map[string]bool
	}
	var days []day
	for _, d := range []time.Time{now.AddDate(0, 0, -1), now} {
		start := gtfs.ServiceDay(d, p.loc)
		days = append(days, day{start, start.Format("20060102"), cal.Active(start)})
	}
	var out []call
	for _, id := range stopIDs {
		sts, err := p.index.StopTimes(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			t, ok := trips[st.TripID]
			if !ok || (len(want) > 0 && !want[t.RouteID]) {
				continue
			}
			for _, d := range days {
				if !d.active[t.ServiceID] {
					continue
				}
				at := d.start.Add(time.Duration(st.Departure) * time.Second)
				if at.Before(from) || at.After(to) {
					continue
				}
				out = append(out, call{stopID: id, trip: st.TripID, date: d.date, seq: st.Seq, at: at})
			}
		}
	}
	return out, nil
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
