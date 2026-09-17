// Package ovapi is the Netherlands provider, backed by the community-run
// OVapi (v0.ovapi.nl) that republishes every Dutch operator's real-time
// feed. Stop areas carry their town, which scopes searches; there is no
// route → stops call, so it is a StopSearcher only.
package ovapi

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const DefaultBaseURL = "http://v0.ovapi.nl"

const indexTTL = 24 * time.Hour

type city struct {
	info    transit.Info
	aliases []string
	town    string // TimingPointTown as OVapi spells it
}

// Probes verified live on 2026-09-17: a central stop area and a line that
// passes it.
var cities = []city{
	{cityInfo("amsterdam", "Amsterdam (GVB)", "53", "centraal station"), []string{"ams"}, "Amsterdam"},
	{cityInfo("rotterdam", "Rotterdam (RET)", "E", "rotterdam centraal"), nil, "Rotterdam"},
	{cityInfo("thehague", "The Hague (HTM)", "1", "den haag centraal"), []string{"denhaag", "den-haag", "hague", "the-hague"}, "Den Haag"},
	{cityInfo("utrecht", "Utrecht (U-OV)", "1", "centraal station"), nil, "Utrecht"},
	{cityInfo("eindhoven", "Eindhoven (Hermes)", "1", "station"), nil, "Eindhoven"},
	{cityInfo("groningen", "Groningen (Qbuzz)", "1", "hoofdstation"), nil, "Groningen"},
}

func cityInfo(id, name, route, query string) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name,
		Country:  "netherlands",
		Provider: "ovapi",
		TZ:       "Europe/Amsterdam",
		Realtime: true,
		Notes:    "stations and interchanges only (OVapi's area index has no plain tram/bus stops); first run downloads the national stop list; -l not available",
		Probe:    transit.Probe{Route: route, Query: query},
	}
}

// Amsterdam is the primary town's Info, for callers constructing New directly.
var Amsterdam = cities[0].info

func init() {
	registry.RegisterCountry(registry.Country{ID: "netherlands", Name: "Netherlands", Aliases: []string{"nl", "nederland", "holland"}, Providers: []string{"ovapi"}, AnyTown: NewTown})
	for _, c := range cities {
		c := c
		registry.Register(registry.Entry{
			Info:    c.info,
			Aliases: c.aliases,
			New:     func(d registry.Deps) transit.Provider { return newProvider(d, c) },
		})
	}
}

type Provider struct {
	BaseURL string
	city    city
	http    *httpx.Client
	index   *cache.Cache // provider-wide: the stop-area index is national
	now     func() time.Time
	loc     *time.Location
}

func New(d registry.Deps, info transit.Info) transit.Provider {
	for _, c := range cities {
		if c.info.ID == info.ID {
			return newProvider(d, c)
		}
	}
	return newProvider(d, cities[0])
}

// NewTown builds a provider for any Dutch town, matched against the
// TimingPointTown of the national stop-area index.
func NewTown(d registry.Deps, town string) transit.Provider {
	info := cityInfo(strings.ReplaceAll(match.Normalize(town), " ", "-"), town+" (OVapi)", "", "")
	return newProvider(d, city{info: info, town: town})
}

func newProvider(d registry.Deps, c city) *Provider {
	return &Provider{BaseURL: DefaultBaseURL, city: c, http: httpx.New("ovapi", d.HTTP), index: cache.Default("ovapi", indexTTL), now: d.Clock(), loc: c.info.Location()}
}

func (p *Provider) Info() transit.Info { return p.city.info }

// Cold reports whether the national stop-area index still has to be fetched.
func (p *Provider) Cold() bool {
	var idx map[string]areaEntry
	return !p.index.Load("stopareas", &idx) || len(idx) == 0
}

func (p *Provider) areas(ctx context.Context) (map[string]areaEntry, error) {
	var idx map[string]areaEntry
	if p.index.Load("stopareas", &idx) && len(idx) > 0 {
		return idx, nil
	}
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "stopareacode", nil), &idx); err != nil {
		return nil, err
	}
	for k := range idx {
		if idx[k].StopAreaCode == "" {
			delete(idx, k) // a few index rows are empty objects
		}
	}
	if len(idx) > 0 {
		_ = p.index.Store("stopareas", idx)
	}
	return idx, nil
}

// SearchStops matches stop-area names within the town. Areas sharing a
// name (the several parts of a station) come back as separate ids so the
// app queries them all.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	idx, err := p.areas(ctx)
	if err != nil {
		return nil, err
	}
	town := match.Normalize(p.city.town)
	var all []match.Stop
	byID := map[string]transit.Stop{}
	for code, a := range idx {
		if match.Normalize(a.TimingPointTown) != town {
			continue
		}
		name := a.TimingPointName
		// Many names already start with the town ("Amsterdam, Museumplein").
		name = strings.TrimSpace(strings.TrimPrefix(name, a.TimingPointTown+","))
		all = append(all, match.Stop{ID: code, Name: name})
		byID[code] = transit.Stop{ID: code, Name: name, Lat: a.Latitude, Lon: a.Longitude}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("ovapi: no stops in a Dutch town named %q", p.city.town)
	}
	// Map order is random; a stable order keeps runs reproducible.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].ID < all[j].ID
	})
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

// The board cannot be filtered by line; the app filters by Line.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now()
	cutoff := now.Add(window)
	deps, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, id string) ([]transit.Departure, error) {
		var resp map[string]map[string]timingPoint
		if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "stopareacode/"+url.PathEscape(id), nil), &resp); err != nil {
			return nil, err
		}
		var out []transit.Departure
		for _, tps := range resp {
			for _, tp := range tps {
				for _, ps := range tp.Passes {
					d, ok := p.toDeparture(ps, id)
					if !ok || (window > 0 && d.At.After(cutoff)) {
						continue
					}
					out = append(out, d)
				}
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

func (p *Provider) toDeparture(ps pass, areaID string) (transit.Departure, bool) {
	if ps.TripStopStatus == "PASSED" {
		return transit.Departure{}, false
	}
	d := transit.Departure{
		Headsign:    ps.DestinationName50,
		Line:        ps.LinePublicNumber,
		DirectionID: strconv.Itoa(ps.LineDirection),
		Cancelled:   strings.HasPrefix(ps.TripStopStatus, "CANCEL"),
		Live:        ps.TripStopStatus == "DRIVING" || ps.TripStopStatus == "ARRIVED",
		Platform:    ps.UserStopCode,
		StopID:      areaID,
		TripID:      ps.LinePlanningNumber + "/" + strconv.FormatInt(ps.JourneyNumber, 10),
	}
	if d.Line == "" {
		d.Line = ps.LinePlanningNumber
	}
	if t, err := time.ParseInLocation(localTime, ps.TargetDepartureTime, p.loc); err == nil {
		d.Scheduled = t
	}
	d.At = d.Scheduled
	if d.Live {
		if t, err := time.ParseInLocation(localTime, ps.ExpectedDepartureTime, p.loc); err == nil {
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
