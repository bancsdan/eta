// Package transit is the city-independent domain model: stops, routes,
// departures and the Provider interfaces every city package implements.
package transit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ID is provider-native and must be accepted by the same provider's
// Departures. ParentID names the station/hub of a child platform.
type Stop struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID string  `json:"parentId,omitempty"`
	Lat      float64 `json:"lat,omitempty"`
	Lon      float64 `json:"lon,omitempty"`
}

type Route struct {
	ID        string `json:"id"`
	ShortName string `json:"shortName"`
	LongName  string `json:"longName,omitempty"`
	Mode      string `json:"mode,omitempty"` // bus, tram, metro, rail, ferry or ""
}

// DirectionID is "0"/"1", "inbound"/"outbound" or "" when unknown.
type Pattern struct {
	DirectionID string `json:"directionId"`
	Headsign    string `json:"headsign"`
	Stops       []Stop `json:"stops"`
}

type Departure struct {
	At          time.Time // best known time: the live prediction when Live, else Scheduled
	Scheduled   time.Time // timetable time; zero when the API gives none
	Live        bool      // At comes from a real-time prediction
	Cancelled   bool
	Platform    string
	Headsign    string
	DirectionID string
	Line        string
	RouteID     string // native route id when the feed gives one; preferred for filtering
	TripID      string // for de-duplication across parent/child stops; may be ""
	StopID      string // platform the entry was observed at
}

// Now is the server's clock when the API reports one.
type Departures struct {
	Now        time.Time
	Departures []Departure
}

type KeyReq int

const (
	KeyNone     KeyReq = iota // works without a key
	KeyOptional               // works without, a key raises rate limits
	KeyRequired               // every network call needs it
)

// Env is named after the data provider that issued the key,
// ETA_<PROVIDER>_API_KEY, or ETA_<PROVIDER>_<LABEL>_API_KEY when a provider
// hands out several.
type KeySpec struct {
	Env       string
	Label     string // "", or "train"/"bus" for multi-key cities
	Req       KeyReq
	SignupURL string
}

func (k KeySpec) ConfigName() string {
	s := strings.TrimSuffix(strings.TrimPrefix(k.Env, "ETA_"), "_API_KEY")
	return strings.ToLower(s)
}

type Info struct {
	ID       string
	Name     string
	Provider string // data source id, "entur"; one provider can serve many cities
	TZ       string // IANA zone, "Europe/Berlin"
	Keys     []KeySpec
	Realtime bool // false when every departure is schedule-only
	Notes    string
	// Probe is a route and stop known to exist, used by `eta cities --check`
	// and the live conformance test.
	Probe Probe
}

type Probe struct {
	Route string
	Query string
}

// routes may be empty; providers that can filter server-side should, the app
// filters again client-side.
type Provider interface {
	Info() Info
	Departures(ctx context.Context, stopIDs []string, routes []Route, window time.Duration) (*Departures, error)
}

type RouteLister interface {
	Provider
	// A bus and a tram may share a number, so several routes can come back.
	// Nothing matching is *UnknownRouteError.
	FindRoutes(ctx context.Context, short string) ([]Route, error)
	RouteStops(ctx context.Context, r Route) ([]Pattern, error)
}

type StopSearcher interface {
	Provider
	SearchStops(ctx context.Context, query string) ([]Stop, error)
}

// ColdStarter marks providers whose first run downloads bulk data (a GTFS
// zip, a full stop list) and therefore needs a longer time budget.
type ColdStarter interface {
	Cold() bool
}

var (
	ErrNoKey        = errors.New("missing API key")
	ErrUnauthorized = errors.New("API key rejected")
	ErrNotFound     = errors.New("not found")
	ErrRateLimited  = errors.New("rate limited")
	ErrUnknownRoute = errors.New("unknown route")
)

type UnknownRouteError struct {
	Route       string
	Suggestions []string
}

func (e *UnknownRouteError) Error() string {
	msg := fmt.Sprintf("unknown route %q", e.Route)
	if len(e.Suggestions) > 0 {
		msg += " (did you mean: " + strings.Join(e.Suggestions, ", ") + ")"
	}
	return msg
}

func (e *UnknownRouteError) Unwrap() error { return ErrUnknownRoute }

func (i Info) Location() *time.Location {
	if loc, err := time.LoadLocation(i.TZ); err == nil {
		return loc
	}
	return time.UTC
}

func SortDepartures(ds []Departure) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].At.Before(ds[j].At) })
}

// A stop whose fetch fails is skipped; only when every stop fails is the
// first error returned.
func Gather(ctx context.Context, stopIDs []string, fetch func(ctx context.Context, stopID string) ([]Departure, error)) ([]Departure, error) {
	ids := dedupe(stopIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	boards := make([][]Departure, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			boards[i], errs[i] = fetch(ctx, id)
		}(i, id)
	}
	wg.Wait()
	var out []Departure
	var firstErr error
	failed := 0
	for i := range ids {
		if errs[i] != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("stop %s: %w", ids[i], errs[i])
			}
			continue
		}
		out = append(out, boards[i]...)
	}
	if failed == len(ids) {
		return nil, firstErr
	}
	SortDepartures(out)
	return out, nil
}

const MaxSuggestions = 8

func UnknownRoute(route string, suggestions []string) *UnknownRouteError {
	names := dedupe(suggestions)
	if len(names) > MaxSuggestions {
		names = names[:MaxSuggestions]
	}
	return &UnknownRouteError{Route: route, Suggestions: names}
}

func ModeFromGTFS(routeType int) string {
	switch routeType {
	case 0, 5, 12:
		return "tram"
	case 1:
		return "metro"
	case 2:
		return "rail"
	case 3, 11:
		return "bus"
	case 4:
		return "ferry"
	case 6, 7:
		return "cableway"
	}
	return ""
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
