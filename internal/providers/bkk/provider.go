package bkk

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

// DefaultBaseURL is the OpenAPI server URL joined with the "otp" dialect and
// the "/api/where" prefix every endpoint in the spec shares.
const DefaultBaseURL = "https://futar.bkk.hu/api/query/v1/ws/otp/api/where"

const (
	keyEnv    = "ETA_BKK_API_KEY"
	signupURL = "https://opendata.bkk.hu"
)

var info = transit.Info{
	ID:       "budapest",
	Provider: "bkk",
	Country:  "hungary",
	Name:     "Budapest (BKK FUTÁR)",
	TZ:       "Europe/Budapest",
	Keys:     []transit.KeySpec{{Env: keyEnv, Req: transit.KeyRequired, SignupURL: signupURL}},
	Realtime: true,
	Probe:    transit.Probe{Route: "155", Query: "zugligeti"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "hungary", Name: "Hungary", Aliases: []string{"hu", "magyarorszag"}, Providers: []string{"bkk"}})
	registry.Register(registry.Entry{Info: info, Aliases: []string{"bkk"}, New: New})
}

type Provider struct {
	BaseURL string

	c   *client
	now func() time.Time
}

func New(d registry.Deps) transit.Provider {
	return newProvider(d)
}

func newProvider(d registry.Deps) transit.Provider {
	key := d.KeyFor(keyEnv)
	h := httpx.New(info.ID, d.HTTP)
	h.Query = url.Values{"key": {key}, "version": {"4"}, "appVersion": {"eta-cli"}}
	h.Redact = []string{key}
	return &Provider{BaseURL: DefaultBaseURL, c: &client{http: h, key: key}, now: d.Clock()}
}

func (p *Provider) Info() transit.Info { return info }

// FUTÁR's search endpoint is a loose text match, so the exact short name
// filtering happens here; the other hits become "did you mean" suggestions.
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	q := url.Values{"query": {short}, "includeReferences": {"routes"}}
	var data searchData
	if _, err := p.c.get(ctx, p.url("search", q), &data); err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var out []transit.Route
	var others []string
	for _, id := range data.Entry.RouteIDs {
		r, ok := data.References.Routes[id]
		if !ok {
			continue
		}
		if match.Normalize(r.ShortName) == want {
			out = append(out, transit.Route{
				ID:        r.ID,
				ShortName: r.ShortName,
				LongName:  r.LongName,
				Mode:      strings.ToLower(r.Type),
			})
			continue
		}
		others = append(others, r.ShortName)
	}
	if len(out) == 0 {
		return nil, transit.UnknownRoute(short, others)
	}
	return out, nil
}

// Not yet verified against the live API: the shape follows the OpenAPI spec
// (entry.stopIds + references.stops).
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	q := url.Values{"query": {query}, "includeReferences": {"stops"}}
	var data searchData
	if _, err := p.c.get(ctx, p.url("search", q), &data); err != nil {
		return nil, err
	}
	out := make([]transit.Stop, 0, len(data.Entry.StopIDs))
	for _, id := range data.Entry.StopIDs {
		s, ok := data.References.Stops[id]
		if !ok || s.Name == "" {
			continue
		}
		out = append(out, transit.Stop{ID: id, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
	}
	return out, nil
}

func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	q := url.Values{"routeId": {r.ID}, "includeReferences": {"stops"}}
	var data routeDetailsData
	if _, err := p.c.get(ctx, p.url("route-details", q), &data); err != nil {
		return nil, err
	}
	out := make([]transit.Pattern, 0, len(data.Entry.Variants))
	for _, v := range data.Entry.Variants {
		pat := transit.Pattern{DirectionID: v.Direction, Headsign: v.Headsign}
		for _, id := range v.StopIDs {
			s, ok := data.References.Stops[id]
			if !ok || s.Name == "" {
				continue // reference missing: a nameless stop helps nobody
			}
			pat.Stops = append(pat.Stops, transit.Stop{ID: id, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
		}
		if len(pat.Stops) == 0 {
			continue
		}
		out = append(out, pat)
	}
	return out, nil
}

// routes, when they carry native ids, are passed as includeRouteId so the
// server keeps them in the limit; the result is still returned unfiltered.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	if len(stopIDs) == 0 {
		return &transit.Departures{Now: p.now()}, nil
	}
	q := url.Values{
		"stopId":            stopIDs,
		"minutesBefore":     {"0"},
		"minutesAfter":      {strconv.Itoa(xutil.WindowMinutes(window))},
		"limit":             {"200"},
		"onlyDepartures":    {"false"},
		"includeReferences": {"trips,stops,routes"},
	}
	for _, r := range routes {
		if r.ID != "" {
			q.Add("includeRouteId", r.ID)
		}
	}
	var data arrivalsData
	now, err := p.c.get(ctx, p.url("arrivals-and-departures-for-stop", q), &data)
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = p.now()
	}
	out := &transit.Departures{Now: now, Departures: make([]transit.Departure, 0, len(data.Entry.StopTimes))}
	for _, st := range data.Entry.StopTimes {
		if d, ok := departure(st, data.References); ok {
			out.Departures = append(out.Departures, d)
		}
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

func departure(st stopTime, refs arrivalRefs) (transit.Departure, bool) {
	if st.PickupAllowed != nil && !*st.PickupAllowed {
		return transit.Departure{}, false // end of the line, drop-off only
	}
	sched := xutil.EpochTime(first(st.DepartureTime, st.ArrivalTime))
	pred := xutil.EpochTime(first(st.PredictedDepartureTime, st.PredictedArrivalTime))
	// A prediction flagged predictionScheduled is schedule-derived, so it
	// is not a live one.
	live := !pred.IsZero() && !st.PredictionScheduled
	d := transit.Departure{
		Scheduled: sched,
		Live:      live,
		At:        sched,
		Headsign:  st.StopHeadsign,
		TripID:    st.TripID,
		StopID:    st.StopID,
	}
	if live {
		d.At = pred
	}
	if t, ok := refs.Trips[st.TripID]; ok {
		if t.Headsign != "" {
			d.Headsign = t.Headsign
		}
		d.DirectionID = t.DirectionID
		d.RouteID = t.RouteID
		if r, ok := refs.Routes[t.RouteID]; ok {
			d.Line = r.ShortName
		}
	}
	if d.At.IsZero() {
		return transit.Departure{}, false
	}
	return d, true
}

func (p *Provider) url(path string, q url.Values) string {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return httpx.URL(base, path, q)
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
)
