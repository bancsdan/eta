package bkk

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// The fixtures under testdata are hand-written from the FUTÁR OpenAPI spec
// (https://opendata.bkk.hu/docs/futar-openapi.yaml) and the recorded shapes
// the GoKK project's fake server replayed; the API needs a key, so they
// could not be captured with curl here. Every time in them is relative to
// fixtureNow.
const testKey = "k3y"

var fixtureNow = time.Unix(1789252200, 0) // 2026-09-12T22:30:00Z

var fixtures = map[string]string{
	"/search?includeReferences=stops":   "search-stops.json",
	"/search":                           "search.json",
	"/route-details":                    "route-details.json",
	"/arrivals-and-departures-for-stop": "arrivals.json",
}

func newTestProvider(base, key string) *Provider {
	p := newProvider(registry.Deps{
		Key: func(string) string { return key },
		Now: func() time.Time { return fixtureNow },
	})
	p.BaseURL = base
	return p
}

func apiServer(t *testing.T, files map[string]string) *transittest.Server {
	t.Helper()
	srv := transittest.NewServer(t, files)
	srv.Auth = func(r *http.Request) bool { return r.URL.Query().Get("key") == testKey }
	return srv
}

func lastQuery(srv *transittest.Server, path string) url.Values {
	reqs := srv.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].URL.Path == path {
			return reqs[i].URL.Query()
		}
	}
	return nil
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(srv.URL, testKey)
	transittest.Conform(t, p, transittest.Case{
		Route:     "155",
		Query:     "zugligeti",
		WantStop:  "Zugligeti",
		WantLines: []string{"155"},
		Now:       fixtureNow,
	})
}

func TestFindRoutes(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(srv.URL, testKey)
	ctx := context.Background()

	got, err := p.FindRoutes(ctx, "155")
	if err != nil {
		t.Fatalf("FindRoutes: %v", err)
	}
	want := []transit.Route{{ID: "BKK_1550", ShortName: "155", LongName: "Zugliget / Széll Kálmán tér M", Mode: "bus"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("FindRoutes = %+v, want %+v", got, want)
	}

	// The search endpoint is a loose text match: 155A must not answer "155".
	_, err = p.FindRoutes(ctx, "999")
	var ure *transit.UnknownRouteError
	if !errors.As(err, &ure) {
		t.Fatalf("FindRoutes(999) error = %v (%T), want *UnknownRouteError", err, err)
	}
	if !errors.Is(err, transit.ErrUnknownRoute) {
		t.Error("UnknownRouteError must unwrap to ErrUnknownRoute")
	}
	if strings.Join(ure.Suggestions, ",") != "155,155A" {
		t.Errorf("suggestions = %v, want [155 155A]", ure.Suggestions)
	}
}

func TestRouteStops(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(srv.URL, testKey)
	pats, err := p.RouteStops(context.Background(), transit.Route{ID: "BKK_1550", ShortName: "155"})
	if err != nil {
		t.Fatalf("RouteStops: %v", err)
	}
	if len(pats) != 2 {
		t.Fatalf("got %d patterns, want 2", len(pats))
	}
	if pats[0].DirectionID != "1" || pats[0].Headsign != "Széll Kálmán tér M" {
		t.Errorf("pattern 0 = %+v", pats[0])
	}
	var names []string
	for _, s := range pats[1].Stops {
		names = append(names, s.Name)
	}
	if strings.Join(names, " | ") != "Széll Kálmán tér M | Zugligeti út | Zugliget, Libegő" {
		t.Errorf("direction 0 stops = %v", names)
	}
	if pats[1].Stops[0].ID != "BKK_F02001" || pats[1].Stops[0].Lat == 0 {
		t.Errorf("stop not mapped: %+v", pats[1].Stops[0])
	}
}

func TestDepartures(t *testing.T) {
	srv := apiServer(t, fixtures)
	p := newTestProvider(srv.URL, testKey)
	routes := []transit.Route{{ID: "BKK_1550", ShortName: "155"}}

	deps, err := p.Departures(context.Background(), []string{"BKK_F02004", "BKK_F02003"}, routes, 90*time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v, want the envelope's currentTime %v", deps.Now, fixtureNow)
	}

	type row struct {
		trip string
		at   time.Duration // from fixtureNow
		live bool
	}
	want := []row{
		{"BKK_T1", 3*time.Minute + 12*time.Second, true},
		{"BKK_T5", 5 * time.Minute, false}, // predictionScheduled: schedule-derived
		{"BKK_T2", 7 * time.Minute, true},
		{"BKK_T6", 12*time.Minute + 24*time.Second, true},
		{"BKK_T3", 16 * time.Minute, true},
		{"BKK_T4", 25 * time.Minute, false}, // no prediction at all
	}
	if len(deps.Departures) != len(want) {
		t.Fatalf("got %d departures, want %d: %+v", len(deps.Departures), len(want), deps.Departures)
	}
	for i, w := range want {
		d := deps.Departures[i]
		if d.TripID != w.trip || !d.At.Equal(fixtureNow.Add(w.at)) || d.Live != w.live {
			t.Errorf("departure %d = %s at %v live=%v, want %s at +%v live=%v",
				i, d.TripID, d.At.Sub(fixtureNow), d.Live, w.trip, w.at, w.live)
		}
		if d.Line != "155" || d.RouteID != "BKK_1550" {
			t.Errorf("departure %d: line %q route %q", i, d.Line, d.RouteID)
		}
		if !d.Live && !d.At.Equal(d.Scheduled) {
			t.Errorf("departure %d: not live but At %v != Scheduled %v", i, d.At, d.Scheduled)
		}
	}
	first := deps.Departures[0]
	if first.Headsign != "Zugliget" || first.DirectionID != "0" || first.StopID != "BKK_F02004" {
		t.Errorf("trip references not mapped: %+v", first)
	}

	q := lastQuery(srv, "/arrivals-and-departures-for-stop")
	checks := map[string]string{
		"minutesBefore":     "0",
		"minutesAfter":      "90",
		"limit":             "200",
		"onlyDepartures":    "false",
		"includeReferences": "trips,stops,routes",
		"includeRouteId":    "BKK_1550",
		"key":               testKey,
		"version":           "4",
		"appVersion":        "eta-cli",
	}
	for k, v := range checks {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if got := strings.Join(q["stopId"], ","); got != "BKK_F02004,BKK_F02003" {
		t.Errorf("stopId = %q, want both stops as repeated params", got)
	}
}

func TestDeparturesNoStops(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:0", testKey) // must not be dialled
	deps, err := p.Departures(context.Background(), nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("empty stop ids: %v", err)
	}
	if len(deps.Departures) != 0 || !deps.Now.Equal(fixtureNow) {
		t.Errorf("want an empty board clocked at Now, got %+v", deps)
	}
}

func TestMissingKey(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(srv.URL, "")
	ctx := context.Background()
	calls := map[string]error{}
	_, calls["FindRoutes"] = p.FindRoutes(ctx, "155")
	_, calls["RouteStops"] = p.RouteStops(ctx, transit.Route{ID: "BKK_1550"})
	_, calls["Departures"] = p.Departures(ctx, []string{"BKK_F02003"}, nil, time.Hour)
	for name, err := range calls {
		if !errors.Is(err, transit.ErrNoKey) {
			t.Errorf("%s with no key = %v, want ErrNoKey", name, err)
		}
		if !strings.Contains(err.Error(), keyEnv) {
			t.Errorf("%s error should name %s: %v", name, keyEnv, err)
		}
	}
}

func TestWrongKey(t *testing.T) {
	srv := apiServer(t, fixtures)
	p := newTestProvider(srv.URL, "wrong")
	if _, err := p.FindRoutes(context.Background(), "155"); !errors.Is(err, transit.ErrUnauthorized) {
		t.Fatalf("wrong key = %v, want ErrUnauthorized", err)
	}
}

func TestKeyRedacted(t *testing.T) {
	p := newTestProvider("http://example.invalid", "s3cret")
	if len(p.c.http.Redact) == 0 || p.c.http.Redact[0] != "s3cret" {
		t.Fatalf("the key must be redacted in ETA_DEBUG output, got %v", p.c.http.Redact)
	}
}

func TestEnvelopeError(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/route-details": "not-found.json"})
	p := newTestProvider(srv.URL, testKey)
	_, err := p.RouteStops(context.Background(), transit.Route{ID: "BKK_9999"})
	if !errors.Is(err, transit.ErrNotFound) {
		t.Fatalf("envelope code 404 = %v, want ErrNotFound", err)
	}
}

func TestSearchStops(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(srv.URL, testKey)
	stops, err := p.SearchStops(context.Background(), "zugligeti")
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 2 || stops[0].ID != "BKK_F02004" || stops[0].Name != "Zugligeti út" || stops[0].Lat == 0 {
		t.Errorf("stops = %+v", stops)
	}
}

func TestInfo(t *testing.T) {
	i := newTestProvider("", testKey).Info()
	if i.ID != "budapest" || i.TZ != "Europe/Budapest" || !i.Realtime {
		t.Fatalf("info = %+v", i)
	}
	if len(i.Keys) != 1 || i.Keys[0].Req != transit.KeyRequired || i.Keys[0].ConfigName() != "bkk" {
		t.Fatalf("keys = %+v", i.Keys)
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{
		Key:  func(env string) string { return strings.TrimSpace(os.Getenv(env)) },
		HTTP: &http.Client{Timeout: 20 * time.Second},
		Now:  time.Now,
	})
	transittest.Live(t, p, transittest.Case{Route: info.Probe.Route, Query: info.Probe.Query})
}
