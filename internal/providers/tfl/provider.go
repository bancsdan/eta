// Package tfl is the Transport for London provider, backed by the TfL
// Unified API (https://api.tfl.gov.uk): tube, bus, DLR, Overground, the
// Elizabeth line and trams.
package tfl

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const DefaultBaseURL = "https://api.tfl.gov.uk"

const searchModes = "tube,bus,dlr,overground,elizabeth-line,tram"

var directions = []string{"inbound", "outbound"}

var info = transit.Info{
	ID:       "london",
	Provider: "tfl",
	Country:  "United Kingdom",
	Name:     "London (TfL)",
	TZ:       "Europe/London",
	Keys: []transit.KeySpec{{
		Env:       "ETA_TFL_API_KEY",
		Req:       transit.KeyOptional,
		SignupURL: "https://api-portal.tfl.gov.uk",
	}},
	Realtime: true,
	Notes:    "tube, bus, DLR, Overground, Elizabeth line, tram",
	Probe:    transit.Probe{Route: "Central", Query: "bank"},
}

func init() {
	registry.Register(registry.Entry{Info: info, Aliases: []string{"tfl"}, New: New})
}

type Provider struct {
	BaseURL string

	http  *httpx.Client
	cache *cache.Cache
	now   func() time.Time
}

// Without a key TfL allows 50 requests a minute per IP.
func New(d registry.Deps) transit.Provider {
	c := httpx.New("tfl", d.HTTP)
	if k := d.KeyFor(info.Keys[0].Env); k != "" {
		c.Query.Set("app_key", k)
		c.Redact = append(c.Redact, k)
	}
	return &Provider{BaseURL: DefaultBaseURL, http: c, cache: d.Cache, now: d.Clock()}
}

func (p *Provider) Info() transit.Info { return info }

func (p *Provider) url(path, query string) string {
	u := httpx.Join(p.BaseURL, path)
	if query != "" {
		u += "?" + query
	}
	return u
}

// TfL's line search is fuzzy (it answers "central" with both the Central line
// and Grand Central), so only exact name matches are kept.
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	var res lineSearchResponse
	u := p.url("Line/Search/"+url.PathEscape(strings.TrimSpace(short)), "")
	if err := p.http.GetJSON(ctx, u, &res); err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var out []transit.Route
	var suggestions []string
	seen := map[string]bool{}
	for _, m := range res.SearchMatches {
		if m.LineID == "" || m.LineName == "" {
			continue
		}
		if match.Normalize(m.LineName) == want {
			if seen[m.LineID] {
				continue
			}
			seen[m.LineID] = true
			out = append(out, transit.Route{ID: m.LineID, ShortName: m.LineName, Mode: m.mode()})
			continue
		}
		suggestions = append(suggestions, m.LineName)
	}
	if len(out) == 0 {
		return nil, transit.UnknownRoute(short, suggestions)
	}
	return out, nil
}

// TfL splits a line into branches (stopPointSequences) and itineraries across
// those branches (orderedLineRoutes); the itineraries are what riders
// recognise, so they win when present.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	id := r.ID
	if id == "" {
		id = r.ShortName
	}
	var out []transit.Pattern
	var firstErr error
	for _, dir := range directions {
		var seq routeSequence
		u := p.url("Line/"+url.PathEscape(id)+"/Route/Sequence/"+dir, "excludeCrowding=true")
		if err := p.http.GetJSON(ctx, u, &seq); err != nil {
			// One-directional lines 404 (or error) on the other
			// direction; only fail when neither direction answers.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, patterns(dir, seq)...)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func patterns(dir string, seq routeSequence) []transit.Pattern {
	table := map[string]transit.Stop{}
	for _, s := range seq.StopPointSequences {
		for _, sp := range s.StopPoint {
			if sp.ID == "" {
				continue
			}
			table[sp.ID] = toStop(sp)
		}
	}
	var out []transit.Pattern
	for _, olr := range seq.OrderedLineRoutes {
		stops := make([]transit.Stop, 0, len(olr.NaptanIDs))
		for _, id := range olr.NaptanIDs {
			if s, ok := table[id]; ok {
				stops = append(stops, s)
			}
		}
		if len(stops) == 0 {
			continue
		}
		out = append(out, transit.Pattern{
			DirectionID: dir,
			Headsign:    headsign(olr.Name, stops),
			Stops:       stops,
		})
	}
	if len(out) > 0 {
		return out
	}
	// No itineraries: fall back to the raw branches.
	for _, s := range seq.StopPointSequences {
		stops := make([]transit.Stop, 0, len(s.StopPoint))
		for _, sp := range s.StopPoint {
			if sp.ID == "" {
				continue
			}
			stops = append(stops, toStop(sp))
		}
		if len(stops) == 0 {
			continue
		}
		out = append(out, transit.Pattern{
			DirectionID: dir,
			Headsign:    headsign("", stops),
			Stops:       stops,
		})
	}
	return out
}

// Names keep their " Underground Station" style suffixes: matching is
// substring based and riders type either form.
func toStop(sp matchedStop) transit.Stop {
	parent := sp.ParentID
	if parent == "" && sp.TopMostParentID != sp.ID {
		parent = sp.TopMostParentID
	}
	return transit.Stop{ID: sp.ID, Name: sp.Name, ParentID: parent, Lat: sp.Lat, Lon: sp.Lon}
}

func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	var res stopSearchResponse
	u := p.url("StopPoint/Search/"+url.PathEscape(strings.TrimSpace(query)), "modes="+url.QueryEscape(searchModes))
	if err := p.http.GetJSON(ctx, u, &res); err != nil {
		return nil, err
	}
	var out []transit.Stop
	seen := map[string]bool{}
	add := func(s transit.Stop) {
		if s.ID == "" || s.Name == "" || seen[s.ID] {
			return
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	for _, m := range res.Matches {
		if m.ID == "" {
			continue
		}
		if isHub(m.ID) {
			children, err := p.expandHub(ctx, m.ID)
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				add(c)
			}
			continue
		}
		parent := m.TopMostParentID
		if parent == m.ID {
			parent = ""
		}
		add(transit.Stop{ID: m.ID, Name: m.Name, ParentID: parent, Lat: m.Lat, Lon: m.Lon})
	}
	return out, nil
}

func isHub(id string) bool { return strings.HasPrefix(id, "HUB") }

// Cached for the cache's TTL: hub membership changes about once a decade.
func (p *Provider) expandHub(ctx context.Context, hubID string) ([]transit.Stop, error) {
	key := "hub-" + hubID
	var cached []transit.Stop
	if p.cache.Load(key, &cached) && len(cached) > 0 {
		return cached, nil
	}
	var sp stopPoint
	if err := p.http.GetJSON(ctx, p.url("StopPoint/"+url.PathEscape(hubID), ""), &sp); err != nil {
		return nil, err
	}
	type ranked struct {
		stop transit.Stop
		rank int
	}
	var found []ranked
	seen := map[string]bool{}
	var walk func(children []stopPoint, depth int)
	walk = func(children []stopPoint, depth int) {
		if depth > 3 {
			return
		}
		for _, c := range children {
			id := c.naptan()
			if id == "" {
				continue
			}
			if rank, ok := departureRank(c.StopType); ok {
				if seen[id] || c.CommonName == "" {
					continue
				}
				seen[id] = true
				found = append(found, ranked{transit.Stop{
					ID: id, Name: c.CommonName, ParentID: hubID, Lat: c.Lat, Lon: c.Lon,
				}, rank})
				continue
			}
			// Stop areas, pairs and clusters hold the real stops one
			// level down, and TfL returns no arrivals for them.
			walk(c.Children, depth+1)
		}
	}
	walk(sp.Children, 0)
	// Stations before the bus stops named after them: a hub can hold a
	// dozen kerbside stops and only the callers' first few candidates get
	// probed.
	sort.SliceStable(found, func(i, j int) bool { return found[i].rank < found[j].rank })
	out := make([]transit.Stop, 0, len(found))
	for _, f := range found {
		out = append(out, f.stop)
	}
	if len(out) > 0 {
		_ = p.cache.Store(key, out)
	}
	return out, nil
}

// 0 = station, 1 = kerbside stop; anything else gets no /Arrivals from TfL.
func departureRank(stopType string) (int, bool) {
	switch stopType {
	case "NaptanMetroStation", "NaptanRailStation", "NaptanBusCoachStation", "NaptanFerryPort":
		return 0, true
	case "NaptanPublicBusCoachTram", "NaptanFerryAccess":
		return 1, true
	}
	return 0, false
}

// A station id (940GZZLU…, 910G…) yields all of its platforms; a bus stop id
// (490…) just that stop. TfL publishes no timetable time and no cancellations
// here, so every entry is Live with a zero Scheduled.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now()
	var cutoff time.Time
	if window > 0 {
		cutoff = now.Add(window)
	}
	deps, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, id string) ([]transit.Departure, error) {
		var arrivals []arrival
		u := p.url("StopPoint/"+url.PathEscape(id)+"/Arrivals", "")
		if err := p.http.GetJSON(ctx, u, &arrivals); err != nil {
			return nil, err
		}
		out := make([]transit.Departure, 0, len(arrivals))
		for _, a := range arrivals {
			d := toDeparture(a, now)
			if d.At.IsZero() || (!cutoff.IsZero() && d.At.After(cutoff)) {
				continue
			}
			out = append(out, d)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return &transit.Departures{Now: now, Departures: deps}, nil
}

func toDeparture(a arrival, now time.Time) transit.Departure {
	at := xutil.ParseRFC3339(a.ExpectedArrival)
	if at.IsZero() {
		at = now.Add(time.Duration(a.TimeToStation) * time.Second)
	}
	head := stripStationSuffix(a.DestinationName)
	if head == "" {
		head = strings.TrimSpace(a.Towards)
	}
	stop := a.NaptanID
	if stop == "" {
		stop = a.ID
	}
	return transit.Departure{
		At:          at,
		Live:        true,
		Platform:    a.PlatformName,
		Headsign:    head,
		DirectionID: a.Direction,
		Line:        a.LineName,
		RouteID:     a.LineID,
		// vehicleId alone repeats across platforms on the tube (trains
		// are numbered per line), so the trip key needs the line and
		// the arrival time as well.
		TripID: a.VehicleID + "|" + a.LineID + "|" + a.ExpectedArrival,
		StopID: stop,
	}
}

// Kept in stop names (riders search for either form), stripped from headsigns.
var stationSuffixes = []string{
	" Underground Station",
	" Overground Station",
	" Elizabeth line Station",
	" DLR Station",
	" Rail Station",
	" Tram Stop",
	" Bus Station",
	" Station",
}

func stripStationSuffix(s string) string {
	s = strings.TrimSpace(s)
	for _, suf := range stationSuffixes {
		if len(s) > len(suf) && strings.EqualFold(s[len(s)-len(suf):], suf) {
			return strings.TrimSpace(s[:len(s)-len(suf)])
		}
	}
	return s
}

// TfL writes the arrow as the HTML entity "&harr;".
var routeNameSeparators = []string{"&harr;", "↔", "→", "&rarr;", " to "}

func lastIndexFold(s, sub string) int {
	lower := strings.ToLower(s)
	if len(lower) != len(s) {
		return strings.LastIndex(s, sub)
	}
	return strings.LastIndex(lower, strings.ToLower(sub))
}

// "Epping &harr; West Ruislip via Newbury Park" → "West Ruislip via Newbury
// Park"; the last stop is the fallback.
func headsign(routeName string, stops []transit.Stop) string {
	name := strings.TrimSpace(routeName)
	for _, sep := range routeNameSeparators {
		if i := lastIndexFold(name, sep); i >= 0 {
			name = name[i+len(sep):]
			break
		}
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" && len(stops) > 0 {
		name = stops[len(stops)-1].Name
	}
	return stripStationSuffix(name)
}

var (
	_ transit.Provider     = (*Provider)(nil)
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
)
