package bvg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live from https://v6.bvg.transport.rest:
//
//	curl -s 'https://v6.bvg.transport.rest/locations?query=alexanderplatz&results=5&poi=false&addresses=false'
//	curl -s 'https://v6.bvg.transport.rest/stops/900100003/departures?duration=30&results=8'
//
// The departures array was trimmed to 8 rows and two of them hand-edited:
// the "200" bus lost its realtime data (delay null) and the last "100" bus
// was turned into a cancelled trip (cancelled true, when null).
var fixtureNow = time.Date(2026, 9, 13, 20, 42, 0, 0, berlinLoc())

func berlinLoc() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.UTC
	}
	return loc
}

func newTestProvider(t *testing.T, baseURL string) *Provider {
	t.Helper()
	p := New(registry.Deps{Now: func() time.Time { return fixtureNow }}, Berlin).(*Provider)
	p.BaseURL = baseURL
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{
		"/locations":                  "locations-alexanderplatz.json",
		"/stops/900100003/departures": "departures-900100003.json",
	})
	p := newTestProvider(t, srv.URL)
	transittest.Conform(t, p, transittest.Case{
		Route:    "M4",
		Query:    "alexanderplatz",
		WantStop: "Alexanderplatz",
		Now:      fixtureNow,
	})
}

func TestSearchStops(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/locations": "locations-alexanderplatz.json"})
	p := newTestProvider(t, srv.URL)
	stops, err := p.SearchStops(context.Background(), "alexanderplatz")
	if err != nil {
		t.Fatalf("SearchStops: %v", err)
	}
	if len(stops) != 5 {
		t.Fatalf("got %d stops, want 5", len(stops))
	}
	// The API sorts by relevance; we must not reorder it.
	if stops[0].ID != "900100003" || stops[0].Name != "S+U Alexanderplatz Bhf (Berlin)" {
		t.Errorf("first stop = %+v", stops[0])
	}
	if stops[0].Lat == 0 || stops[0].Lon == 0 {
		t.Errorf("first stop has no coordinates: %+v", stops[0])
	}
	if stops[2].ID != "900100005" {
		t.Errorf("API order not preserved: %+v", stops)
	}
}

func TestSearchStopsSkipsNonStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"type":"poi","id":"p1","name":"Fernsehturm"},
			{"type":"location","id":"","name":"Some address"},
			{"type":"station","id":"900100001","name":"S+U Friedrichstr."},
			{"type":"stop","id":"900100003","name":"S+U Alexanderplatz Bhf","location":{"latitude":52.5,"longitude":13.4}}
		]`))
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL)
	stops, err := p.SearchStops(context.Background(), "x")
	if err != nil {
		t.Fatalf("SearchStops: %v", err)
	}
	if len(stops) != 2 || stops[0].ID != "900100001" || stops[1].ID != "900100003" {
		t.Fatalf("got %+v, want only the station and the stop", stops)
	}
}

func TestDeparturesMapping(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/stops/900100003/departures": "departures-900100003.json"})
	p := newTestProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"900100003"}, nil, 30*time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v, want %v", deps.Now, fixtureNow)
	}
	if len(deps.Departures) != 8 {
		t.Fatalf("got %d departures, want 8", len(deps.Departures))
	}
	first := deps.Departures[0]
	if first.Line != "M4" || first.Headsign != "S Hackescher Markt" {
		t.Errorf("first = %+v", first)
	}
	if !first.Live || first.RouteID != "" || first.DirectionID != "" {
		t.Errorf("first = %+v: want Live, empty RouteID and DirectionID", first)
	}
	if first.TripID != "1|63422|22|86|13092026" || first.StopID != "900100026" || first.Platform != "Pos. 15" {
		t.Errorf("first = %+v", first)
	}
	// The parent station's board mixes in its child platforms.
	if first.StopID == deps.Departures[2].StopID {
		t.Errorf("expected departures from several platform ids")
	}
	if want := first.Scheduled.Add(-2 * time.Minute); !first.At.Equal(want) {
		t.Errorf("At = %v, want %v", first.At, want)
	}

	byLine := map[string]transit.Departure{}
	for _, d := range deps.Departures {
		byLine[d.Line] = d
	}
	noRT := byLine["200"] // hand-edited: delay null
	if noRT.Live {
		t.Errorf("line 200 should not be Live: %+v", noRT)
	}
	if !noRT.At.Equal(noRT.Scheduled) || noRT.At.IsZero() {
		t.Errorf("line 200: At %v should equal Scheduled %v", noRT.At, noRT.Scheduled)
	}
	cancelled := byLine["100"] // hand-edited: cancelled, when null
	if !cancelled.Cancelled || cancelled.Live {
		t.Errorf("line 100 should be cancelled and not Live: %+v", cancelled)
	}
	if !cancelled.At.Equal(cancelled.Scheduled) || cancelled.At.IsZero() {
		t.Errorf("cancelled entry should fall back to plannedWhen: %+v", cancelled)
	}
	for i := 1; i < len(deps.Departures); i++ {
		if deps.Departures[i].At.Before(deps.Departures[i-1].At) {
			t.Fatalf("departures not sorted by At at index %d", i)
		}
	}
}

func TestToDepartureEdgeCases(t *testing.T) {
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	b := func(v bool) *bool { return &v }
	planned := "2026-09-13T20:45:00+02:00"
	live := "2026-09-13T20:47:00+02:00"
	at := func(v string) time.Time {
		tt, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatalf("parse %q: %v", v, err)
		}
		return tt
	}

	tests := []struct {
		name string
		in   apiDeparture
		want transit.Departure
	}{
		{
			name: "realtime delay",
			in:   apiDeparture{When: s(live), PlannedWhen: s(planned), Delay: i(120), Line: &apiLine{Name: "U2"}},
			want: transit.Departure{At: at(live), Scheduled: at(planned), Live: true, Line: "U2"},
		},
		{
			name: "null delay means schedule only",
			in:   apiDeparture{When: s(planned), PlannedWhen: s(planned), Line: &apiLine{Name: "RE1"}},
			want: transit.Departure{At: at(planned), Scheduled: at(planned), Line: "RE1"},
		},
		{
			name: "cancelled via null when",
			in:   apiDeparture{When: nil, PlannedWhen: s(planned), Line: &apiLine{Name: "S3"}},
			want: transit.Departure{At: at(planned), Scheduled: at(planned), Cancelled: true, Line: "S3"},
		},
		{
			name: "cancelled flag with a time still set",
			in:   apiDeparture{When: s(live), PlannedWhen: s(planned), Delay: i(120), Cancelled: b(true), Line: &apiLine{Name: "M41"}},
			want: transit.Departure{At: at(live), Scheduled: at(planned), Live: true, Cancelled: true, Line: "M41"},
		},
		{
			name: "platform falls back to plannedPlatform",
			in:   apiDeparture{When: s(planned), PlannedWhen: s(planned), PlannedPlatform: s("3"), Line: &apiLine{Name: "S5"}},
			want: transit.Departure{At: at(planned), Scheduled: at(planned), Platform: "3", Line: "S5"},
		},
		{
			name: "everything missing does not panic",
			in:   apiDeparture{},
			want: transit.Departure{Cancelled: true},
		},
		{
			name: "no plannedWhen keeps the live time",
			in:   apiDeparture{When: s(live), Delay: i(60), Line: &apiLine{Name: "M10"}},
			want: transit.Departure{At: at(live), Live: true, Line: "M10"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toDeparture(tc.in)
			if !got.At.Equal(tc.want.At) || !got.Scheduled.Equal(tc.want.Scheduled) {
				t.Errorf("times: got At=%v Scheduled=%v, want At=%v Scheduled=%v", got.At, got.Scheduled, tc.want.At, tc.want.Scheduled)
			}
			got.At, got.Scheduled = tc.want.At, tc.want.Scheduled
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDeparturesNoStops(t *testing.T) {
	p := newTestProvider(t, "http://127.0.0.1:1") // never dialled
	deps, err := p.Departures(context.Background(), nil, nil, time.Hour)
	if err != nil {
		t.Fatalf("Departures with no stops: %v", err)
	}
	if len(deps.Departures) != 0 || deps.Now.IsZero() {
		t.Fatalf("got %+v, want an empty board with a clock", deps)
	}
}

func TestDeparturesPartialFailure(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/stops/900100003/departures": "departures-900100003.json"})
	p := newTestProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"900100003", "nope"}, nil, 30*time.Minute)
	if err != nil {
		t.Fatalf("Departures: %v", err)
	}
	if len(deps.Departures) != 8 {
		t.Fatalf("got %d departures, want 8", len(deps.Departures))
	}
	if _, err := p.Departures(context.Background(), []string{"nope"}, nil, 30*time.Minute); !errors.Is(err, transit.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, transit.ErrUnauthorized},
		{http.StatusForbidden, transit.ErrUnauthorized},
		{http.StatusNotFound, transit.ErrNotFound},
		{http.StatusTooManyRequests, transit.ErrRateLimited},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", tc.code)
		}))
		p := newTestProvider(t, srv.URL)
		_, err := p.SearchStops(context.Background(), "x")
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: got %v, want %v", tc.code, err, tc.want)
		}
		srv.Close()
	}
}

func TestCancelledContext(t *testing.T) {
	srv := transittest.Serve(t, map[string]string{"/stops/900100003/departures": "departures-900100003.json"})
	p := newTestProvider(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Departures(ctx, []string{"900100003"}, nil, time.Minute); err == nil {
		t.Fatal("want an error for a cancelled context")
	}
}

func TestInfo(t *testing.T) {
	p := New(registry.Deps{}, Berlin).(*Provider)
	got := p.Info()
	if got.ID != "berlin" || got.Name != "Berlin (BVG)" || got.TZ != "Europe/Berlin" || !got.Realtime {
		t.Errorf("Info = %+v", got)
	}
	if len(got.Keys) != 0 {
		t.Errorf("Berlin needs no API key, got %+v", got.Keys)
	}
	if _, ok := transit.Provider(p).(transit.StopSearcher); !ok {
		t.Error("provider must implement StopSearcher")
	}
	if _, ok := transit.Provider(p).(transit.RouteLister); ok {
		t.Error("the BVG API has no route → stops call; do not claim RouteLister")
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{HTTP: &http.Client{Timeout: 20 * time.Second}, Now: time.Now}, Berlin)
	transittest.Live(t, p, transittest.Case{
		Route:    Berlin.Probe.Route,
		Query:    Berlin.Probe.Query,
		WantStop: "Alexanderplatz",
	})
}
