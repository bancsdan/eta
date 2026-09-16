package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/transit"
)

var now = time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)

type fake struct {
	findCalls, stopsCalls atomic.Int32
	searchCalls, depCalls atomic.Int32
	board                 []transit.Departure
}

func (f *fake) Info() transit.Info { return transit.Info{ID: "test", Name: "Test", TZ: "UTC"} }

func (f *fake) Departures(_ context.Context, stopIDs []string, _ []transit.Route, _ time.Duration) (*transit.Departures, error) {
	f.depCalls.Add(1)
	var out []transit.Departure
	for _, d := range f.board {
		for _, id := range stopIDs {
			if d.StopID == id {
				out = append(out, d)
			}
		}
	}
	return &transit.Departures{Now: now, Departures: out}, nil
}

type routeFake struct{ *fake }

func (f routeFake) FindRoutes(_ context.Context, short string) ([]transit.Route, error) {
	f.findCalls.Add(1)
	if short != "155" {
		return nil, &transit.UnknownRouteError{Route: short, Suggestions: []string{"155"}}
	}
	return []transit.Route{{ID: "r155", ShortName: "155"}}, nil
}

func (f routeFake) RouteStops(_ context.Context, r transit.Route) ([]transit.Pattern, error) {
	f.stopsCalls.Add(1)
	return []transit.Pattern{
		{DirectionID: "0", Headsign: "Zugliget", Stops: []transit.Stop{{ID: "a0", Name: "Széll Kálmán tér M"}, {ID: "z0", Name: "Zugligeti út"}}},
		{DirectionID: "1", Headsign: "Széll Kálmán tér M", Stops: []transit.Stop{{ID: "z1", Name: "Zugligeti út"}, {ID: "a1", Name: "Széll Kálmán tér M"}}},
	}, nil
}

type searchFake struct{ *fake }

func (f searchFake) SearchStops(_ context.Context, q string) ([]transit.Stop, error) {
	f.searchCalls.Add(1)
	if strings.Contains(q, "nowhere") {
		return nil, nil
	}
	return []transit.Stop{{ID: "alex", Name: "S+U Alexanderplatz"}, {ID: "alex2", Name: "Alexanderplatz/Dircksenstr."}}, nil
}

func board() []transit.Departure {
	return []transit.Departure{
		{StopID: "z0", Line: "155", RouteID: "r155", DirectionID: "0", Headsign: "Zugliget", At: now.Add(3*time.Minute + 12*time.Second), Live: true, TripID: "t1"},
		{StopID: "z0", Line: "155", RouteID: "r155", DirectionID: "0", Headsign: "Zugliget", At: now.Add(7 * time.Minute), Live: true, TripID: "t2"},
		{StopID: "z1", Line: "155", RouteID: "r155", DirectionID: "1", Headsign: "Széll Kálmán tér M", At: now.Add(5 * time.Minute), Scheduled: now.Add(5 * time.Minute), TripID: "t3"},
		{StopID: "z1", Line: "155", RouteID: "r155", DirectionID: "1", Headsign: "Széll Kálmán tér M", At: now.Add(-2 * time.Minute), Live: true, TripID: "gone"},
		{StopID: "z0", Line: "22", RouteID: "r22", DirectionID: "0", Headsign: "Elsewhere", At: now.Add(time.Minute), Live: true, TripID: "other"},
		{StopID: "z0", Line: "155", RouteID: "r155", DirectionID: "0", Headsign: "Zugliget", At: now.Add(9 * time.Minute), Cancelled: true, TripID: "c"},
		{StopID: "alex", Line: "M4", DirectionID: "", Headsign: "Falkenberg", At: now.Add(2 * time.Minute), Live: true, TripID: "b1"},
		{StopID: "alex", Line: "M4", DirectionID: "", Headsign: "Falkenberg", At: now.Add(2 * time.Minute), Live: true, TripID: "b1"}, // duplicate platform
		{StopID: "alex2", Line: "M5", DirectionID: "", Headsign: "Zingster Str.", At: now.Add(4 * time.Minute), Live: true, TripID: "b2"},
		{StopID: "alex", Line: "M6", Headsign: "Hackescher Markt", At: now.Add(time.Minute), Live: true, TripID: "b3"},
		{StopID: "alex2", Line: "M6", Headsign: "Hackescher Markt", At: now.Add(3 * time.Minute), Live: true, TripID: "b4"},
	}
}

func newApp(t *testing.T, p transit.Provider) (*App, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return &App{Provider: p, Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return now }}, Out: &buf}, &buf
}

func TestRunRouteListerGroupsAndCaches(t *testing.T) {
	f := &fake{board: board()}
	a, buf := newApp(t, routeFake{f})
	opts := Options{Country: "testland", Town: "test", Route: "155", Query: "zugligeti", Count: 2}
	for i := 0; i < 2; i++ {
		buf.Reset()
		if err := a.Run(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
	}
	want := "Zugligeti út\n155 → Zugliget\n  3m12s  7m0s\n155 → Széll Kálmán tér M\n  ~5m0s\n"
	if buf.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", buf.String(), want)
	}
	if f.findCalls.Load() != 1 || f.stopsCalls.Load() != 1 || f.depCalls.Load() != 2 {
		t.Errorf("calls find=%d stops=%d deps=%d, want 1/1/2", f.findCalls.Load(), f.stopsCalls.Load(), f.depCalls.Load())
	}
}

func TestRunList(t *testing.T) {
	a, buf := newApp(t, routeFake{&fake{}})
	if err := a.Run(context.Background(), Options{Route: "155", List: true}); err != nil {
		t.Fatal(err)
	}
	want := "155\nSzéll Kálmán tér M → Zugliget\n  1. Széll Kálmán tér M\n  2. Zugligeti út\nZugligeti út → Széll Kálmán tér M\n  1. Zugligeti út\n  2. Széll Kálmán tér M\n"
	if buf.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestRunStopSearcherDisambiguatesByRoute(t *testing.T) {
	f := &fake{board: board()}
	a, buf := newApp(t, searchFake{f})
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "M5", Query: "alexanderplatz"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Alexanderplatz/Dircksenstr.\nM5 → Zingster Str.\n  4m0s\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	buf.Reset()
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "M4", Query: "alexanderplatz"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "S+U Alexanderplatz\nM4 → Falkenberg\n  2m0s\n" {
		t.Errorf("output:\n%s", buf.String())
	}
	if f.searchCalls.Load() != 1 {
		t.Errorf("search calls = %d, want 1 (cached)", f.searchCalls.Load())
	}
	buf.Reset()
	var hint bytes.Buffer
	a.Err = &hint
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "M6", Query: "alexanderplatz"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "S+U Alexanderplatz\nM6 → Hackescher Markt\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	if hint.String() != "eta: \"alexanderplatz\" also matches Alexanderplatz/Dircksenstr.\n" {
		t.Errorf("hint = %q", hint.String())
	}
}

func TestRunBoardWithoutRoute(t *testing.T) {
	f := &fake{board: board()}
	a, buf := newApp(t, searchFake{f})
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Query: "alexanderplatz", Count: 2}); err != nil {
		t.Fatal(err)
	}
	want := "S+U Alexanderplatz\nM4 → Falkenberg\n  2m0s\nM6 → Hackescher Markt\n  1m0s\n"
	if buf.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", buf.String(), want)
	}
	if f.depCalls.Load() != 1 {
		t.Errorf("departure calls = %d, want 1 (no probing without a route)", f.depCalls.Load())
	}
	buf.Reset()
	a.Pick = func(names []string) int {
		if len(names) != 2 || names[1] != "Alexanderplatz/Dircksenstr." {
			t.Errorf("pick names = %v", names)
		}
		return 1
	}
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Query: "alexanderplatz"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Alexanderplatz/Dircksenstr.\nM5 → Zingster Str.\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	buf.Reset()
	a.Pick = nil
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Query: "alexanderplatz", JSON: true}); err != nil {
		t.Fatal(err)
	}
	var got DeparturesJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Route != "" || len(got.Directions) != 2 || got.Directions[0].Line != "M4" || got.Directions[1].Line != "M6" {
		t.Errorf("json: %+v", got)
	}
	a2, _ := newApp(t, routeFake{&fake{}})
	err := a2.Run(context.Background(), Options{Query: "zugligeti"})
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "give a route") {
		t.Errorf("err = %v", err)
	}
}

// bothFake lists routes and searches stops; with declineRoutes it behaves
// like an unscoped national provider that cannot resolve routes by name.
type bothFake struct {
	*fake
	declineRoutes bool
}

func (f bothFake) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	if f.declineRoutes {
		return nil, transit.ErrNoRouteLookup
	}
	return routeFake{f.fake}.FindRoutes(ctx, short)
}
func (f bothFake) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	return routeFake{f.fake}.RouteStops(ctx, r)
}
func (f bothFake) SearchStops(ctx context.Context, q string) ([]transit.Stop, error) {
	return searchFake{f.fake}.SearchStops(ctx, q)
}

func TestFallsBackToSearchOnlyWhenLookupUnavailable(t *testing.T) {
	a, buf := newApp(t, bothFake{fake: &fake{board: board()}, declineRoutes: true})
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "M5", Query: "alexanderplatz"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Alexanderplatz/Dircksenstr.\nM5 → Zingster Str.\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "M5", List: true})
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "-l is not supported") {
		t.Errorf("-l with search fallback: %v", err)
	}
	// A scoped provider that simply doesn't know the route must still say so,
	// not silently fall back and blame the stop.
	a2, buf2 := newApp(t, bothFake{fake: &fake{board: board()}})
	err = a2.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "9999", Query: "zugligeti"})
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, `unknown route "9999"`) {
		t.Errorf("typo in route: %v", err)
	}
	if err := a2.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "155", Query: "zugligeti"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf2.String(), "Zugligeti út\n155 → Zugliget\n") {
		t.Errorf("output:\n%s", buf2.String())
	}
}

func TestPickOnRouteListerAmbiguity(t *testing.T) {
	a, buf := newApp(t, routeFake{&fake{board: board()}})
	a.Pick = func(names []string) int { return 1 } // "t" matches both stops; take the second
	if err := a.Run(context.Background(), Options{Route: "155", Query: "t"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Zugligeti út\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	a.Pick = func([]string) int { return -1 }
	err := a.Run(context.Background(), Options{Route: "155", Query: "t"})
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "matches several stops") {
		t.Errorf("declined pick should fall back to the usage error, got %v", err)
	}
}

func TestLineLess(t *testing.T) {
	in := []string{"M10", "155", "Central", "9", "M4", "m2", "U2", "S3"}
	sort.SliceStable(in, func(i, j int) bool { return lineLess(in[i], in[j]) < 0 })
	want := "9 155 Central m2 M4 M10 S3 U2"
	if got := strings.Join(in, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestRunUserErrors(t *testing.T) {
	cases := []struct {
		name string
		p    transit.Provider
		o    Options
		want string
	}{
		{"unknown route", routeFake{&fake{}}, Options{Route: "9", Query: "x"}, `unknown route "9" (did you mean: 155)`},
		{"no stop", routeFake{&fake{}}, Options{Route: "155", Query: "nothing"}, "no stop matching"},
		{"ambiguous", routeFake{&fake{}}, Options{Route: "155", Query: "t"}, "matches several stops"},
		{"list unsupported", searchFake{&fake{}}, Options{Route: "M4", List: true}, "-l is not supported"},
		{"search empty", searchFake{&fake{}}, Options{Route: "M4", Query: "nowhere"}, "no stop matching"},
		{"not on route", searchFake{&fake{board: board()}}, Options{Route: "99", Query: "alexanderplatz"}, "is served by route 99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := newApp(t, c.p)
			err := a.Run(context.Background(), c.o)
			var ue *UsageError
			if !errors.As(err, &ue) || !strings.Contains(ue.Msg, c.want) {
				t.Errorf("err = %v, want UsageError containing %q", err, c.want)
			}
		})
	}
}

func TestRunNoUpcoming(t *testing.T) {
	a, buf := newApp(t, routeFake{&fake{}})
	if err := a.Run(context.Background(), Options{Route: "155", Query: "zugligeti"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "Zugligeti út\nNo upcoming 155 departures in the next 90 min.\n" {
		t.Errorf("output:\n%s", buf.String())
	}
}

func TestRunJSON(t *testing.T) {
	a, buf := newApp(t, routeFake{&fake{board: board()}})
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "test", Route: "155", Query: "zugligeti", JSON: true, Count: 5}); err != nil {
		t.Fatal(err)
	}
	var got DeparturesJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Town != "test" || got.Country != "testland" || got.Stop != "Zugligeti út" || got.Route != "155" || !got.GeneratedAt.Equal(now) {
		t.Errorf("header: %+v", got)
	}
	if len(got.Directions) != 2 || len(got.Directions[0].Departures) != 2 || got.Directions[0].Departures[0].InSeconds != 192 || got.Directions[1].Departures[0].Live {
		t.Errorf("directions: %+v", got.Directions)
	}
}

func TestFormatArrival(t *testing.T) {
	cases := []struct {
		ar    Arrival
		clock bool
		want  string
	}{
		{Arrival{At: now.Add(42 * time.Second), Live: true}, false, "42s"},
		{Arrival{At: now.Add(5*time.Minute + 42*time.Second), Live: true}, false, "5m42s"},
		{Arrival{At: now.Add(-3 * time.Second), Live: true}, false, "now"},
		{Arrival{At: now.Add(time.Hour + 3*time.Minute + 11*time.Second)}, false, "~1h3m11s"},
		{Arrival{At: now.Add(5 * time.Minute), Live: true}, true, "5m0s (" + now.Add(5*time.Minute).Local().Format("15:04") + ")"},
	}
	for _, c := range cases {
		if got := formatArrival(c.ar, now, c.clock); got != c.want {
			t.Errorf("formatArrival(%v) = %q, want %q", c.ar, got, c.want)
		}
	}
}

func TestTownBreaksStopTies(t *testing.T) {
	f := &fake{board: board()}
	a, buf := newApp(t, routeFake{f})
	// "t" matches both stops on 155; the town "zugliget" picks Zugligeti út.
	if err := a.Run(context.Background(), Options{Country: "testland", Town: "zugliget", Route: "155", Query: "t"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Zugligeti út\n") {
		t.Errorf("output:\n%s", buf.String())
	}
	// A town no candidate mentions changes nothing: still ambiguous.
	err := a.Run(context.Background(), Options{Country: "testland", Town: "elsewhere", Route: "155", Query: "t"})
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "matches several stops") {
		t.Errorf("err = %v", err)
	}
}
