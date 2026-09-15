// Package mbta is the MBTA v3 API provider: subway, light rail, bus,
// commuter rail and ferry departures for Greater Boston.
package mbta

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

// Requests work without a key at roughly 20/minute; the key only raises
// that ceiling.
const DefaultBaseURL = "https://api-v3.mbta.com"

const keyEnv = "ETA_MBTA_API_KEY"

const routesCacheKey = "routes-all"

const stationsCacheKey = "stations-all"

var info = transit.Info{
	ID:       "boston",
	Provider: "mbta",
	Country:  "usa",
	Name:     "Boston (MBTA)",
	TZ:       "America/New_York",
	Keys: []transit.KeySpec{{
		Env:       keyEnv,
		Req:       transit.KeyOptional,
		SignupURL: "https://api-v3.mbta.com/register",
	}},
	Realtime: true,
	Notes:    "stop search without a route covers stations only (bus stops need a route); schedule-only departures stop at 23:59 when the window crosses midnight",
	Probe:    transit.Probe{Route: "Red", Query: "park street"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "usa", Name: "United States", Aliases: []string{"us", "united-states", "america"}, Providers: []string{"mbta"}})
	registry.Register(registry.Entry{Info: info, Aliases: []string{"mbta"}, New: New})
}

type Provider struct {
	BaseURL string

	http  *httpx.Client
	cache *cache.Cache
	now   func() time.Time
	loc   *time.Location

	mu     sync.Mutex
	routes *routeIndex // process-lifetime memo on top of the disk cache
}

func New(d registry.Deps) transit.Provider {
	c := httpx.New("mbta", d.HTTP)
	if k := d.KeyFor(keyEnv); k != "" {
		c.Header.Set("x-api-key", k)
		c.Redact = append(c.Redact, k)
	}
	return &Provider{BaseURL: DefaultBaseURL, http: c, cache: d.Cache, now: d.Clock(), loc: info.Location()}
}

func (p *Provider) Info() transit.Info { return info }

type routeIndex struct {
	Routes []indexedRoute `json:"routes"`
}

// Destinations is indexed by direction_id and supplies pattern headsigns.
type indexedRoute struct {
	ID           string   `json:"id"`
	ShortName    string   `json:"shortName"`
	LongName     string   `json:"longName"`
	Mode         string   `json:"mode"`
	Destinations []string `json:"destinations"`
}

func (r indexedRoute) route() transit.Route {
	return transit.Route{ID: r.ID, ShortName: r.ShortName, LongName: r.LongName, Mode: r.Mode}
}

func (ix *routeIndex) shortName(id string) string {
	if ix != nil {
		for _, r := range ix.Routes {
			if r.ID == id {
				return r.ShortName
			}
		}
	}
	return id
}

func (ix *routeIndex) destination(id string, dir int) string {
	if ix == nil {
		return ""
	}
	for _, r := range ix.Routes {
		if r.ID == id && dir >= 0 && dir < len(r.Destinations) {
			return r.Destinations[dir]
		}
	}
	return ""
}

func (p *Provider) loadRoutes(ctx context.Context) (*routeIndex, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes != nil {
		return p.routes, nil
	}
	var ix routeIndex
	if p.cache.Load(routesCacheKey, &ix) && len(ix.Routes) > 0 {
		p.routes = &ix
		return p.routes, nil
	}
	var resp routesResp
	u := p.url("/routes",
		"filter[type]", "0,1,2,3,4",
		"fields[route]", "short_name,long_name,type,direction_names,direction_destinations")
	if err := p.http.GetJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	built := routeIndex{Routes: make([]indexedRoute, 0, len(resp.Data))}
	for _, r := range resp.Data {
		short := r.Attributes.ShortName
		if short == "" {
			short = r.ID
		}
		built.Routes = append(built.Routes, indexedRoute{
			ID:           r.ID,
			ShortName:    short,
			LongName:     r.Attributes.LongName,
			Mode:         mode(r.Attributes.Type),
			Destinations: r.Attributes.DirectionDestinations,
		})
	}
	_ = p.cache.Store(routesCacheKey, built)
	p.routes = &built
	return p.routes, nil
}

func mode(t *int) string {
	if t == nil {
		return ""
	}
	return transit.ModeFromGTFS(*t)
}

// MBTA route ids and short names live in different spaces per mode, so
// lookup tries three tiers and returns every hit of the first one that
// matches: the public short name (bus "1", "SL1", Green Line branch "E"),
// then the route id ("Red", "Green-E", "CR-Fitchburg"), then the long name
// with or without a trailing " Line" ("Red Line" and "red" both find Red).
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	ix, err := p.loadRoutes(ctx)
	if err != nil {
		return nil, err
	}
	q := match.Normalize(short)
	if q == "" {
		return nil, &transit.UnknownRouteError{Route: short}
	}
	var byShort, byID, byLong []transit.Route
	for _, r := range ix.Routes {
		switch {
		case match.Normalize(r.ShortName) == q:
			byShort = append(byShort, r.route())
		case match.Normalize(r.ID) == q:
			byID = append(byID, r.route())
		case match.Normalize(r.LongName) == q,
			r.LongName != "" && match.Normalize(strings.TrimSuffix(r.LongName, " Line")) == q:
			byLong = append(byLong, r.route())
		}
	}
	for _, tier := range [][]transit.Route{byShort, byID, byLong} {
		if len(tier) > 0 {
			return tier, nil
		}
	}
	return nil, transit.UnknownRoute(short, p.suggest(ix, short))
}

func (p *Provider) suggest(ix *routeIndex, short string) []string {
	names := make([]match.Stop, 0, len(ix.Routes))
	for _, r := range ix.Routes {
		names = append(names, match.Stop{ID: r.ID, Name: r.ShortName})
	}
	cands := match.Closest(short, names, 8)
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Name)
	}
	return out
}

// MBTA has no name search, but the station list is small enough to fetch
// once a day and match locally. Bus stops are not covered: there are
// thousands, and with a route the RouteLister path finds them anyway.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	stations, err := p.loadStations(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]match.Stop, 0, len(stations))
	byID := make(map[string]transit.Stop, len(stations))
	for _, s := range stations {
		all = append(all, match.Stop{ID: s.ID, Name: s.Name})
		byID[s.ID] = s
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

func (p *Provider) loadStations(ctx context.Context) ([]transit.Stop, error) {
	var stations []transit.Stop
	if p.cache.Load(stationsCacheKey, &stations) && len(stations) > 0 {
		return stations, nil
	}
	var resp stopsResp
	u := p.url("/stops", "filter[location_type]", "1", "fields[stop]", "name,municipality", "page[limit]", "1000")
	if err := p.http.GetJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	for _, s := range resp.Data {
		if s.ID == "" || s.Attributes.Name == "" {
			continue
		}
		stations = append(stations, transit.Stop{ID: s.ID, Name: s.Attributes.Name})
	}
	if len(stations) > 0 {
		_ = p.cache.Store(stationsCacheKey, stations)
	}
	return stations, nil
}

// The feed returns parent stations for rail modes ("place-pktrm") and
// kerbside stops for buses; both are accepted by Departures, and a
// prediction filtered by a parent station covers all of its platforms.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	ix, err := p.loadRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []transit.Pattern
	for dir := range 2 {
		var resp stopsResp
		u := p.url("/stops",
			"filter[route]", r.ID,
			"filter[direction_id]", fmt.Sprint(dir),
			"include", "parent_station",
			"fields[stop]", "name,platform_code,location_type")
		if err := p.http.GetJSON(ctx, u, &resp); err != nil {
			return nil, err
		}
		if len(resp.Data) == 0 {
			continue
		}
		stops := make([]transit.Stop, 0, len(resp.Data))
		for _, s := range resp.Data {
			if s.ID == "" || s.Attributes.Name == "" {
				continue
			}
			stops = append(stops, transit.Stop{
				ID:       s.ID,
				Name:     s.Attributes.Name,
				ParentID: s.Relationships.ParentStation.id(),
			})
		}
		if len(stops) == 0 {
			continue
		}
		out = append(out, transit.Pattern{
			DirectionID: fmt.Sprint(dir),
			Headsign:    ix.destination(r.ID, dir),
			Stops:       stops,
		})
	}
	return out, nil
}

func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now()
	if len(stopIDs) == 0 {
		return &transit.Departures{Now: now}, nil
	}
	if window <= 0 {
		window = 90 * time.Minute
	}
	// The index only supplies rider-facing line names; a failure there must
	// not cost us the board, so fall back to native route ids.
	ix, _ := p.loadRoutes(ctx)

	stops := csv(stopIDs)
	routeIDs := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.ID != "" {
			routeIDs = append(routeIDs, r.ID)
		}
	}
	routeFilter := csv(routeIDs)

	q := []string{"filter[stop]", stops}
	if routeFilter != "" {
		q = append(q, "filter[route]", routeFilter)
	}
	q = append(q, "include", "trip,stop", "sort", "departure_time", "page[limit]", "100")
	var preds predictionsResp
	if err := p.http.GetJSON(ctx, p.url("/predictions", q...), &preds); err != nil {
		return nil, err
	}
	inc := indexIncluded(preds.Included)
	out := make([]transit.Departure, 0, len(preds.Data))
	predicted := map[string]bool{}
	for _, pr := range preds.Data {
		at, ok := xutil.FirstTime(pr.Attributes.DepartureTime, pr.Attributes.ArrivalTime)
		if !ok {
			continue // a prediction that neither arrives nor departs
		}
		tripID := pr.Relationships.Trip.id()
		if tripID != "" {
			predicted[tripID] = true
		}
		routeID := pr.Relationships.Route.id()
		stopID := pr.Relationships.Stop.id()
		headsign := inc.headsign(tripID)
		if headsign == "" {
			headsign = xutil.Str(pr.Attributes.TripHeadsign)
		}
		out = append(out, transit.Departure{
			At:          at,
			Live:        true,
			Cancelled:   cancelled(pr.Attributes.ScheduleRelationship),
			Platform:    inc.platform(stopID),
			Headsign:    headsign,
			DirectionID: dirID(pr.Attributes.DirectionID),
			Line:        ix.shortName(routeID),
			RouteID:     routeID,
			TripID:      tripID,
			StopID:      stopID,
		})
	}

	sched, err := p.schedules(ctx, stops, routeFilter, now, window)
	if err != nil {
		return nil, err
	}
	sinc := indexIncluded(sched.Included)
	for _, s := range sched.Data {
		tripID := s.Relationships.Trip.id()
		if tripID != "" && predicted[tripID] {
			continue // already on the board with a live time
		}
		at, ok := xutil.FirstTime(s.Attributes.DepartureTime, s.Attributes.ArrivalTime)
		if !ok {
			continue
		}
		routeID := s.Relationships.Route.id()
		headsign := sinc.headsign(tripID)
		if headsign == "" {
			headsign = xutil.Str(s.Attributes.StopHeadsign)
		}
		out = append(out, transit.Departure{
			At:          at,
			Scheduled:   at,
			Headsign:    headsign,
			DirectionID: dirID(s.Attributes.DirectionID),
			Line:        ix.shortName(routeID),
			RouteID:     routeID,
			TripID:      tripID,
			StopID:      s.Relationships.Stop.id(),
		})
	}
	transit.SortDepartures(out)
	return &transit.Departures{Now: now, Departures: out}, nil
}

// min_time and max_time are service-day clock times in the city's zone; the
// API would accept "25:30" for after-midnight service but we have no way to
// say which service day that belongs to here, so a window that crosses
// midnight is capped at 23:59 (see Info.Notes).
func (p *Provider) schedules(ctx context.Context, stops, routeFilter string, now time.Time, window time.Duration) (*schedulesResp, error) {
	from := now.In(p.loc)
	to := from.Add(window)
	maxTime := to.Format("15:04")
	if to.YearDay() != from.YearDay() || to.Year() != from.Year() {
		maxTime = "23:59"
	}
	q := []string{"filter[stop]", stops}
	if routeFilter != "" {
		q = append(q, "filter[route]", routeFilter)
	}
	q = append(q,
		"filter[min_time]", from.Format("15:04"),
		"filter[max_time]", maxTime,
		"include", "trip",
		"sort", "departure_time")
	var resp schedulesResp
	if err := p.http.GetJSON(ctx, p.url("/schedules", q...), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// JSON:API filter keys keep their literal brackets (the API accepts them)
// so fixture keys stay readable.
func (p *Provider) url(path string, kv ...string) string {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var sb strings.Builder
	sb.WriteString(httpx.Join(base, path))
	for i := 0; i+1 < len(kv); i += 2 {
		if i == 0 {
			sb.WriteByte('?')
		} else {
			sb.WriteByte('&')
		}
		sb.WriteString(kv[i])
		sb.WriteByte('=')
		sb.WriteString(escapeCSV(kv[i+1]))
	}
	return sb.String()
}

// The commas the API uses to separate filter values must stay literal, so
// only the elements around them are escaped.
func escapeCSV(v string) string {
	parts := strings.Split(v, ",")
	for i, s := range parts {
		parts[i] = url.QueryEscape(s)
	}
	return strings.Join(parts, ",")
}

func csv(xs []string) string { return strings.Join(xs, ",") }

func cancelled(rel *string) bool {
	switch strings.ToUpper(xutil.Str(rel)) {
	case "CANCELLED", "SKIPPED":
		return true
	}
	return false
}

func dirID(d *int) string {
	if d == nil {
		return ""
	}
	return fmt.Sprint(*d)
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
)
