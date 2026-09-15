// Package sl is the Stockholm provider, backed by SL's keyless Transport
// API. There is no name search: the full site list is fetched once a day
// and matched locally, and no route → stops call, so it is a StopSearcher.
package sl

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

const DefaultBaseURL = "https://transport.integration.sl.se/v1"

const sitesCacheKey = "sites"

var info = transit.Info{
	ID:       "stockholm",
	Name:     "Stockholm (SL)",
	Provider: "sl",
	Country:  "sweden",
	TZ:       "Europe/Stockholm",
	Realtime: true,
	Notes:    "first run downloads the stop list; stop search only, -l not available",
	Probe:    transit.Probe{Route: "19", Query: "slussen"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "sweden", Name: "Sweden", Aliases: []string{"se", "sverige"}, Providers: []string{"sl"}})
	registry.Register(registry.Entry{Info: info, Aliases: []string{"sl"}, New: New})
}

type Provider struct {
	BaseURL string
	http    *httpx.Client
	cache   *cache.Cache
	now     func() time.Time
	loc     *time.Location
}

func New(d registry.Deps) transit.Provider {
	return &Provider{BaseURL: DefaultBaseURL, http: httpx.New("sl", d.HTTP), cache: d.Cache, now: d.Clock(), loc: info.Location()}
}

func (p *Provider) Info() transit.Info { return info }

// Cold reports whether the site list still has to be downloaded.
func (p *Provider) Cold() bool {
	var sites []site
	return !p.cache.Load(sitesCacheKey, &sites) || len(sites) == 0
}

func (p *Provider) sites(ctx context.Context) ([]site, error) {
	var sites []site
	if p.cache.Load(sitesCacheKey, &sites) && len(sites) > 0 {
		return sites, nil
	}
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "sites", url.Values{"expand": {"false"}}), &sites); err != nil {
		return nil, err
	}
	if len(sites) > 0 {
		_ = p.cache.Store(sitesCacheKey, sites)
	}
	return sites, nil
}

func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	sites, err := p.sites(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]match.Stop, 0, len(sites))
	byID := make(map[string]site, len(sites))
	for _, s := range sites {
		id := strconv.Itoa(s.ID)
		all = append(all, match.Stop{ID: id, Name: s.Name})
		byID[id] = s
	}
	cands := match.Find(query, all)
	if len(cands) == 0 {
		cands = match.Closest(query, all, 8)
	}
	var out []transit.Stop
	for _, c := range cands {
		for _, id := range c.IDs {
			s := byID[id]
			out = append(out, transit.Stop{ID: id, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
		}
	}
	return out, nil
}

// A single numeric route is passed as the API's line filter (the parameter
// is an integer and matches the public designation); anything else is left
// to the app's client-side filter.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now()
	q := url.Values{"forecast": {strconv.Itoa(xutil.WindowMinutes(window))}}
	if len(routes) == 1 {
		if _, err := strconv.Atoi(routes[0].ShortName); err == nil {
			q.Set("line", routes[0].ShortName)
		}
	}
	deps, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, id string) ([]transit.Departure, error) {
		var resp departuresResp
		if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "sites/"+url.PathEscape(id)+"/departures", q), &resp); err != nil {
			return nil, err
		}
		out := make([]transit.Departure, 0, len(resp.Departures))
		for _, r := range resp.Departures {
			if d, ok := p.toDeparture(r, id); ok {
				out = append(out, d)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return &transit.Departures{Now: now, Departures: deps}, nil
}

const localTime = "2006-01-02T15:04:05"

func (p *Provider) toDeparture(r departure, requested string) (transit.Departure, bool) {
	d := transit.Departure{
		Headsign:    r.Destination,
		DirectionID: strconv.Itoa(r.DirectionCode),
		Cancelled:   r.State == "CANCELLED",
		Live:        r.State == "EXPECTED" || r.State == "ATSTOP",
		StopID:      requested,
	}
	if r.Line != nil {
		d.Line = r.Line.Designation
	}
	if r.Journey != nil && r.Journey.ID != 0 {
		d.TripID = fmt.Sprint(r.Journey.ID)
	}
	if r.StopPoint != nil {
		d.Platform = r.StopPoint.Designation
		if r.StopPoint.ID != 0 {
			d.StopID = strconv.Itoa(r.StopPoint.ID)
		}
	}
	if t, err := time.ParseInLocation(localTime, r.Scheduled, p.loc); err == nil {
		d.Scheduled = t
	}
	d.At = d.Scheduled
	if d.Live {
		if t, err := time.ParseInLocation(localTime, r.Expected, p.loc); err == nil {
			d.At = t
		}
	}
	if d.At.IsZero() {
		return d, false
	}
	return d, true
}

var (
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
