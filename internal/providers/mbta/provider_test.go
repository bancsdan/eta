package mbta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures in testdata are real MBTA v3 responses captured on 2026-09-13,
// trimmed to a handful of entries (the "included" arrays were trimmed to
// match). fixtureNow sits just before the earliest time in them.
var fixtureNow = time.Date(2026, 9, 13, 14, 46, 0, 0, time.FixedZone("EDT", -4*60*60))

var fixtures = map[string]string{
	"/stops?filter[location_type]=1": "stations.json",
	"/routes":                        "routes.json",
	"/stops?filter[route]=Red&filter[direction_id]=0": "stops-red-0.json",
	"/stops?filter[route]=Red&filter[direction_id]=1": "stops-red-1.json",
	"/predictions?filter[stop]=place-pktrm":           "predictions-red-pktrm.json",
	"/schedules?filter[stop]=place-pktrm":             "schedules-red-pktrm.json",
}

func newTestProvider(t *testing.T, baseURL string) *Provider {
	t.Helper()
	p := New(registry.Deps{
		Key:  func(string) string { return "" },
		HTTP: baseURL2Client(),
		Now:  func() time.Time { return fixtureNow },
	}).(*Provider)
	p.BaseURL = baseURL
	return p
}

func baseURL2Client() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv.URL)
	transittest.Conform(t, p, transittest.Case{
		Route:     "Red",
		Query:     "park street",
		WantStop:  "Park Street",
		WantLines: []string{"Red"},
		Now:       fixtureNow,
	})
}

func TestFindRoutesTiers(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv.URL)
	ctx := context.Background()

	cases := []struct {
		query     string
		wantIDs   []string
		wantShort string
		wantMode  string
	}{
		{"1", []string{"1"}, "1", "bus"},              // bus short_name
		{"E", []string{"Green-E"}, "E", "tram"},       // Green Line branch short_name
		{"Red", []string{"Red"}, "Red", "metro"},      // id, short_name is ""
		{"green-e", []string{"Green-E"}, "E", "tram"}, // id, normalized
		{"cr-fitchburg", []string{"CR-Fitchburg"}, "CR-Fitchburg", "rail"},
		{"Orange Line", []string{"Orange"}, "Orange", "metro"}, // long_name
		{"orange", []string{"Orange"}, "Orange", "metro"},      // long_name minus " Line"
		{"Hingham Ferry", []string{"Boat-F1"}, "Boat-F1", "ferry"},
	}
	for _, c := range cases {
		got, err := p.FindRoutes(ctx, c.query)
		if err != nil {
			t.Errorf("FindRoutes(%q): %v", c.query, err)
			continue
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		if strings.Join(ids, ",") != strings.Join(c.wantIDs, ",") {
			t.Errorf("FindRoutes(%q) = %v, want %v", c.query, ids, c.wantIDs)
			continue
		}
		if got[0].ShortName != c.wantShort {
			t.Errorf("FindRoutes(%q).ShortName = %q, want %q", c.query, got[0].ShortName, c.wantShort)
		}
		if got[0].Mode != c.wantMode {
			t.Errorf("FindRoutes(%q).Mode = %q, want %q", c.query, got[0].Mode, c.wantMode)
		}
	}

	_, err := p.FindRoutes(ctx, "nope")
	var ure *transit.UnknownRouteError
	if !errors.As(err, &ure) {
		t.Fatalf("FindRoutes on a bogus route = %v, want *transit.UnknownRouteError", err)
	}
	if !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("error does not unwrap to ErrUnknownRoute")
	}
	if len(ure.Suggestions) == 0 || len(ure.Suggestions) > 8 {
		t.Errorf("got %d suggestions, want 1..8", len(ure.Suggestions))
	}
}

func TestRouteStops(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv.URL)
	ps, err := p.RouteStops(context.Background(), transit.Route{ID: "Red", ShortName: "Red"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d patterns, want 2", len(ps))
	}
	if ps[0].DirectionID != "0" || ps[0].Headsign != "Ashmont/Braintree" {
		t.Errorf("pattern 0 = %q/%q, want 0/Ashmont/Braintree", ps[0].DirectionID, ps[0].Headsign)
	}
	if ps[1].DirectionID != "1" || ps[1].Headsign != "Alewife" {
		t.Errorf("pattern 1 = %q/%q, want 1/Alewife", ps[1].DirectionID, ps[1].Headsign)
	}
	if ps[0].Stops[0].ID != "place-alfcl" || ps[0].Stops[0].Name != "Alewife" {
		t.Errorf("first stop = %+v, want place-alfcl/Alewife", ps[0].Stops[0])
	}
	// Direction 1 is the reverse of direction 0, i.e. travel order.
	if ps[1].Stops[0].ID != "place-brntn" {
		t.Errorf("direction 1 starts at %q, want place-brntn", ps[1].Stops[0].ID)
	}
}

func TestDeparturesMergesSchedules(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"place-pktrm"},
		[]transit.Route{{ID: "Red", ShortName: "Red"}}, 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v, want %v", deps.Now, fixtureNow)
	}
	// 6 predictions plus the one scheduled trip nobody predicts (77916368);
	// the other three schedule rows repeat predicted trips.
	if len(deps.Departures) != 7 {
		t.Fatalf("got %d departures, want 7", len(deps.Departures))
	}
	var live, sched int
	for _, d := range deps.Departures {
		if d.Live {
			live++
			if !d.Scheduled.IsZero() {
				t.Errorf("live departure %+v should have a zero Scheduled", d)
			}
		} else {
			sched++
			if !d.At.Equal(d.Scheduled) {
				t.Errorf("scheduled departure At %v != Scheduled %v", d.At, d.Scheduled)
			}
			if d.TripID != "77916368" {
				t.Errorf("unexpected schedule-only trip %q", d.TripID)
			}
		}
		if d.RouteID != "Red" || d.Line != "Red" {
			t.Errorf("departure %+v: want RouteID/Line Red", d)
		}
	}
	if live != 6 || sched != 1 {
		t.Errorf("live/scheduled = %d/%d, want 6/1", live, sched)
	}
	first := deps.Departures[0]
	if first.TripID != "77916473" || first.Headsign != "Alewife" || first.DirectionID != "1" {
		t.Errorf("first departure = %+v", first)
	}
	// place-pktrm is a parent station; predictions come back on its child
	// platforms, and the subway exposes a platform name rather than a code.
	if first.StopID != "70076" || first.Platform != "Alewife" {
		t.Errorf("first departure stop/platform = %q/%q, want 70076/Alewife", first.StopID, first.Platform)
	}
	for i := 1; i < len(deps.Departures); i++ {
		if deps.Departures[i].At.Before(deps.Departures[i-1].At) {
			t.Fatalf("departures are not sorted at index %d", i)
		}
	}
}

func TestDeparturesNoStops(t *testing.T) {
	p := newTestProvider(t, "http://127.0.0.1:1") // must not be dialled
	deps, err := p.Departures(context.Background(), nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("empty stop list should not error: %v", err)
	}
	if len(deps.Departures) != 0 || !deps.Now.Equal(fixtureNow) {
		t.Errorf("got %+v, want an empty board at fixtureNow", deps)
	}
}

type stub struct {
	body    map[string]string
	status  int
	queries map[string]string
}

func (s *stub) serve(t *testing.T) *Provider {
	t.Helper()
	s.queries = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.queries[r.URL.Path] = r.URL.RawQuery
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(`{"errors":[{"status":"403"}]}`))
			return
		}
		b, ok := s.body[r.URL.Path]
		if !ok {
			b = `{"data":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b))
	}))
	t.Cleanup(srv.Close)
	return newTestProvider(t, srv.URL)
}

const stubRoutes = `{"data":[{"id":"Red","attributes":{"short_name":"","long_name":"Red Line","type":1,` +
	`"direction_destinations":["Ashmont/Braintree","Alewife"]}}]}`

func TestDepartureMapping(t *testing.T) {
	pred := func(attrs string) string {
		return `{"data":[{"id":"p1","attributes":` + attrs + `,"relationships":{` +
			`"route":{"data":{"id":"Red","type":"route"}},` +
			`"stop":{"data":{"id":"70076","type":"stop"}},` +
			`"trip":{"data":{"id":"t1","type":"trip"}}}}],` +
			`"included":[{"id":"t1","type":"trip","attributes":{"headsign":"Alewife"}},` +
			`{"id":"70076","type":"stop","attributes":{"platform_code":"2","platform_name":"Alewife"},` +
			`"relationships":{"parent_station":{"data":{"id":"place-pktrm","type":"stop"}}}}]}`
	}
	const times = `"arrival_time":"2026-09-13T15:00:00-04:00","departure_time":"2026-09-13T15:01:00-04:00","direction_id":1`

	cases := []struct {
		name          string
		predictions   string
		wantCount     int
		wantCancelled bool
		wantAt        string
		wantPlatform  string
		wantDirection string
	}{
		{
			name:          "running trip uses departure_time",
			predictions:   pred(`{` + times + `,"schedule_relationship":null,"status":null}`),
			wantCount:     1,
			wantAt:        "2026-09-13T15:01:00-04:00",
			wantPlatform:  "2",
			wantDirection: "1",
		},
		{
			name:          "cancelled",
			predictions:   pred(`{` + times + `,"schedule_relationship":"CANCELLED"}`),
			wantCount:     1,
			wantCancelled: true,
			wantAt:        "2026-09-13T15:01:00-04:00",
			wantPlatform:  "2",
			wantDirection: "1",
		},
		{
			name:          "skipped counts as cancelled",
			predictions:   pred(`{` + times + `,"schedule_relationship":"SKIPPED"}`),
			wantCount:     1,
			wantCancelled: true,
			wantAt:        "2026-09-13T15:01:00-04:00",
			wantPlatform:  "2",
			wantDirection: "1",
		},
		{
			name: "null departure_time falls back to arrival_time",
			predictions: pred(`{"arrival_time":"2026-09-13T15:00:00-04:00","departure_time":null,` +
				`"direction_id":0,"schedule_relationship":null}`),
			wantCount:     1,
			wantAt:        "2026-09-13T15:00:00-04:00",
			wantPlatform:  "2",
			wantDirection: "0",
		},
		{
			name:        "both times null is skipped",
			predictions: pred(`{"arrival_time":null,"departure_time":null,"direction_id":null}`),
			wantCount:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &stub{body: map[string]string{"/routes": stubRoutes, "/predictions": c.predictions}}
			p := s.serve(t)
			deps, err := p.Departures(context.Background(), []string{"place-pktrm"}, nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(deps.Departures) != c.wantCount {
				t.Fatalf("got %d departures, want %d", len(deps.Departures), c.wantCount)
			}
			if c.wantCount == 0 {
				return
			}
			d := deps.Departures[0]
			if got := d.At.Format(time.RFC3339); got != c.wantAt {
				t.Errorf("At = %s, want %s", got, c.wantAt)
			}
			if d.Cancelled != c.wantCancelled {
				t.Errorf("Cancelled = %v, want %v", d.Cancelled, c.wantCancelled)
			}
			if d.Platform != c.wantPlatform {
				t.Errorf("Platform = %q, want %q", d.Platform, c.wantPlatform)
			}
			if d.DirectionID != c.wantDirection {
				t.Errorf("DirectionID = %q, want %q", d.DirectionID, c.wantDirection)
			}
			if !d.Live || !d.Scheduled.IsZero() {
				t.Errorf("predictions must be Live with a zero Scheduled, got %+v", d)
			}
			if d.Headsign != "Alewife" || d.TripID != "t1" || d.StopID != "70076" {
				t.Errorf("unexpected trip/stop mapping: %+v", d)
			}
			if d.Line != "Red" || d.RouteID != "Red" {
				t.Errorf("Line/RouteID = %q/%q, want Red/Red", d.Line, d.RouteID)
			}
		})
	}
}

func TestScheduleWindow(t *testing.T) {
	s := &stub{body: map[string]string{"/routes": stubRoutes}}
	p := s.serve(t)
	if _, err := p.Departures(context.Background(), []string{"place-pktrm"},
		[]transit.Route{{ID: "Red"}}, 90*time.Minute); err != nil {
		t.Fatal(err)
	}
	q := s.queries["/schedules"]
	for _, want := range []string{"filter[stop]=place-pktrm", "filter[route]=Red",
		"filter[min_time]=14%3A46", "filter[max_time]=16%3A16", "include=trip"} {
		if !strings.Contains(q, want) {
			t.Errorf("schedules query %q is missing %q", q, want)
		}
	}

	s2 := &stub{body: map[string]string{"/routes": stubRoutes}}
	p2 := s2.serve(t)
	if _, err := p2.Departures(context.Background(), []string{"place-pktrm"}, nil, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	if q := s2.queries["/schedules"]; !strings.Contains(q, "filter[max_time]=23%3A59") {
		t.Errorf("schedules query %q should cap max_time at 23:59 (percent-encoded)", q)
	}
	if q := s2.queries["/schedules"]; strings.Contains(q, "filter[route]") {
		t.Errorf("schedules query %q should omit the route filter when no routes are given", q)
	}
}

func TestUnauthorized(t *testing.T) {
	s := &stub{status: http.StatusForbidden}
	p := s.serve(t)
	_, err := p.FindRoutes(context.Background(), "Red")
	if !errors.Is(err, transit.ErrUnauthorized) {
		t.Fatalf("got %v, want transit.ErrUnauthorized", err)
	}
}

func TestAPIKeyHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(stubRoutes))
	}))
	t.Cleanup(srv.Close)
	p := New(registry.Deps{
		Key:  func(env string) string { return map[string]string{keyEnv: "secret"}[env] },
		HTTP: srv.Client(),
		Now:  func() time.Time { return fixtureNow },
	}).(*Provider)
	p.BaseURL = srv.URL
	if _, err := p.FindRoutes(context.Background(), "Red"); err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Errorf("x-api-key = %q, want %q", got, "secret")
	}
	found := false
	for _, r := range p.http.Redact {
		if r == "secret" {
			found = true
		}
	}
	if !found {
		t.Errorf("the API key should be registered for redaction")
	}
}

func TestInfo(t *testing.T) {
	p := New(registry.Deps{Key: func(string) string { return "" }}).(*Provider)
	i := p.Info()
	if i.ID != "boston" || i.Name != "Boston (MBTA)" || i.TZ != "America/New_York" {
		t.Errorf("unexpected Info: %+v", i)
	}
	if len(i.Keys) != 1 || i.Keys[0].Req != transit.KeyOptional || i.Keys[0].Env != "ETA_MBTA_API_KEY" {
		t.Errorf("unexpected Keys: %+v", i.Keys)
	}
	if !i.Realtime {
		t.Errorf("Realtime should be true")
	}
	if _, ok := transit.Provider(p).(transit.RouteLister); !ok {
		t.Errorf("boston must implement transit.RouteLister")
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{
		Key:  func(env string) string { return os.Getenv(env) },
		HTTP: &http.Client{Timeout: 20 * time.Second},
		Now:  time.Now,
	})
	transittest.Live(t, p, transittest.Case{
		Route:     info.Probe.Route,
		Query:     info.Probe.Query,
		WantStop:  "Park Street",
		WantLines: []string{"Red"},
	})
}

func TestSearchStopsMatchesStations(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL)
	p.cache = &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	ctx := context.Background()
	stops, err := p.SearchStops(ctx, "park st")
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 1 || stops[0].ID != "place-pktrm" || stops[0].Name != "Park Street" {
		t.Fatalf("stops = %+v", stops)
	}
	if stops, err = p.SearchStops(ctx, "harvrd"); err != nil || len(stops) == 0 || stops[0].Name != "Harvard" {
		t.Errorf("closest = %+v %v", stops, err)
	}
	if n := srv.Hits("stations.json"); n != 1 {
		t.Errorf("stations fetched %d times, want 1", n)
	}
}
