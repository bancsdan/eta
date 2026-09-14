// Package entur is the Oslo / Norway provider, backed by Entur's national
// APIs: the Pelias geocoder for stop search and JourneyPlanner v3 (GraphQL)
// for lines and real-time departures. Both are keyless but require the
// ET-Client-Name header on every request.
package entur

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const (
	defaultBaseURL     = "https://api.entur.io/journey-planner/v3/graphql"
	defaultGeocoderURL = "https://api.entur.io/geocoder/v1"
)

// clientName is the value Entur requires in ET-Client-Name so it can
// attribute traffic; there is no API key.
const clientName = "bancsdan-eta"

// Anything not starting with quayPrefix is treated as a StopPlace.
const quayPrefix = "NSR:Quay:"

// city is one registered city. Authorities scope line lookups to its
// operator: without that a public code like "31" matches dozens of lines
// nationwide. Focus ranks geocoder hits near the city first.
type city struct {
	info        transit.Info
	aliases     []string
	authorities []string
	focusLat    float64
	focusLon    float64
}

var cities = []city{
	{
		info: transit.Info{
			ID:       "oslo",
			Name:     "Oslo (Entur / Ruter)",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "route lookup scoped to Ruter (Oslo & Akershus); stop search covers all Norway",
			Probe:    transit.Probe{Route: "31", Query: "jernbanetorget"},
		},
		aliases:     []string{"entur", "ruter"},
		authorities: []string{"RUT:Authority:RUT"},
		focusLat:    59.911, focusLon: 10.750,
	},
	{
		info: transit.Info{
			ID:       "bergen",
			Name:     "Bergen (Entur / Skyss)",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "route lookup scoped to Skyss; stop search covers all Norway",
			Probe:    transit.Probe{Route: "1", Query: "byparken"},
		},
		aliases:     []string{"skyss"},
		authorities: []string{"SKY:Authority:SKY"},
		focusLat:    60.393, focusLon: 5.324,
	},
	{
		info: transit.Info{
			ID:       "trondheim",
			Name:     "Trondheim (Entur / AtB)",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "route lookup scoped to AtB; stop search covers all Norway",
			Probe:    transit.Probe{Route: "3", Query: "dragvoll"},
		},
		aliases:     []string{"atb"},
		authorities: []string{"ATB:Authority:2"},
		focusLat:    63.430, focusLon: 10.395,
	},
	{
		// Kolumbus publishes its timetable lines under KOL:Authority:8;
		// KOL:Authority:KOL only carries the on-demand "HentMeg" service.
		info: transit.Info{
			ID:       "stavanger",
			Name:     "Stavanger (Entur / Kolumbus)",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "route lookup scoped to Kolumbus; stop search covers all Norway",
			Probe:    transit.Probe{Route: "1", Query: "hillevåg"},
		},
		aliases:     []string{"kolumbus"},
		authorities: []string{"KOL:Authority:8"},
		focusLat:    58.970, focusLon: 5.733,
	},
}

// Oslo is the primary city's Info, for callers constructing New directly.
var Oslo = cities[0].info

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
	BaseURL     string
	GeocoderURL string

	city  city
	http  *httpx.Client
	cache *cache.Cache
	now   func() time.Time
	loc   *time.Location
}

func New(d registry.Deps, info transit.Info) transit.Provider {
	h := httpx.New("entur", d.HTTP)
	h.Header.Set("ET-Client-Name", clientName)
	c := cityFor(info.ID)
	return &Provider{
		city:        c,
		BaseURL:     defaultBaseURL,
		GeocoderURL: defaultGeocoderURL,
		http:        h,
		cache:       d.Cache,
		now:         d.Clock(),
		loc:         c.info.Location(),
	}
}

func (p *Provider) Info() transit.Info { return p.city.info }

// cityFor returns the registered city with id, defaulting to the first.
func cityFor(id string) city {
	for _, c := range cities {
		if c.info.ID == id {
			return c
		}
	}
	return cities[0]
}

func (p *Provider) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	return p.http.GraphQL(ctx, p.BaseURL, query, vars, out)
}

const linesQuery = `query($code:String,$auth:[String]){ lines(publicCode:$code, authorities:$auth) { id publicCode name transportMode journeyPatterns { id directionType name quays { id name publicCode stopPlace { id name } } } } }`

const lineQuery = `query($id:ID!){ line(id:$id) { id publicCode name transportMode journeyPatterns { id directionType name quays { id name publicCode stopPlace { id name } } } } }`

// The full response is cached so RouteStops needs no second round trip.
func (p *Provider) lines(ctx context.Context, short string) ([]lineDTO, error) {
	key := "lines-" + match.Normalize(short)
	var cached []lineDTO
	if p.cache.Load(key, &cached) && len(cached) > 0 {
		return cached, nil
	}
	var data linesData
	if err := p.graphql(ctx, linesQuery, map[string]any{"code": short, "auth": p.city.authorities}, &data); err != nil {
		return nil, err
	}
	if len(data.Lines) > 0 {
		_ = p.cache.Store(key, data.Lines)
	}
	return data.Lines, nil
}

func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	short = strings.TrimSpace(short)
	if short == "" {
		return nil, &transit.UnknownRouteError{Route: short}
	}
	lines, err := p.lines(ctx, short)
	if err != nil {
		return nil, err
	}
	out := make([]transit.Route, 0, len(lines))
	for _, l := range lines {
		if l.ID == "" {
			continue
		}
		code := l.PublicCode
		if code == "" {
			code = short
		}
		out = append(out, transit.Route{
			ID:        l.ID,
			ShortName: code,
			LongName:  l.Name,
			Mode:      l.TransportMode,
		})
	}
	if len(out) == 0 {
		return nil, &transit.UnknownRouteError{Route: short}
	}
	return out, nil
}

// Several patterns share a direction (short turns, branches); the app
// merges them, longest first.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	line, err := p.line(ctx, r)
	if err != nil {
		return nil, err
	}
	if line == nil {
		return nil, fmt.Errorf("entur: line %s: %w", r.ID, transit.ErrNotFound)
	}
	out := make([]transit.Pattern, 0, len(line.JourneyPatterns))
	for _, jp := range line.JourneyPatterns {
		stops := make([]transit.Stop, 0, len(jp.Quays))
		for _, q := range jp.Quays {
			if q.ID == "" {
				continue
			}
			s := transit.Stop{ID: q.ID, Name: q.Name}
			if q.StopPlace != nil {
				if q.StopPlace.Name != "" {
					s.Name = q.StopPlace.Name
				}
				s.ParentID = q.StopPlace.ID
			}
			if s.Name == "" {
				s.Name = q.ID
			}
			stops = append(stops, s)
		}
		if len(stops) == 0 {
			continue
		}
		headsign := jp.Name
		if headsign == "" {
			headsign = stops[len(stops)-1].Name
		}
		out = append(out, transit.Pattern{
			DirectionID: jp.DirectionType,
			Headsign:    headsign,
			Stops:       stops,
		})
	}
	return out, nil
}

func (p *Provider) line(ctx context.Context, r transit.Route) (*lineDTO, error) {
	if r.ShortName != "" {
		lines, err := p.lines(ctx, r.ShortName)
		if err != nil {
			return nil, err
		}
		for i := range lines {
			if r.ID == "" || lines[i].ID == r.ID {
				return &lines[i], nil
			}
		}
	}
	if r.ID == "" {
		return nil, &transit.UnknownRouteError{Route: r.ShortName}
	}
	var data lineData
	if err := p.graphql(ctx, lineQuery, map[string]any{"id": r.ID}, &data); err != nil {
		return nil, err
	}
	return data.Line, nil
}

// Names get the locality appended when it adds information, since many
// Norwegian stop names repeat across the country.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	q := url.Values{"text": {query}, "layers": {"venue"}, "size": {"10"}, "lang": {"en"}}
	if p.city.focusLat != 0 {
		q.Set("focus.point.lat", fmt.Sprint(p.city.focusLat))
		q.Set("focus.point.lon", fmt.Sprint(p.city.focusLon))
	}
	u := httpx.URL(p.GeocoderURL, "autocomplete", q)
	var resp geoResponse
	if err := p.http.GetJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	out := make([]transit.Stop, 0, len(resp.Features))
	for _, f := range resp.Features {
		pr := f.Properties
		if pr.ID == "" {
			continue
		}
		name := pr.Name
		if name == "" {
			name = pr.Label
		}
		if name == "" {
			continue
		}
		if pr.Locality != "" && match.Normalize(pr.Locality) != match.Normalize(name) {
			name += ", " + pr.Locality
		}
		s := transit.Stop{ID: pr.ID, Name: name}
		if len(f.Geometry.Coordinates) >= 2 {
			s.Lon, s.Lat = f.Geometry.Coordinates[0], f.Geometry.Coordinates[1]
		}
		out = append(out, s)
	}
	return out, nil
}

// The quay and stopPlace branches are different GraphQL types, so a single
// fragment cannot cover both; this selection set is shared textually.
const callsSelection = `{ name estimatedCalls(numberOfDepartures: 100, timeRange: %d) { aimedDepartureTime expectedDepartureTime realtime cancellation predictionInaccurate destinationDisplay { frontText } serviceJourney { id line { id publicCode transportMode } directionType } quay { id publicCode } } }`

// Each id is fetched as an aliased field of one GraphQL query, so N stops
// cost one round trip. routes is ignored: JourneyPlanner has no per-line
// filter on estimatedCalls and the app filters client-side anyway.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now()}
	ids := xutil.Dedupe(stopIDs)
	if len(ids) == 0 {
		return out, nil
	}
	secs := int(window / time.Second)
	if secs <= 0 {
		secs = int(time.Hour / time.Second)
	}
	sel := fmt.Sprintf(callsSelection, secs)

	var head, body strings.Builder
	vars := make(map[string]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			head.WriteString(",")
		}
		fmt.Fprintf(&head, "$id%d:String!", i)
		field := "stopPlace"
		if strings.HasPrefix(id, quayPrefix) {
			field = "quay"
		}
		fmt.Fprintf(&body, " s%d: %s(id:$id%d) %s", i, field, i, sel)
		vars[fmt.Sprintf("id%d", i)] = id
	}
	query := "query(" + head.String() + "){" + body.String() + " }"

	var places map[string]*placeDTO
	if err := p.graphql(ctx, query, vars, &places); err != nil {
		return nil, err
	}
	for i, id := range ids {
		pl := places[fmt.Sprintf("s%d", i)]
		if pl == nil {
			continue
		}
		for _, c := range pl.EstimatedCalls {
			out.Departures = append(out.Departures, p.toDeparture(c, id))
		}
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

func (p *Provider) toDeparture(c estimatedCallDTO, fallbackStop string) transit.Departure {
	d := transit.Departure{
		Live:      c.Realtime,
		Cancelled: c.Cancellation,
		StopID:    fallbackStop,
	}
	d.Scheduled = p.parseTime(c.AimedDepartureTime)
	d.At = p.parseTime(c.ExpectedDepartureTime)
	if d.At.IsZero() {
		d.At = d.Scheduled
	}
	if c.DestinationDisplay != nil {
		d.Headsign = c.DestinationDisplay.FrontText
	}
	if sj := c.ServiceJourney; sj != nil {
		d.TripID = sj.ID
		d.DirectionID = sj.DirectionType
		if sj.Line != nil {
			d.Line = sj.Line.PublicCode
			d.RouteID = sj.Line.ID
		}
	}
	if c.Quay != nil {
		d.Platform = c.Quay.PublicCode
		if c.Quay.ID != "" {
			d.StopID = c.Quay.ID
		}
	}
	return d
}

func (p *Provider) parseTime(s string) time.Time {
	t := xutil.ParseRFC3339(s)
	if t.IsZero() {
		return t
	}
	return t.In(p.loc)
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
)
