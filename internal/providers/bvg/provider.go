// Package bvg is the Berlin (BVG/VBB) provider, backed by the keyless
// v6.bvg.transport.rest HAFAS wrapper. The API has no route → stops call,
// so the package implements StopSearcher only and the app filters the board
// by line name.
package bvg

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

// The public, keyless BVG endpoint allows 100 requests/minute.
const DefaultBaseURL = "https://v6.bvg.transport.rest"

// city is one registered city on the shared endpoint. Search is not scoped
// (the HAFAS backend ranks by relevance), so cities differ only in metadata.
type city struct {
	info    transit.Info
	aliases []string
}

var cities = []city{
	{
		info: transit.Info{
			ID:       "berlin",
			Name:     "Berlin (BVG)",
			Provider: "bvg",
			Country:  "Germany",
			TZ:       "Europe/Berlin",
			Realtime: true,
			Notes:    "Berlin & Brandenburg; -l not available",
			Probe:    transit.Probe{Route: "M4", Query: "alexanderplatz"},
		},
		aliases: []string{"bvg"},
	},
	{
		// Same VBB data set; only the metadata and probe differ.
		info: transit.Info{
			ID:       "potsdam",
			Name:     "Potsdam (VBB)",
			Provider: "bvg",
			Country:  "Germany",
			TZ:       "Europe/Berlin",
			Realtime: true,
			Notes:    "served by the Berlin/Brandenburg endpoint; -l not available",
			Probe:    transit.Probe{Route: "91", Query: "potsdam hauptbahnhof"},
		},
	},
}

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
}

// New builds the provider for one of the registered cities' Info.
func New(d registry.Deps, info transit.Info) transit.Provider {
	return &Provider{BaseURL: DefaultBaseURL, info: info, http: httpx.New("bvg", d.HTTP), now: d.Clock()}
}

// Berlin is the Info of the primary city, for callers constructing New directly.
var Berlin = cities[0].info

func (p *Provider) Info() transit.Info { return p.info }

// Results stay in the API's own relevance order.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	q := url.Values{
		"query":     {query},
		"results":   {"10"},
		"poi":       {"false"},
		"addresses": {"false"},
		"stops":     {"true"},
	}
	var locs []apiLocation
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "/locations", q), &locs); err != nil {
		return nil, err
	}
	out := make([]transit.Stop, 0, len(locs))
	for _, l := range locs {
		if l.ID == "" || l.Name == "" || (l.Type != "stop" && l.Type != "station") {
			continue
		}
		s := transit.Stop{ID: l.ID, Name: l.Name}
		if l.Location != nil {
			s.Lat, s.Lon = l.Location.Latitude, l.Location.Longitude
		}
		out = append(out, s)
	}
	return out, nil
}

// The API cannot filter by line, so routes is ignored and the app filters
// client-side by Line.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	q := url.Values{"duration": {strconv.Itoa(xutil.WindowMinutes(window))}, "results": {"100"}}
	deps, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, id string) ([]transit.Departure, error) {
		var board apiDepartures
		u := httpx.URL(p.BaseURL, "/stops/"+url.PathEscape(id)+"/departures", q)
		if err := p.http.GetJSON(ctx, u, &board); err != nil {
			return nil, err
		}
		out := make([]transit.Departure, 0, len(board.Departures))
		for _, d := range board.Departures {
			out = append(out, toDeparture(d))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return &transit.Departures{Now: p.now(), Departures: deps}, nil
}

// RouteID stays empty on purpose: the feed's line ids ("de-vbb-11000000-tram-m4") are in no id
// space FindRoutes could return, so the app must filter by Line.
func toDeparture(d apiDeparture) transit.Departure {
	out := transit.Departure{
		Headsign:  xutil.Str(d.Direction),
		TripID:    d.TripID,
		Platform:  xutil.Str(d.Platform),
		Live:      d.Delay != nil,
		Cancelled: d.Cancelled != nil && *d.Cancelled,
	}
	if d.Platform == nil {
		out.Platform = xutil.Str(d.PlannedPlatform)
	}
	if d.Line != nil {
		out.Line = d.Line.Name
	}
	if d.Stop != nil {
		out.StopID = d.Stop.ID
	}
	out.Scheduled = xutil.ParseRFC3339(xutil.Str(d.PlannedWhen))
	if d.When == nil {
		// A null `when` means the trip is cancelled at this stop.
		out.Cancelled = true
		out.At = out.Scheduled
		out.Live = false
	} else {
		out.At = xutil.ParseRFC3339(*d.When)
		if out.At.IsZero() {
			out.At = out.Scheduled
		}
	}
	if !out.Live && !out.Scheduled.IsZero() {
		// Without realtime data the timetable time is the only truth, even
		// on the rare row where `when` drifts from `plannedWhen`.
		out.At = out.Scheduled
	}
	return out
}
