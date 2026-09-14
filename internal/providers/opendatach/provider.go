// Package opendatach is the Swiss provider, backed by the keyless
// transport.opendata.ch API that covers every operator in the country. It
// has no route → stops call, so cities share one StopSearcher.
package opendatach

import (
	"context"
	"net/url"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
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
var cities = []city{
	{info: cityInfo("zurich", "Zürich (SBB / VBZ)", "11", "bellevue"), aliases: []string{"zuerich", "ch", "switzerland"}},
	{info: cityInfo("bern", "Bern (SBB / Bernmobil)", "9", "bern, bahnhof"), aliases: nil},
	{info: cityInfo("basel", "Basel (SBB / BVB)", "8", "basel, barfüsserplatz"), aliases: nil},
	{info: cityInfo("geneva", "Geneva (SBB / TPG)", "12", "genève, bel-air"), aliases: []string{"geneve", "genf"}},
	{info: cityInfo("lausanne", "Lausanne (SBB / TL)", "M2", "lausanne-flon"), aliases: nil},
}

func cityInfo(id, name, route, query string) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name,
		Provider: "opendatach",
		TZ:       "Europe/Zurich",
		Realtime: true,
		Notes:    "whole of Switzerland; stop search only, -l not available",
		Probe:    transit.Probe{Route: route, Query: query},
	}
}

// Zurich is the primary city's Info, for callers constructing New directly.
var Zurich = cities[0].info

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
	info    transit.Info
	http    *httpx.Client
	now     func() time.Time
	loc     *time.Location
}

func New(d registry.Deps, info transit.Info) transit.Provider {
	return &Provider{BaseURL: DefaultBaseURL, info: info, http: httpx.New("transport.opendata.ch", d.HTTP), now: d.Clock(), loc: info.Location()}
}

func (p *Provider) Info() transit.Info { return p.info }

func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	q := url.Values{"query": {query}, "type": {"station"}}
	var resp locationsResp
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "locations", q), &resp); err != nil {
		return nil, err
	}
	out := make([]transit.Stop, 0, len(resp.Stations))
	for _, s := range resp.Stations {
		if s.ID == nil || *s.ID == "" || s.Name == "" {
			continue
		}
		st := transit.Stop{ID: *s.ID, Name: s.Name}
		if s.Coordinate != nil && s.Coordinate.X != nil && s.Coordinate.Y != nil {
			st.Lat, st.Lon = *s.Coordinate.X, *s.Coordinate.Y
		}
		out = append(out, st)
	}
	return out, nil
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
