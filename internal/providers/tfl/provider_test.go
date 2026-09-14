package tfl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// fixtureNow is pinned just before the earliest expectedArrival in
// testdata/arrivals-940GZZLUBNK.json (2026-09-13T18:45:36Z).
var fixtureNow = time.Date(2026, 9, 13, 18, 45, 20, 0, time.UTC)

var fixtures = map[string]string{
	// TfL's search is case-insensitive; the probe asks for "Central".
	"/Line/Search/central":                  "line-search-central.json",
	"/Line/Search/Central":                  "line-search-central.json",
	"/Line/Search/":                         "line-search-empty.json",
	"/Line/central/Route/Sequence/inbound":  "route-central-inbound.json",
	"/Line/central/Route/Sequence/outbound": "route-central-outbound.json",
	"/StopPoint/Search/Bank":                "stoppoint-search-bank.json",
	"/StopPoint/HUBBAN":                     "hub-hubban.json",
	"/StopPoint/940GZZLUBNK/Arrivals":       "arrivals-940GZZLUBNK.json",
}

func testDeps(t *testing.T) registry.Deps {
	t.Helper()
	return registry.Deps{
		Key:   func(string) string { return "" },
		Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }},
		Now:   func() time.Time { return fixtureNow },
	}
}

func newTestProvider(t *testing.T, srv *httptest.Server, d registry.Deps) *Provider {
	t.Helper()
	p, ok := New(d).(*Provider)
	if !ok {
		t.Fatalf("New returned %T, want *Provider", New(d))
	}
	p.BaseURL = srv.URL
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv, testDeps(t))
	transittest.Conform(t, p, transittest.Case{
		Route:    "Central",
		Query:    "bank",
		WantStop: "Bank",
		Now:      fixtureNow,
	})
}

func TestRegistered(t *testing.T) {
	for _, id := range []string{"london", "tfl", "LONDON"} {
		if _, ok := registry.Lookup(id); !ok {
			t.Errorf("registry.Lookup(%q) found nothing", id)
		}
	}
}

func TestFindRoutes(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv, testDeps(t))
	ctx := context.Background()

	got, err := p.FindRoutes(ctx, "central")
	if err != nil {
		t.Fatalf("FindRoutes: %v", err)
	}
	// "Grand Central" (national-rail) also comes back from TfL's fuzzy
	// line search and must be dropped.
	if len(got) != 1 {
		t.Fatalf("FindRoutes returned %+v, want just the Central line", got)
	}
	if got[0].ID != "central" || got[0].ShortName != "Central" || got[0].Mode != "tube" {
		t.Errorf("route = %+v", got[0])
	}

	_, err = p.FindRoutes(ctx, "zz-no-such-route-zz")
	var ure *transit.UnknownRouteError
	if !errors.As(err, &ure) {
		t.Fatalf("FindRoutes(bogus) error = %v (%T), want *UnknownRouteError", err, err)
	}
	if !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("error does not wrap ErrUnknownRoute")
	}
}

func TestFindRoutesSuggestions(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/Line/Search/": "line-search-central.json"})
	p := newTestProvider(t, srv, testDeps(t))
	_, err := p.FindRoutes(context.Background(), "centrall")
	var ure *transit.UnknownRouteError
	if !errors.As(err, &ure) {
		t.Fatalf("error = %v (%T), want *UnknownRouteError", err, err)
	}
	want := []string{"Central", "Grand Central"}
	if strings.Join(ure.Suggestions, ",") != strings.Join(want, ",") {
		t.Errorf("suggestions = %v, want %v", ure.Suggestions, want)
	}
}

func TestRouteStops(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv, testDeps(t))
	ps, err := p.RouteStops(context.Background(), transit.Route{ID: "central", ShortName: "Central"})
	if err != nil {
		t.Fatalf("RouteStops: %v", err)
	}
	if len(ps) != 4 {
		t.Fatalf("got %d patterns, want 4", len(ps))
	}
	var dirs, heads []string
	for _, pat := range ps {
		dirs = append(dirs, pat.DirectionID)
		heads = append(heads, pat.Headsign)
		if len(pat.Stops) == 0 {
			t.Fatalf("pattern %+v has no stops", pat)
		}
	}
	if got, want := strings.Join(dirs, ","), "inbound,inbound,outbound,outbound"; got != want {
		t.Errorf("directions = %q, want %q", got, want)
	}
	if got, want := strings.Join(heads, "|"), "West Ruislip|Ealing Broadway|Epping|Hainault"; got != want {
		t.Errorf("headsigns = %q, want %q", got, want)
	}
	// Stop names keep their suffixes: matching is substring based.
	var bank *transit.Stop
	for i, s := range ps[0].Stops {
		if s.ID == "940GZZLUBNK" {
			bank = &ps[0].Stops[i]
		}
	}
	if bank == nil {
		t.Fatalf("Bank is missing from the inbound pattern")
	}
	if bank.Name != "Bank Underground Station" {
		t.Errorf("stop name = %q, want the unstripped name", bank.Name)
	}
	if bank.Lat == 0 || bank.Lon == 0 {
		t.Errorf("stop %+v has no coordinates", *bank)
	}
}

func TestRouteStopsOneDirectionOnly(t *testing.T) {
	// Outbound-only lines 404 on the other direction; one good direction
	// is enough.
	srv := transittest.Serve(t, map[string]string{
		"/Line/central/Route/Sequence/outbound": "route-central-outbound.json",
	})
	p := newTestProvider(t, srv, testDeps(t))
	ps, err := p.RouteStops(context.Background(), transit.Route{ID: "central"})
	if err != nil {
		t.Fatalf("RouteStops: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d patterns, want 2", len(ps))
	}
	empty := transittest.Serve(t, nil)
	p2 := newTestProvider(t, empty, testDeps(t))
	if _, err := p2.RouteStops(context.Background(), transit.Route{ID: "central"}); !errors.Is(err, transit.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestRouteStopsFallsBackToBranches(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "route-central-inbound.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(src), `"orderedLineRoutes"`, `"unusedLineRoutes"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, "x.json"))
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(t, srv, testDeps(t))
	ps, err := p.RouteStops(context.Background(), transit.Route{ID: "central"})
	if err != nil {
		t.Fatalf("RouteStops: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d patterns, want 2", len(ps))
	}
	last := ps[0].Stops[len(ps[0].Stops)-1]
	if ps[0].Headsign != stripStationSuffix(last.Name) {
		t.Errorf("headsign = %q, want the last stop %q", ps[0].Headsign, last.Name)
	}
}

func TestSearchStopsExpandsHubs(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	d := testDeps(t)
	p := newTestProvider(t, srv.Server, d)
	ctx := context.Background()

	stops, err := p.SearchStops(ctx, "Bank")
	if err != nil {
		t.Fatalf("SearchStops: %v", err)
	}
	byID := map[string]transit.Stop{}
	index := map[string]int{}
	for i, s := range stops {
		byID[s.ID] = s
		index[s.ID] = i
		if strings.HasPrefix(s.ID, "HUB") {
			t.Errorf("hub id %q leaked into the results", s.ID)
		}
	}
	// HUBBAN expands to its two stations plus the bus stops nested a
	// level deeper inside a NaptanOnstreetBusCoachStopCluster.
	for _, want := range []string{"940GZZLUBNK", "940GZZDLBNK", "490000013D", "490000013E"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("hub child %s is missing from %v", want, byID)
		}
	}
	// The cluster itself answers no arrivals, so it must not be listed.
	if _, ok := byID["490G000556"]; ok {
		t.Errorf("stop cluster 490G000556 should not be a result")
	}
	if got := byID["940GZZLUBNK"]; got.Name != "Bank Underground Station" || got.ParentID != "HUBBAN" {
		t.Errorf("hub child = %+v", got)
	}
	// Stations rank ahead of the kerbside bus stops named after them, so
	// callers that probe only the first few candidates still see them.
	if index["940GZZLUBNK"] > index["490000013D"] {
		t.Errorf("bus stop 490000013D (%d) sorts before the station 940GZZLUBNK (%d)",
			index["490000013D"], index["940GZZLUBNK"])
	}
	if _, ok := byID["940GZZLUEMB"]; !ok {
		t.Errorf("non-hub match 940GZZLUEMB is missing")
	}

	if n := srv.Hits("hub-hubban.json"); n != 1 {
		t.Fatalf("hub fetched %d times, want 1", n)
	}
	if _, err := p.SearchStops(ctx, "Bank"); err != nil {
		t.Fatalf("SearchStops again: %v", err)
	}
	if n := srv.Hits("hub-hubban.json"); n != 1 {
		t.Errorf("hub expansion was not cached: %d fetches", n)
	}
}

func TestDepartures(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv, testDeps(t))
	deps, err := p.Departures(context.Background(), []string{"940GZZLUBNK", "940GZZLUBNK"}, nil, 90*time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v, want %v", deps.Now, fixtureNow)
	}
	// The duplicate stop id must be fetched once, not twice.
	if len(deps.Departures) != 8 {
		t.Fatalf("got %d departures, want 8", len(deps.Departures))
	}
	first := deps.Departures[0]
	want := transit.Departure{
		At:          time.Date(2026, 9, 13, 18, 45, 36, 0, time.UTC),
		Live:        true,
		Platform:    "Eastbound - Platform 6",
		Headsign:    "Epping",
		DirectionID: "outbound",
		Line:        "Central",
		RouteID:     "central",
		TripID:      "024|central|2026-09-13T18:45:36Z",
		StopID:      "940GZZLUBNK",
	}
	if !first.At.Equal(want.At) || first.Scheduled != want.Scheduled || first.Live != want.Live ||
		first.Cancelled || first.Platform != want.Platform || first.Headsign != want.Headsign ||
		first.DirectionID != want.DirectionID || first.Line != want.Line || first.RouteID != want.RouteID ||
		first.TripID != want.TripID || first.StopID != want.StopID {
		t.Errorf("departure =\n%+v\nwant\n%+v", first, want)
	}
	for i := 1; i < len(deps.Departures); i++ {
		if deps.Departures[i].At.Before(deps.Departures[i-1].At) {
			t.Fatalf("departures are not sorted at %d", i)
		}
	}
	short, err := p.Departures(context.Background(), []string{"940GZZLUBNK"}, nil, time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if len(short.Departures) != 2 {
		t.Errorf("got %d departures in a 1m window, want 2", len(short.Departures))
	}
}

func TestDeparturesNoStops(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv, testDeps(t))
	deps, err := p.Departures(context.Background(), nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if len(deps.Departures) != 0 || deps.Now.IsZero() {
		t.Errorf("got %+v, want an empty board with a clock", deps)
	}
}

func TestToDeparture(t *testing.T) {
	now := fixtureNow
	tests := []struct {
		name     string
		in       arrival
		wantAt   time.Time
		wantHead string
		wantStop string
	}{
		{
			name:     "tube arrival strips the station suffix",
			in:       arrival{NaptanID: "940GZZLUBNK", DestinationName: "Hainault Underground Station", Towards: "Hainault via Newbury Park", ExpectedArrival: "2026-09-13T19:00:06Z"},
			wantAt:   time.Date(2026, 9, 13, 19, 0, 6, 0, time.UTC),
			wantHead: "Hainault",
			wantStop: "940GZZLUBNK",
		},
		{
			name:     "bus destination is kept verbatim",
			in:       arrival{NaptanID: "490000013D", DestinationName: "Ilford, Hainault Street", Towards: "Aldgate Or Liverpool Street", ExpectedArrival: "2026-09-13T19:13:04Z"},
			wantAt:   time.Date(2026, 9, 13, 19, 13, 4, 0, time.UTC),
			wantHead: "Ilford, Hainault Street",
			wantStop: "490000013D",
		},
		{
			name:     "no destination falls back to towards",
			in:       arrival{ID: "1276558037", DestinationName: "", Towards: "Aldgate Or Liverpool Street", ExpectedArrival: "2026-09-13T19:13:04Z"},
			wantAt:   time.Date(2026, 9, 13, 19, 13, 4, 0, time.UTC),
			wantHead: "Aldgate Or Liverpool Street",
			wantStop: "1276558037",
		},
		{
			name:     "unparseable time falls back to timeToStation",
			in:       arrival{NaptanID: "940GZZLUBNK", DestinationName: "Epping Rail Station", ExpectedArrival: "", TimeToStation: 120},
			wantAt:   now.Add(2 * time.Minute),
			wantHead: "Epping",
			wantStop: "940GZZLUBNK",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toDeparture(tt.in, now)
			if !got.At.Equal(tt.wantAt) {
				t.Errorf("At = %v, want %v", got.At, tt.wantAt)
			}
			if got.Headsign != tt.wantHead {
				t.Errorf("Headsign = %q, want %q", got.Headsign, tt.wantHead)
			}
			if got.StopID != tt.wantStop {
				t.Errorf("StopID = %q, want %q", got.StopID, tt.wantStop)
			}
			if !got.Live || got.Cancelled || !got.Scheduled.IsZero() {
				t.Errorf("got %+v, want a live, uncancelled, unscheduled entry", got)
			}
		})
	}
}

func TestStripStationSuffix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hainault Underground Station", "Hainault"},
		{"Bank DLR Station", "Bank"},
		{"Stratford Rail Station", "Stratford"},
		{"Wimbledon Tram Stop", "Wimbledon"},
		{"Hounslow Bus Station", "Hounslow"},
		{"Abbey Wood Elizabeth line Station", "Abbey Wood"},
		{"Highbury & Islington Overground Station", "Highbury & Islington"},
		{"Paddington Station", "Paddington"},
		{"Ilford, Hainault Street", "Ilford, Hainault Street"},
		{"Station", "Station"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripStationSuffix(tt.in); got != tt.want {
			t.Errorf("stripStationSuffix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHeadsign(t *testing.T) {
	stops := []transit.Stop{{Name: "Bank Underground Station"}, {Name: "Epping Underground Station"}}
	tests := []struct{ in, want string }{
		{"Epping  &harr;  West Ruislip ", "West Ruislip"},
		{"West Ruislip  &harr;  Hainault  via Newbury Park", "Hainault via Newbury Park"},
		{"Holborn Circus &harr;  Hainault Street", "Hainault Street"},
		{"Ealing Broadway ↔ Epping", "Epping"},
		{"Stratford → Canary Wharf", "Canary Wharf"},
		{"Victoria to Brixton Underground Station", "Brixton"},
		{"", "Epping"}, // falls back to the last stop
	}
	for _, tt := range tests {
		if got := headsign(tt.in, stops); got != tt.want {
			t.Errorf("headsign(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAPIKeyIsSentAndRedacted(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("app_key")
		b, err := os.ReadFile(filepath.Join("testdata", "line-search-central.json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	d := testDeps(t)
	d.Key = func(env string) string {
		if env != "ETA_TFL_API_KEY" {
			t.Errorf("asked for key %q", env)
		}
		return "s3cret"
	}
	p := newTestProvider(t, srv, d)
	if _, err := p.FindRoutes(context.Background(), "Central"); err != nil {
		t.Fatalf("FindRoutes: %v", err)
	}
	if gotKey != "s3cret" {
		t.Errorf("app_key = %q, want the configured key", gotKey)
	}
	var redacted bool
	for _, s := range p.http.Redact {
		if s == "s3cret" {
			redacted = true
		}
	}
	if !redacted {
		t.Errorf("key is not in Redact: %v", p.http.Redact)
	}
	gotKey = "unset"
	p2 := newTestProvider(t, srv, testDeps(t))
	if _, err := p2.FindRoutes(context.Background(), "Central"); err != nil {
		t.Fatalf("FindRoutes: %v", err)
	}
	if gotKey != "" {
		t.Errorf("app_key = %q, want none", gotKey)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Invalid app key"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(t, srv, testDeps(t))
	ctx := context.Background()
	if _, err := p.FindRoutes(ctx, "Central"); !errors.Is(err, transit.ErrUnauthorized) {
		t.Errorf("FindRoutes error = %v, want ErrUnauthorized", err)
	}
	if _, err := p.Departures(ctx, []string{"940GZZLUBNK"}, nil, time.Hour); !errors.Is(err, transit.ErrUnauthorized) {
		t.Errorf("Departures error = %v, want ErrUnauthorized", err)
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{
		Key:   func(env string) string { return os.Getenv(env) },
		Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour},
		Now:   time.Now,
	})
	transittest.Live(t, p, transittest.Case{
		Route:    info.Probe.Route,
		Query:    info.Probe.Query,
		WantStop: "Bank",
	})
}
