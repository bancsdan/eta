// Package opendatach is the Swiss provider, backed by the keyless
// transport.opendata.ch API that covers every operator in the country. It
// has no route → stops call, so cities share one StopSearcher.
package opendatach

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const DefaultBaseURL = "https://transport.opendata.ch/v1"

type city struct {
	info    transit.Info
	aliases []string
}

// The API is national and unscoped, so cities differ only in metadata.
// Probes are a central stop and a local line that calls there (verified
// live 2026-09-15).
var cities = []city{
	{info: cityInfo("zurich", "Zürich (VBZ)", "11", "zürich, bellevue"), aliases: []string{"zuerich"}},
	{info: cityInfo("geneva", "Geneva (TPG)", "12", "genève, bel-air"), aliases: []string{"geneve", "genf"}},
	{info: cityInfo("basel", "Basel (BVB)", "8", "basel, barfüsserplatz")},
	{info: cityInfo("bern", "Bern (Bernmobil)", "9", "bern, bahnhof")},
	{info: cityInfo("lausanne", "Lausanne (TL)", "M2", "lausanne-flon")},
	{info: cityInfo("winterthur", "Winterthur (Stadtbus)", "1", "winterthur, hauptbahnhof")},
	{info: cityInfo("lucerne", "Lucerne (VBL)", "1", "luzern, bahnhof"), aliases: []string{"luzern"}},
	{info: cityInfo("stgallen", "St. Gallen (VBSG)", "1", "st. gallen, bahnhof"), aliases: []string{"st-gallen", "sanktgallen"}},
	{info: cityInfo("lugano", "Lugano (TPL)", "1", "lugano, centro")},
	{info: cityInfo("biel", "Biel/Bienne (VB)", "1", "biel/bienne, zentralplatz"), aliases: []string{"bienne"}},
	{info: cityInfo("thun", "Thun (STI)", "1", "thun, bahnhof")},
	{info: cityInfo("fribourg", "Fribourg (TPF)", "1", "fribourg/freiburg, pl. gare"), aliases: []string{"freiburg"}},
	{info: cityInfo("schaffhausen", "Schaffhausen (VBSH)", "1", "schaffhausen, bahnhof")},
	{info: cityInfo("chur", "Chur (Stadtbus)", "2", "chur, bahnhofplatz")},
	{info: cityInfo("neuchatel", "Neuchâtel (transN)", "101", "neuchâtel, place pury"), aliases: []string{"neuenburg"}},
	{info: cityInfo("sion", "Sion (Bus Sédunois)", "311", "sion, poste/gare"), aliases: []string{"sitten"}},
}

func cityInfo(id, name, route, query string) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name,
		Provider: "opendatach",
		Country:  "switzerland",
		TZ:       "Europe/Zurich",
		Realtime: true,
		Notes:    "whole of Switzerland; stop search only, -l not available",
		Probe:    transit.Probe{Route: route, Query: query},
	}
}

// Zurich is the primary city's Info, for callers constructing New directly.
var Zurich = cities[0].info

func init() {
	registry.RegisterCountry(registry.Country{ID: "switzerland", Name: "Switzerland", Aliases: []string{"ch", "schweiz", "suisse", "svizzera"}, Providers: []string{"opendatach"}, AnyTown: NewTown})
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
	info    transit.Info
	town    string // local town name to keep in station names ("Zürich", "Genève", or as typed)
	http    *httpx.Client
	now     func() time.Time
	loc     *time.Location
}

func New(d registry.Deps, info transit.Info) transit.Provider {
	return &Provider{BaseURL: DefaultBaseURL, info: info, town: localName(info), http: httpx.New("transport.opendata.ch", d.HTTP), now: d.Clock(), loc: info.Location()}
}

// NewTown builds a provider for any Swiss town: station names embed the
// town ("Zürich, Bellevue"), so the search is filtered on it.
func NewTown(d registry.Deps, town string) transit.Provider {
	info := cityInfo(strings.ReplaceAll(match.Normalize(town), " ", "-"), town+" (transport.opendata.ch)", "", "")
	info.Notes = "any Swiss town; stop search only, -l not available"
	return &Provider{BaseURL: DefaultBaseURL, info: info, town: town, http: httpx.New("transport.opendata.ch", d.HTTP), now: d.Clock(), loc: info.Location()}
}

// localName is the town as it appears in station names: the part of the
// registered probe before the comma.
func localName(info transit.Info) string {
	name, _, _ := strings.Cut(info.Probe.Query, ",")
	return strings.TrimSpace(name)
}

func (p *Provider) Info() transit.Info { return p.info }

func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	stops, err := p.search(ctx, query)
	if err != nil {
		return nil, err
	}
	town := match.Normalize(p.town)
	if town == "" {
		return stops, nil
	}
	if out := inTown(stops, town); len(out) > 0 {
		return out, nil
	}
	// A bare stop name ("bahnhof") lists stations all over the country and
	// may miss the town entirely; ask again with the town in the query.
	if !strings.Contains(match.Normalize(query), town) {
		more, err := p.search(ctx, p.town+" "+query)
		if err != nil {
			return nil, err
		}
		if out := inTown(more, town); len(out) > 0 {
			return out, nil
		}
	}
	return stops, nil
}

// inTown keeps the stations whose name carries the town.
func inTown(stops []transit.Stop, town string) []transit.Stop {
	var out []transit.Stop
	for _, st := range stops {
		if strings.Contains(match.Normalize(st.Name), town) {
			out = append(out, st)
		}
	}
	return out
}

func (p *Provider) search(ctx context.Context, query string) ([]transit.Stop, error) {
	q := url.Values{"query": {query}, "type": {"station"}}
	var resp locationsResp
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "locations", q), &resp); err != nil {
		return nil, err
	}
	all := make([]transit.Stop, 0, len(resp.Stations))
	for _, s := range resp.Stations {
		if s.ID == nil || *s.ID == "" || s.Name == "" {
			continue
		}
		st := transit.Stop{ID: *s.ID, Name: s.Name}
		if s.Coordinate != nil && s.Coordinate.X != nil && s.Coordinate.Y != nil {
			st.Lat, st.Lon = *s.Coordinate.X, *s.Coordinate.Y
		}
		all = append(all, st)
	}
	return all, nil
}

// The board cannot be filtered by line, so routes is ignored and the app
// filters by Line. limit is high so a rare line still makes the cut.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now()
	cutoff := now.Add(window)
	deps, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, id string) ([]transit.Departure, error) {
		q := url.Values{"station": {id}, "limit": {"100"}}
		var resp stationboardResp
		if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "stationboard", q), &resp); err != nil {
			return nil, err
		}
		out := make([]transit.Departure, 0, len(resp.Stationboard))
		for _, e := range resp.Stationboard {
			d, ok := p.toDeparture(e, id)
			if !ok || (window > 0 && d.At.After(cutoff)) {
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

// swissTime is the feed's timestamp layout: an offset without a colon, which
// time.RFC3339 rejects.
const swissTime = "2006-01-02T15:04:05-0700"

func (p *Provider) toDeparture(e entry, requested string) (transit.Departure, bool) {
	d := transit.Departure{
		Headsign: e.To,
		Line:     e.Number,
		TripID:   e.Name,
		Platform: xutil.Str(e.Stop.Platform),
		StopID:   requested,
	}
	if d.Line == "" {
		d.Line = e.Category
	}
	if e.Stop.Station.ID != nil && *e.Stop.Station.ID != "" {
		d.StopID = *e.Stop.Station.ID
	}
	if e.Stop.DepartureTimestamp != nil && *e.Stop.DepartureTimestamp > 0 {
		d.Scheduled = time.Unix(*e.Stop.DepartureTimestamp, 0).In(p.loc)
	} else if t, err := time.Parse(swissTime, e.Stop.Departure); err == nil {
		d.Scheduled = t.In(p.loc)
	}
	if d.Scheduled.IsZero() {
		return d, false
	}
	d.At = d.Scheduled
	// A prognosis is the operator's live prediction; a bare delay is the
	// fallback when only minutes late are known.
	if pr := e.Stop.Prognosis; pr != nil && pr.Departure != nil {
		if t, err := time.Parse(swissTime, *pr.Departure); err == nil {
			d.At, d.Live = t.In(p.loc), true
		}
		if pr.Platform != nil && *pr.Platform != "" {
			d.Platform = *pr.Platform
		}
	}
	if !d.Live && e.Stop.Delay != nil {
		d.At, d.Live = d.Scheduled.Add(time.Duration(*e.Stop.Delay)*time.Minute), true
	}
	return d, true
}

var _ transit.StopSearcher = (*Provider)(nil)
