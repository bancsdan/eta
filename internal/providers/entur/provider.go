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
	town        string // set for an unregistered town: searches are scoped to it geographically
}

// One entry per city with its county operator's authority (route lookups
// are scoped to it) and a probe verified live on 2026-09-15: a central stop
// and a line of that operator calling there.
var cities = []city{
	ncity("oslo", "Oslo (Ruter)", "RUT:Authority:RUT", "31", "jernbanetorget", 59.911, 10.750, "entur", "ruter"),
	ncity("bergen", "Bergen (Skyss)", "SKY:Authority:SKY", "1", "byparken", 60.393, 5.324, "skyss"),
	ncity("trondheim", "Trondheim (AtB)", "ATB:Authority:2", "3", "dragvoll", 63.430, 10.395, "atb"),
	ncity("stavanger", "Stavanger (Kolumbus)", "KOL:Authority:8", "1", "hillevåg", 58.970, 5.733, "kolumbus"),
	ncity("drammen", "Drammen (Brakar)", "BRA:Authority:4", "1", "bragernes torg", 59.744, 10.204, "brakar"),
	ncity("fredrikstad", "Fredrikstad (Østfold kollektivtrafikk)", "OST:Authority:1", "1", "fredrikstad bussterminal", 59.211, 10.950),
	ncity("sarpsborg", "Sarpsborg (Østfold kollektivtrafikk)", "OST:Authority:1", "1", "sarpsborg bussterminal", 59.284, 11.109),
	ncity("kristiansand", "Kristiansand (AKT)", "AKT:Authority:AKT_ID", "10", "kristiansand rutebilstasjon", 58.146, 7.996, "akt"),
	ncity("tromso", "Tromsø (Svipper)", "TRO:Authority:1", "100", "prostneset", 69.649, 18.956, "tromsø", "svipper"),
	ncity("skien", "Skien (Farte)", "TEL:Authority:TFK_ID", "M1", "skien landmannstorget", 59.209, 9.608, "farte"),
	ncity("porsgrunn", "Porsgrunn (Farte)", "TEL:Authority:TFK_ID", "M1", "porsgrunn kammerherreløkka", 59.139, 9.657),
	ncity("hamar", "Hamar (Innlandstrafikk)", "INN:Authority:INN_ID", "B21", "hamar skysstasjon", 60.794, 11.068),
	ncity("lillehammer", "Lillehammer (Innlandstrafikk)", "INN:Authority:INN_ID", "B1", "lillehammer skysstasjon", 61.115, 10.463),
	ncity("molde", "Molde (FRAM)", "MOR:Authority:MOR", "100", "molde trafikkterminal", 62.737, 7.160),
	ncity("alesund", "Ålesund (FRAM)", "MOR:Authority:MOR", "1", "st. olavs plass", 62.472, 6.155, "ålesund"),
	ncity("bodo", "Bodø (Nordland fylkeskommune)", "NOR:Authority:12", "1", "bodø sentrum", 67.280, 14.405, "bodø"),
}

// ncity builds one table row; aliases are optional.
func ncity(id, name, authority, route, query string, lat, lon float64, aliases ...string) city {
	return city{
		info: transit.Info{
			ID:       id,
			Name:     name,
			Country:  "norway",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "route lookup scoped to the county operator; stop search covers all Norway",
			Probe:    transit.Probe{Route: route, Query: query},
		},
		aliases:     aliases,
		authorities: []string{authority},
		focusLat:    lat,
		focusLon:    lon,
	}
}

// Oslo is the primary city's Info, for callers constructing New directly.
var Oslo = cities[0].info

func init() {
	registry.RegisterCountry(registry.Country{ID: "norway", Name: "Norway", Aliases: []string{"no", "norge"}, Providers: []string{"entur"}, AnyTown: NewTown})
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

// NewTown builds a provider for a town without a registered entry. The
// town is geocoded once (cached) and every search is scoped to it: stops
// by locality, else within xutil.NearbyKm; lines by having a stop nearby.
func NewTown(d registry.Deps, town string) transit.Provider {
	h := httpx.New("entur", d.HTTP)
	h.Header.Set("ET-Client-Name", clientName)
	c := city{
		info: transit.Info{
			ID:       strings.ReplaceAll(match.Normalize(town), " ", "-"),
			Name:     town + " (Entur)",
			Country:  "norway",
			Provider: "entur",
			TZ:       "Europe/Oslo",
			Realtime: true,
			Notes:    "scoped to the town by locality and distance",
		},
		town: town,
	}
	return &Provider{BaseURL: defaultBaseURL, GeocoderURL: defaultGeocoderURL, city: c, http: h, cache: d.Cache, now: d.Clock(), loc: c.info.Location()}
}

// place is a geocoded town.
type place struct {
	Lat, Lon float64
	Locality string
}

// locate geocodes the town once; the result is cached with the town's
// other reference data.
func (p *Provider) locate(ctx context.Context) (place, error) {
	if p.city.town == "" {
		return place{Lat: p.city.focusLat, Lon: p.city.focusLon}, nil
	}
	var pl place
	if p.cache.Load("place", &pl) && pl.Lat != 0 {
		return pl, nil
	}
	// The geocoder has no place layer; the town itself comes back as an
	// entry whose locality (municipality) carries its name, which is what
	// stops are later matched on.
	q := url.Values{"text": {p.city.town}, "size": {"10"}, "lang": {"no"}}
	var resp geoResponse
	if err := p.http.GetJSON(ctx, httpx.URL(p.GeocoderURL, "autocomplete", q), &resp); err != nil {
		return pl, err
	}
	want := match.Normalize(p.city.town)
	var hit *geoFeature
	for i := range resp.Features {
		f := &resp.Features[i]
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		if match.Normalize(f.Properties.Locality) == want || match.Normalize(f.Properties.Name) == want {
			hit = f
			break
		}
	}
	if hit == nil {
		return pl, fmt.Errorf("entur: no place in Norway named %q", p.city.town)
	}
	pl = place{Lat: hit.Geometry.Coordinates[1], Lon: hit.Geometry.Coordinates[0], Locality: hit.Properties.Locality}
	if pl.Locality == "" {
		pl.Locality = p.city.town
	}
	_ = p.cache.Store("place", pl)
	return pl, nil
}

// inTown reports whether a point belongs to the town: same locality name
// when known, else within xutil.NearbyKm of its centre.
func (p *Provider) inTown(pl place, locality string, lat, lon float64) bool {
	if locality != "" && pl.Locality != "" && match.Normalize(locality) == match.Normalize(pl.Locality) {
		return true
	}
	return lat != 0 && xutil.DistanceKm(pl.Lat, pl.Lon, lat, lon) <= xutil.NearbyKm
}

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

const linesQuery = `query($code:String,$auth:[String]){ lines(publicCode:$code, authorities:$auth) { id publicCode name transportMode journeyPatterns { id directionType name quays { id name publicCode latitude longitude stopPlace { id name } } } } }`

const lineQuery = `query($id:ID!){ line(id:$id) { id publicCode name transportMode journeyPatterns { id directionType name quays { id name publicCode latitude longitude stopPlace { id name } } } } }`

// The full response is cached so RouteStops needs no second round trip.
func (p *Provider) lines(ctx context.Context, short string) ([]lineDTO, error) {
	key := "lines-" + match.Normalize(short)
	var cached []lineDTO
	if p.cache.Load(key, &cached) && len(cached) > 0 {
		return cached, nil
	}
	vars := map[string]any{"code": short}
	if len(p.city.authorities) > 0 {
		vars["auth"] = p.city.authorities
	}
	var data linesData
	if err := p.graphql(ctx, linesQuery, vars, &data); err != nil {
		return nil, err
	}
	lines := data.Lines
	if p.city.town != "" {
		// Nationwide match on the public code: keep the lines that stop in town.
		pl, err := p.locate(ctx)
		if err != nil {
			return nil, err
		}
		lines = nil
		for _, l := range data.Lines {
			if p.lineNear(l, pl) {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) > 0 {
		_ = p.cache.Store(key, lines)
	}
	return lines, nil
}

func (p *Provider) lineNear(l lineDTO, pl place) bool {
	for _, jp := range l.JourneyPatterns {
		for _, q := range jp.Quays {
			if p.inTown(pl, "", q.Latitude, q.Longitude) {
				return true
			}
		}
	}
	return false
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
	pl, err := p.locate(ctx)
	if err != nil {
		return nil, err
	}
	if pl.Lat != 0 {
		q.Set("focus.point.lat", fmt.Sprint(pl.Lat))
		q.Set("focus.point.lon", fmt.Sprint(pl.Lon))
	}
	if p.city.town != "" {
		q.Set("size", "20")
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
		if p.city.town != "" && !p.inTown(pl, pr.Locality, s.Lat, s.Lon) {
			continue
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
