package entur

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// fixtureNow is one minute before the earliest aimedDepartureTime in
// testdata/departures-jernbanetorget.json (2026-09-13T20:46:00+02:00).
var fixtureNow = time.Date(2026, 9, 13, 20, 45, 0, 0, time.FixedZone("CEST", 2*60*60))

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// JourneyPlanner is a single POST path, so transittest.Serve cannot be used:
// requests are dispatched on the GraphQL operation in the body instead.
func serve(t *testing.T, captured *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ET-Client-Name") != clientName {
			http.Error(w, "missing ET-Client-Name", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if !strings.HasSuffix(r.URL.Path, "/autocomplete") {
				http.NotFound(w, r)
				return
			}
			name := "autocomplete-jernbanetorget.json"
			if q := strings.ToLower(r.URL.Query().Get("text")); strings.Contains(q, "grorud") {
				name = "autocomplete-grorud.json"
			}
			_, _ = w.Write(read(t, name))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if captured != nil {
			*captured = append(*captured, string(body))
		}
		switch {
		case strings.Contains(req.Query, "estimatedCalls"):
			_, _ = w.Write(read(t, "departures-jernbanetorget.json"))
		case strings.Contains(req.Query, "lines("):
			if code, _ := req.Variables["code"].(string); code != "31" {
				_, _ = io.WriteString(w, `{"data":{"lines":[]}}`)
				return
			}
			_, _ = w.Write(read(t, "lines-31.json"))
		default:
			_, _ = io.WriteString(w, `{"data":{"line":null}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(t *testing.T, base string) *Provider {
	t.Helper()
	p := New(registry.Deps{Now: func() time.Time { return fixtureNow }}, Oslo).(*Provider)
	p.BaseURL = base + "/journey-planner/v3/graphql"
	p.GeocoderURL = base + "/geocoder/v1"
	return p
}

func TestConform(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	transittest.Conform(t, p, transittest.Case{
		Route:    "31",
		Query:    "jernbanetorget",
		WantStop: "Jernbanetorget",
		Now:      fixtureNow,
	})
}

func TestFindRoutes(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	routes, err := p.FindRoutes(context.Background(), "31")
	if err != nil {
		t.Fatalf("FindRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	want := transit.Route{ID: "RUT:Line:31", ShortName: "31", LongName: "Snarøya - Fornebu - Tonsenhagen - Grorud", Mode: "bus"}
	if routes[0] != want {
		t.Errorf("route = %+v, want %+v", routes[0], want)
	}
}

func TestFindRoutesUnknown(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	_, err := p.FindRoutes(context.Background(), "zz-no-such-route-zz")
	var ure *transit.UnknownRouteError
	if !errors.As(err, &ure) {
		t.Fatalf("err = %v (%T), want *transit.UnknownRouteError", err, err)
	}
	if !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("errors.Is(err, ErrUnknownRoute) = false")
	}
}

func TestRouteStops(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	ps, err := p.RouteStops(context.Background(), transit.Route{ID: "RUT:Line:31", ShortName: "31"})
	if err != nil {
		t.Fatalf("RouteStops: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d patterns, want 2", len(ps))
	}
	if ps[0].DirectionID != "outbound" || ps[1].DirectionID != "inbound" {
		t.Errorf("directions = %q/%q", ps[0].DirectionID, ps[1].DirectionID)
	}
	if ps[0].Headsign != "Tonsenhagen-Snarøya" {
		t.Errorf("headsign = %q", ps[0].Headsign)
	}
	var found bool
	for _, s := range ps[0].Stops {
		if s.ID == "NSR:Quay:7203" {
			found = true
			if s.Name != "Jernbanetorget" {
				t.Errorf("name = %q, want the stop place name", s.Name)
			}
			if s.ParentID != "NSR:StopPlace:4000" {
				t.Errorf("parent = %q", s.ParentID)
			}
		}
	}
	if !found {
		t.Errorf("NSR:Quay:7203 missing from the outbound pattern")
	}
}

func TestSearchStops(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)

	got, err := p.SearchStops(context.Background(), "Jernbanetorget")
	if err != nil {
		t.Fatalf("SearchStops: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stops, want 1", len(got))
	}
	if got[0].ID != "NSR:StopPlace:58366" {
		t.Errorf("id = %q", got[0].ID)
	}
	if got[0].Name != "Jernbanetorget, Oslo" {
		t.Errorf("name = %q", got[0].Name)
	}
	if got[0].Lat < 59 || got[0].Lat > 60 || got[0].Lon < 10 || got[0].Lon > 11 {
		t.Errorf("coordinates look swapped: %v, %v", got[0].Lat, got[0].Lon)
	}

	// Two stops named "Grorud" in different municipalities must stay
	// distinguishable after the locality is appended.
	got, err = p.SearchStops(context.Background(), "grorud")
	if err != nil {
		t.Fatalf("SearchStops: %v", err)
	}
	names := make([]string, len(got))
	for i, s := range got {
		names[i] = s.Name
	}
	want := []string{"Grorud, Oslo", "Grorud, Tønsberg", "Grorud stasjon, Oslo"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestDepartures(t *testing.T) {
	var queries []string
	srv := serve(t, &queries)
	p := newProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"NSR:Quay:7203", "NSR:Quay:7202"}, nil, 90*time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v, want %v", deps.Now, fixtureNow)
	}
	if len(deps.Departures) != 8 {
		t.Fatalf("got %d departures, want 8", len(deps.Departures))
	}
	if len(queries) != 1 {
		t.Fatalf("sent %d GraphQL queries, want 1 batched call", len(queries))
	}
	if !strings.Contains(queries[0], "timeRange: 5400") {
		t.Errorf("window not translated to timeRange seconds: %s", queries[0])
	}
	if !strings.Contains(queries[0], "quay(id:$id0)") || strings.Contains(queries[0], "stopPlace(id:") {
		t.Errorf("quay ids must use the quay() field: %s", queries[0])
	}
	for i := 1; i < len(deps.Departures); i++ {
		if deps.Departures[i].At.Before(deps.Departures[i-1].At) {
			t.Fatalf("departures are not sorted by At")
		}
	}
	first := deps.Departures[0]
	if first.Line != "31" || first.RouteID != "RUT:Line:31" {
		t.Errorf("line = %q / %q", first.Line, first.RouteID)
	}
	if first.Headsign != "Grorud T" || first.DirectionID != "inbound" {
		t.Errorf("headsign = %q, direction = %q", first.Headsign, first.DirectionID)
	}
	if first.StopID != "NSR:Quay:7202" || first.Platform != "D" {
		t.Errorf("stop = %q, platform = %q", first.StopID, first.Platform)
	}
	if !first.Live {
		t.Errorf("first departure should be live")
	}
	if first.At.Equal(first.Scheduled) {
		t.Errorf("live departure should carry a prediction distinct from the aimed time")
	}
	if first.TripID == "" {
		t.Errorf("TripID is empty")
	}
	if zone, _ := first.At.Zone(); !strings.HasPrefix(zone, "CE") {
		t.Errorf("times should be in Europe/Oslo, got zone %q", zone)
	}
	// Schedule-only calls must report At == Scheduled so the app does not
	// render them as predictions.
	var sched int
	for _, d := range deps.Departures {
		if !d.Live {
			sched++
			if !d.At.Equal(d.Scheduled) {
				t.Errorf("non-live departure %v: At %v != Scheduled %v", d.TripID, d.At, d.Scheduled)
			}
		}
	}
	if sched == 0 {
		t.Errorf("fixture has no schedule-only departures")
	}
}

func TestDeparturesStopPlaceAndOddJSON(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(b, &req)
		gotQuery = req.Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"s0":{"name":"Jernbanetorget","estimatedCalls":[
			{"aimedDepartureTime":"2026-09-13T20:50:00+02:00","expectedDepartureTime":"2026-09-13T20:50:00+02:00","realtime":false,"cancellation":true,"destinationDisplay":null,"serviceJourney":{"id":"RUT:ServiceJourney:x","line":{"id":"RUT:Line:17","publicCode":"17","transportMode":"tram"},"directionType":"outbound"},"quay":null},
			{"aimedDepartureTime":"2026-09-13T20:55:00+02:00","expectedDepartureTime":"","realtime":false,"cancellation":false,"destinationDisplay":{"frontText":"Rikshospitalet"},"serviceJourney":null,"quay":{"id":"NSR:Quay:99","publicCode":""}}
		]},"s1":null}}`)
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL)

	deps, err := p.Departures(context.Background(), []string{"NSR:StopPlace:58366", "NSR:StopPlace:404"}, nil, time.Hour)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if !strings.Contains(gotQuery, "stopPlace(id:$id0)") {
		t.Errorf("non-quay ids must use the stopPlace() field: %s", gotQuery)
	}
	if len(deps.Departures) != 2 {
		t.Fatalf("got %d departures, want 2", len(deps.Departures))
	}
	d0 := deps.Departures[0]
	if !d0.Cancelled || d0.StopID != "NSR:StopPlace:58366" || d0.Headsign != "" {
		t.Errorf("cancelled call mapped wrong: %+v", d0)
	}
	d1 := deps.Departures[1]
	if !d1.At.Equal(d1.Scheduled) || d1.At.IsZero() {
		t.Errorf("At should fall back to Scheduled: %+v", d1)
	}
	if d1.Line != "" || d1.TripID != "" || d1.StopID != "NSR:Quay:99" {
		t.Errorf("null serviceJourney mapped wrong: %+v", d1)
	}
}

func TestDeparturesNoStops(t *testing.T) {
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("Departures(nil): %v", err)
	}
	if len(deps.Departures) != 0 || deps.Now.IsZero() {
		t.Errorf("want an empty board with a clock, got %+v", deps)
	}
}

func TestGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errors":[{"message":"Validation error: no such field"}],"data":null}`)
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL)
	if _, err := p.FindRoutes(context.Background(), "31"); err == nil || !strings.Contains(err.Error(), "no such field") {
		t.Fatalf("err = %v, want the GraphQL error surfaced", err)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL)
	if _, err := p.FindRoutes(context.Background(), "31"); !errors.Is(err, transit.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if _, err := p.SearchStops(context.Background(), "jernbanetorget"); !errors.Is(err, transit.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestClientNameHeader(t *testing.T) {
	// serve() 403s when ET-Client-Name is missing, so a successful call
	// proves the header is set on both endpoints.
	srv := serve(t, nil)
	p := newProvider(t, srv.URL)
	if _, err := p.SearchStops(context.Background(), "jernbanetorget"); err != nil {
		t.Fatalf("geocoder: %v", err)
	}
	if _, err := p.FindRoutes(context.Background(), "31"); err != nil {
		t.Fatalf("journey planner: %v", err)
	}
}

func TestInfo(t *testing.T) {
	p := New(registry.Deps{}, Oslo)
	got := p.Info()
	if got.ID != "oslo" || got.TZ != "Europe/Oslo" || !got.Realtime {
		t.Errorf("info = %+v", got)
	}
	if len(got.Keys) != 0 {
		t.Errorf("Entur is keyless, got %+v", got.Keys)
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{Now: time.Now}, Oslo)
	transittest.Live(t, p, transittest.Case{
		Route:    Oslo.Probe.Route,
		Query:    Oslo.Probe.Query,
		WantStop: "Jernbanetorget",
	})
}

func TestEachCitySendsItsAuthority(t *testing.T) {
	for _, c := range cities {
		if len(c.authorities) == 0 {
			continue // the country-wide entry declines route lookups
		}
		var captured []string
		srv := serve(t, &captured)
		p := New(registry.Deps{Now: func() time.Time { return fixtureNow }}, c.info).(*Provider)
		p.BaseURL = srv.URL
		if _, err := p.FindRoutes(context.Background(), "31"); err != nil {
			t.Fatalf("%s: %v", c.info.ID, err)
		}
		if len(captured) != 1 || !strings.Contains(captured[0], c.authorities[0]) {
			t.Errorf("%s: request did not carry authority %s: %v", c.info.ID, c.authorities[0], captured)
		}
		if p.Info().ID != c.info.ID || p.Info().Provider != "entur" {
			t.Errorf("%s: Info = %+v", c.info.ID, p.Info())
		}
	}
}

func TestAnyTownScopesByLocalityAndDistance(t *testing.T) {
	srv := serve(t, nil)
	c := cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	p := NewTown(registry.Deps{Now: func() time.Time { return fixtureNow }, Cache: &c}, "Oslo").(*Provider)
	p.BaseURL, p.GeocoderURL = srv.URL, srv.URL
	if p.Info().ID != "oslo" || p.Info().Country != "norway" || p.Info().Provider != "entur" {
		t.Fatalf("info: %+v", p.Info())
	}
	// The grorud fixture has a Grorud in Oslo and one in Tønsberg; only the
	// Oslo one belongs to the town.
	stops, err := p.SearchStops(context.Background(), "grorud")
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 2 {
		t.Fatalf("stops = %+v", stops)
	}
	for _, st := range stops {
		if !strings.HasSuffix(st.Name, ", Oslo") {
			t.Errorf("stop outside the town kept: %+v", st)
		}
	}
	// Lines are matched nationwide and kept when a quay is near the town.
	rs, err := p.FindRoutes(context.Background(), "31")
	if err != nil || len(rs) == 0 {
		t.Fatalf("FindRoutes: %+v %v", rs, err)
	}
	if ps, err := p.RouteStops(context.Background(), rs[0]); err != nil || len(ps) == 0 {
		t.Errorf("RouteStops: %v", err)
	}
	// A town nowhere near the fixture's quays keeps no line.
	far := NewTown(registry.Deps{Now: func() time.Time { return fixtureNow }, Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour}}, "Tromsø").(*Provider)
	far.BaseURL, far.GeocoderURL = srv.URL, srv.URL
	_ = far.cache.Store("place", place{Lat: 69.65, Lon: 18.96, Locality: "Tromsø"})
	if _, err := far.FindRoutes(context.Background(), "31"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("far town should not see Oslo's line 31: %v", err)
	}
}
