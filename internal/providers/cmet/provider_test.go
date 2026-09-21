package cmet

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live on 2026-09-21 at 11:37 Lisbon time: the stop
// and line lists trimmed to Cascais, Sintra and Campo Grande, the day's
// arrivals at CascaiShopping (050203) and Sintra station (171923), and
// the patterns of their lines.
var fixtureNow = time.Unix(1789984635, 0)

const (
	cascaiShopping = "050203"
	sintraStation  = "171923"
)

func routes(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{
		"/stops":                              "stops.json",
		"/lines":                              "lines.json",
		"/locations/municipalities":           "municipalities.json",
		"/locations/localities":               "localities.json",
		"/arrivals/by_stop/" + cascaiShopping: "arrivals/" + cascaiShopping + ".json",
		"/arrivals/by_stop/" + sintraStation:  "arrivals/" + sintraStation + ".json",
	}
	entries, err := os.ReadDir("testdata/patterns")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		m["/patterns/"+id] = "patterns/" + e.Name()
	}
	return m
}

func newProvider(t *testing.T, srv *transittest.Server, dir, town string) *Provider {
	t.Helper()
	d := registry.Deps{HTTP: srv.Client(), Now: func() time.Time { return fixtureNow }}
	var p *Provider
	if e, ok := registry.LookupTown("portugal", town); ok {
		p = e.New(d).(*Provider)
	} else {
		p = NewTown(d, town).(*Provider)
	}
	p.BaseURL = srv.URL
	p.tables = &cache.Cache{Dir: dir, TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.NewServer(t, routes(t))
	dir := t.TempDir()
	transittest.Conform(t, newProvider(t, srv, dir, "cascais"), transittest.Case{
		Route: "1627", Query: "cascaishopping terminal", WantStop: "CascaiShopping (Terminal)", WantLines: []string{"1627"}, Now: fixtureNow,
	})
	transittest.Conform(t, newProvider(t, srv, dir, "sintra"), transittest.Case{
		Route: "1253", Query: "sintra estação", WantStop: "Sintra (Estação)", WantLines: []string{"1253"}, Now: fixtureNow,
	})
}

func TestScopeAndRoutes(t *testing.T) {
	srv := transittest.NewServer(t, routes(t))
	dir := t.TempDir()
	ctx := context.Background()
	cascais := newProvider(t, srv, dir, "cascais")
	if !cascais.Cold() {
		t.Error("cold before the stop list is fetched")
	}
	rs, err := cascais.FindRoutes(ctx, "1627")
	if err != nil || len(rs) != 1 || rs[0].Mode != "bus" || rs[0].LongName == "" {
		t.Fatalf("1627: %+v %v", rs, err)
	}
	if _, err := cascais.FindRoutes(ctx, "1253"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("1253 is a Sintra line: %v", err)
	}
	if _, err := cascais.FindRoutes(ctx, "162"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("162 must not match 1627: %v", err)
	}
	ps, err := cascais.RouteStops(ctx, rs[0])
	if err != nil || len(ps) != 2 || ps[0].DirectionID != "0" || ps[0].Stops[0].ID != cascaiShopping || !strings.HasPrefix(ps[0].Headsign, "Rio De Mouro") || ps[1].DirectionID != "1" {
		t.Fatalf("RouteStops: %v %+v", err, ps)
	}
	if cascais.Cold() {
		t.Error("warm after the stop list is cached")
	}
	sintra := newProvider(t, srv, dir, "sintra")
	if _, err := sintra.FindRoutes(ctx, "1627"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("1627 in Sintra: %v", err)
	}
	if stops, err := sintra.SearchStops(ctx, "estação"); err != nil || len(stops) == 0 || !strings.Contains(stops[0].Name, "Sintra") {
		t.Errorf("Sintra estação: %+v %v", stops, err)
	}
	if srv.Hits("stops.json") != 1 {
		t.Errorf("stop list fetched %d times; the cache is shared", srv.Hits("stops.json"))
	}
}

func TestAnyTown(t *testing.T) {
	srv := transittest.NewServer(t, routes(t))
	dir := t.TempDir()
	ctx := context.Background()
	estoril := newProvider(t, srv, dir, "Estoril")
	if estoril.Info().ID != "estoril" || estoril.Info().Country != "portugal" || len(estoril.Info().Keys) != 0 {
		t.Errorf("info: %+v", estoril.Info())
	}
	stops, err := estoril.SearchStops(ctx, "estação")
	if err != nil || len(stops) == 0 || !strings.HasPrefix(stops[0].Name, "Estoril") {
		t.Fatalf("Estoril estação: %+v %v", stops, err)
	}
	if stops, _ := estoril.SearchStops(ctx, "cascaishopping"); len(stops) > 0 && strings.HasPrefix(stops[0].Name, "CascaiShopping") {
		t.Errorf("CascaiShopping is in Alcabideche, not Estoril: %+v", stops)
	}
	lisboa := newProvider(t, srv, dir, "Lisboa")
	if stops, err := lisboa.SearchStops(ctx, "campo grande"); err != nil || len(stops) == 0 {
		t.Errorf("Lisboa by municipality name: %+v %v", stops, err)
	}
	nowhere := newProvider(t, srv, dir, "Porto")
	if _, err := nowhere.SearchStops(ctx, "x"); !errors.Is(err, transit.ErrNotFound) {
		t.Errorf("Porto is outside the area: %v", err)
	}
}

func TestDepartures(t *testing.T) {
	srv := transittest.NewServer(t, routes(t))
	dir := t.TempDir()
	ctx := context.Background()
	p := newProvider(t, srv, dir, "cascais")
	ds, err := p.Departures(ctx, []string{cascaiShopping}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Departures) != 7 {
		t.Errorf("want 7 departures in the hour, got %d: %+v", len(ds.Departures), ds.Departures)
	}
	live := 0
	for i, d := range ds.Departures {
		if d.Live {
			live++
		}
		if d.Headsign == "" || d.Line == "" || d.RouteID != d.Line || d.TripID == "" || d.DirectionID == "" || d.StopID != cascaiShopping {
			t.Errorf("bad departure %+v", d)
		}
		if i > 0 && d.At.Before(ds.Departures[i-1].At) {
			t.Errorf("unsorted at %d", i)
		}
	}
	if live != 3 {
		t.Errorf("want 3 live departures, got %d", live)
	}
	in := func(h, m, s int) time.Time { return time.Date(2026, 9, 21, h, m, s, 0, p.loc) }
	d := ds.Departures[1]
	if d.Line != "1620" || !d.Live || !d.At.Equal(in(11, 3, 28)) || !d.Scheduled.Equal(in(11, 5, 0)) || d.DirectionID != "1" || d.Headsign != "Cascais (Terminal)" {
		t.Errorf("1620 at 11:03: %+v", d)
	}
	if d := ds.Departures[0]; d.Line != "1626" || d.Live || !d.At.Equal(in(11, 0, 0)) || d.DirectionID != "0" {
		t.Errorf("1626 at 11:00 (scheduled): %+v", d)
	}
	only, err := p.Departures(ctx, []string{cascaiShopping}, []transit.Route{{ID: "1627"}}, time.Hour)
	if err != nil || len(only.Departures) != 2 {
		t.Errorf("route filter: %+v %v", only, err)
	}
	// A loop line: the run starting here is a departure, the one ending
	// here (same headsign, last call) is not.
	s := newProvider(t, srv, dir, "sintra")
	ds, err = s.Departures(ctx, []string{sintraStation}, nil, time.Hour)
	if err != nil || len(ds.Departures) != 5 {
		t.Fatalf("Sintra: %+v %v", ds, err)
	}
	loops := 0
	for _, d := range ds.Departures {
		if d.Line == "1253" {
			loops++
			if d.Headsign != "Sintra (Estação)" || d.DirectionID != "0" {
				t.Errorf("loop start: %+v", d)
			}
		} else if d.Line != "1252" || d.Headsign != "Portela Sintra (Estação Sul)" {
			t.Errorf("Sintra departure: %+v", d)
		}
	}
	if loops != 2 {
		t.Errorf("want the 2 loop starts, got %d", loops)
	}
	if empty, err := p.Departures(ctx, nil, nil, time.Hour); err != nil || len(empty.Departures) != 0 {
		t.Errorf("no stops: %+v %v", empty, err)
	}
	if none, err := p.Departures(ctx, []string{"000000"}, nil, time.Hour); err != nil || len(none.Departures) != 0 {
		t.Errorf("unknown stop (404) is an empty board: %+v %v", none, err)
	}
}

func TestRegistry(t *testing.T) {
	c, ok := registry.LookupCountry("pt")
	if !ok || c.ID != "portugal" || c.AnyTown == nil {
		t.Fatalf("country: %+v", c)
	}
	if e, ok := registry.LookupTown("portugal", "lisboa"); !ok || e.Info.ID != "lisbon" {
		t.Error("lisboa alias")
	}
	if e, ok := registry.LookupTown("portugal", "setúbal"); !ok || e.Info.ID != "setubal" {
		t.Error("accented Setúbal")
	}
	info, _, err := registry.Resolve("portugal", "Queluz")
	if err != nil || info.ID != "queluz" || info.Provider != "cmet" {
		t.Errorf("Resolve Queluz: %+v %v", info, err)
	}
}

func TestLive(t *testing.T) {
	e, _ := registry.LookupTown("portugal", "cascais")
	transittest.Live(t, e.New(registry.Deps{}), transittest.Case{Route: e.Info.Probe.Route, Query: e.Info.Probe.Query, WantStop: "CascaiShopping", WantLines: []string{"1627"}})
}

func TestTerminating(t *testing.T) {
	cases := []struct {
		headsign, stop string
		seq            int
		want           bool
	}{
		{"CascaiShopping (Terminal)", "CascaiShopping (Terminal)", 33, true},
		{"CascaiShopping (Terminal)", "CascaiShopping (Terminal)", 1, false},
		{"Cacilhas (Terminal)", "Cacilhas (Terminal) P5", 20, true},
		{"Campo Grande", "Campo Grande (Desembarque Norte)", 30, true},
		{"Campo Grande", "Campo Grande (Metro) P7", 30, false},
		{"Campo Grande", "Campo Grande - Av. Brasil", 28, false},
		{"Portela Sintra (Estação Sul)", "Sintra (Estação)", 26, false},
		{"Oeiras (Estação)", "Oeiras (Estação) P7 Entrada Norte", 12, true},
		{"", "Anything", 5, false},
	}
	for _, c := range cases {
		a := arrival{Headsign: c.headsign, Seq: c.seq}
		if got := terminating(a, match.Normalize(c.stop)); got != c.want {
			t.Errorf("terminating(%q at %q, seq %d) = %v", c.headsign, c.stop, c.seq, got)
		}
	}
}
