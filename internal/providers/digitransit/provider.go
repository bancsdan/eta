// Package digitransit is the Finland provider, backed by the Digitransit
// routing API (OpenTripPlanner GraphQL). One package serves Helsinki through
// the HSL router and the other cities through the Waltti router, each scoped
// to its GTFS feed.
//
// Not yet verified against the live API: no key was available when this was
// written, so queries follow the published schema and the fixtures are
// hand-written.
package digitransit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const (
	DefaultBaseURL = "https://api.digitransit.fi/routing/v2"
	keyEnv         = "ETA_DIGITRANSIT_API_KEY"
	keyHeader      = "digitransit-subscription-key"
)

var keySpec = transit.KeySpec{Env: keyEnv, Req: transit.KeyRequired, SignupURL: "https://portal-api.digitransit.fi"}

// city binds a registered city to a router (the OTP instance) and the GTFS
// feed whose routes it should answer with; the Waltti router carries many
// regional feeds at once.
type city struct {
	info    transit.Info
	aliases []string
	router  string
	feeds   []string
}

var cities = []city{
	{info: cityInfo("helsinki", "Helsinki (Digitransit / HSL)", "550", "pasila"), aliases: []string{"hsl"}, router: "hsl", feeds: []string{"HSL"}},
	{info: cityInfo("tampere", "Tampere (Digitransit / Nysse)", "3", "keskustori"), aliases: []string{"nysse"}, router: "waltti", feeds: []string{"tampere"}},
	{info: cityInfo("turku", "Turku (Digitransit / Föli)", "1", "kauppatori"), aliases: []string{"foli"}, router: "waltti", feeds: []string{"FOLI"}},
}

func cityInfo(id, name, route, query string) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name,
		Provider: "digitransit",
		Country:  "Finland",
		TZ:       "Europe/Helsinki",
		Keys:     []transit.KeySpec{keySpec},
		Realtime: true,
		Notes:    "not yet verified against the live API",
		Probe:    transit.Probe{Route: route, Query: query},
	}
}

// Helsinki is the primary city's Info, for callers constructing New directly.
var Helsinki = cities[0].info

func init() {
	for _, c := range cities {
		c := c
		registry.Register(registry.Entry{
			Info:    c.info,
			Aliases: c.aliases,
			New:     func(d registry.Deps) transit.Provider { return New(d, c.info) },
		})
	}
}

type Provider struct {
	BaseURL string
	city    city
	key     string
	http    *httpx.Client
	now     func() time.Time
}

func New(d registry.Deps, info transit.Info) transit.Provider {
	c := cities[0]
	for _, x := range cities {
		if x.info.ID == info.ID {
			c = x
		}
	}
	key := d.KeyFor(keyEnv)
	h := httpx.New("digitransit", d.HTTP)
	if key != "" {
		h.Header.Set(keyHeader, key)
		h.Redact = []string{key}
	}
	return &Provider{BaseURL: DefaultBaseURL, city: c, key: key, http: h, now: d.Clock()}
}

func (p *Provider) Info() transit.Info { return p.city.info }

func (p *Provider) endpoint() string {
	return httpx.Join(p.BaseURL, p.city.router+"/gtfs/v1")
}

func (p *Provider) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	if p.key == "" {
		return fmt.Errorf("digitransit: %w: set %s (free key: %s)", transit.ErrNoKey, keyEnv, keySpec.SignupURL)
	}
	return p.http.GraphQL(ctx, p.endpoint(), query, vars, out)
}

const routesQuery = `query($name:String,$feeds:[String]){ routes(name:$name, feeds:$feeds) { gtfsId shortName longName mode } }`

func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	var data routesData
	if err := p.graphql(ctx, routesQuery, map[string]any{"name": short, "feeds": p.city.feeds}, &data); err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var out []transit.Route
	var others []string
	for _, r := range data.Routes {
		if match.Normalize(r.ShortName) == want {
			out = append(out, transit.Route{ID: r.GtfsID, ShortName: r.ShortName, LongName: r.LongName, Mode: strings.ToLower(r.Mode)})
			continue
		}
		others = append(others, r.ShortName)
	}
	if len(out) == 0 {
		return nil, transit.UnknownRoute(short, others)
	}
	return out, nil
}

const routeQuery = `query($id:String!){ route(id:$id) { patterns { code headsign directionId stops { gtfsId name lat lon platformCode parentStation { gtfsId } } } } }`

func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	var data routeData
	if err := p.graphql(ctx, routeQuery, map[string]any{"id": r.ID}, &data); err != nil {
		return nil, err
	}
	if data.Route == nil {
		return nil, fmt.Errorf("digitransit: route %s: %w", r.ID, transit.ErrNotFound)
	}
	out := make([]transit.Pattern, 0, len(data.Route.Patterns))
	for _, pat := range data.Route.Patterns {
		stops := make([]transit.Stop, 0, len(pat.Stops))
		for _, s := range pat.Stops {
			if s.GtfsID == "" || s.Name == "" {
				continue
			}
			stops = append(stops, toStop(s))
		}
		if len(stops) == 0 {
			continue
		}
		head := pat.Headsign
		if head == "" {
			head = stops[len(stops)-1].Name
		}
		out = append(out, transit.Pattern{DirectionID: strconv.Itoa(pat.DirectionID), Headsign: head, Stops: stops})
	}
	return out, nil
}

const stopsQuery = `query($name:String){ stops(name:$name) { gtfsId name lat lon platformCode parentStation { gtfsId } } }`

func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	var data stopsData
	if err := p.graphql(ctx, stopsQuery, map[string]any{"name": query}, &data); err != nil {
		return nil, err
	}
	out := make([]transit.Stop, 0, len(data.Stops))
	for _, s := range data.Stops {
		if s.GtfsID == "" || s.Name == "" {
			continue
		}
		// The Waltti router answers for every region it hosts; keep the city's feed.
		if !p.inFeeds(s.GtfsID) {
			continue
		}
		out = append(out, toStop(s))
	}
	return out, nil
}

func (p *Provider) inFeeds(gtfsID string) bool {
	feed, _, _ := strings.Cut(gtfsID, ":")
	for _, f := range p.city.feeds {
		if strings.EqualFold(f, feed) {
			return true
		}
	}
	return len(p.city.feeds) == 0
}

func toStop(s stopDTO) transit.Stop {
	st := transit.Stop{ID: s.GtfsID, Name: s.Name, Lat: s.Lat, Lon: s.Lon}
	if s.ParentStation != nil {
		st.ParentID = s.ParentStation.GtfsID
	}
	return st
}

const boardQuery = `query($ids:[String],$n:Int,$t:Int){ stops(ids:$ids) { gtfsId stoptimesWithoutPatterns(numberOfDepartures:$n, timeRange:$t, omitNonPickups:true) { serviceDay scheduledDeparture realtimeDeparture realtime realtimeState headsign trip { gtfsId directionId route { gtfsId shortName } } stop { gtfsId platformCode } } } }`

func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now()}
	ids := xutil.Dedupe(stopIDs)
	if len(ids) == 0 {
		return out, nil
	}
	secs := int(window / time.Second)
	if secs <= 0 {
		secs = 3600
	}
	var data boardData
	if err := p.graphql(ctx, boardQuery, map[string]any{"ids": ids, "n": 100, "t": secs}, &data); err != nil {
		return nil, err
	}
	for _, s := range data.Stops {
		for _, st := range s.Stoptimes {
			out.Departures = append(out.Departures, toDeparture(st, s.GtfsID))
		}
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

func toDeparture(st stoptime, stopID string) transit.Departure {
	d := transit.Departure{
		Scheduled: time.Unix(st.ServiceDay+st.ScheduledDeparture, 0),
		Live:      st.Realtime,
		Cancelled: st.RealtimeState == "CANCELED",
		Headsign:  st.Headsign,
		StopID:    stopID,
	}
	d.At = d.Scheduled
	if st.Realtime && st.RealtimeDeparture > 0 {
		d.At = time.Unix(st.ServiceDay+st.RealtimeDeparture, 0)
	}
	if st.Trip != nil {
		d.TripID = st.Trip.GtfsID
		d.DirectionID = st.Trip.DirectionID
		if st.Trip.Route != nil {
			d.RouteID = st.Trip.Route.GtfsID
			d.Line = st.Trip.Route.ShortName
		}
	}
	if st.Stop != nil {
		d.Platform = xutil.Str(st.Stop.PlatformCode)
		if st.Stop.GtfsID != "" {
			d.StopID = st.Stop.GtfsID
		}
	}
	return d
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
)
