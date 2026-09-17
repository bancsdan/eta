package ovapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live on 2026-09-17: the stop-area index trimmed to
// a few Amsterdam and Rotterdam areas, and area 09500 (Amsterdam Centraal)
// trimmed to ten passes plus one hand-added CANCEL row. fixtureNow sits
// just before the earliest target time.
var fixtureNow = time.Date(2026, 9, 17, 15, 27, 0, 0, time.FixedZone("CEST", 2*3600))

// Every Amsterdam Centraal area answers with the same captured board.
var fixtures = map[string]string{
	"/stopareacode":       "stopareacode.json",
	"/stopareacode/05003": "area-09500.json",
	"/stopareacode/05104": "area-09500.json",
	"/stopareacode/09500": "area-09500.json",
	"/stopareacode/09575": "area-09500.json",
}

func newTestProvider(t *testing.T, base string, c city) *Provider {
	t.Helper()
	p := newProvider(registry.Deps{Now: func() time.Time { return fixtureNow }}, c)
	p.BaseURL = base
	p.index = &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	transittest.Conform(t, newTestProvider(t, srv.URL, cities[0]), transittest.Case{
		Route: "53", Query: "centraal", WantStop: "Centraal Station", Now: fixtureNow,
	})
}

func TestTownScopingAndMapping(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL, cities[0])
	ctx := context.Background()
	if !p.Cold() {
		t.Errorf("should be cold before the index download")
	}
	stops, err := p.SearchStops(ctx, "centraal")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stops {
		if strings.Contains(strings.ToLower(s.Name), "rotterdam") {
			t.Errorf("Rotterdam area leaked into Amsterdam: %+v", s)
		}
	}
	if len(stops) < 2 {
		t.Errorf("Amsterdam Centraal has several stop areas, got %+v", stops)
	}
	if _, err := p.SearchStops(ctx, "dam"); err != nil || srv.Hits("stopareacode.json") != 1 || p.Cold() {
		t.Errorf("index should be fetched once: hits=%d cold=%v err=%v", srv.Hits("stopareacode.json"), p.Cold(), err)
	}
	// Any-town scoping and its refusal.
	rot := newTestProvider(t, srv.URL, city{info: cityInfo("rotterdam", "Rotterdam", "", ""), town: "Rotterdam"})
	if s, err := rot.SearchStops(ctx, "centraal"); err != nil || len(s) == 0 {
		t.Errorf("rotterdam: %+v %v", s, err)
	}
	none := newTestProvider(t, srv.URL, city{info: cityInfo("x", "x", "", ""), town: "Atlantis"})
	if _, err := none.SearchStops(ctx, "centraal"); err == nil || !strings.Contains(err.Error(), "no stops in a Dutch town") {
		t.Errorf("unknown town: %v", err)
	}
	deps, err := p.Departures(ctx, []string{"09500"}, nil, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var live, planned, cancelled int
	for _, d := range deps.Departures {
		if d.Line == "" || d.Headsign == "" || d.At.IsZero() || d.At.Location().String() != "Europe/Amsterdam" {
			t.Errorf("bad mapping: %+v", d)
		}
		switch {
		case d.Cancelled:
			cancelled++
		case d.Live:
			live++
		default:
			planned++
			if !d.At.Equal(d.Scheduled) {
				t.Errorf("planned pass must have At == Scheduled: %+v", d)
			}
		}
	}
	if live == 0 || planned == 0 || cancelled == 0 {
		t.Errorf("live=%d planned=%d cancelled=%d", live, planned, cancelled)
	}
}

func TestLive(t *testing.T) {
	transittest.Live(t, New(registry.Deps{Now: time.Now}, Amsterdam), transittest.Case{Route: Amsterdam.Probe.Route, Query: Amsterdam.Probe.Query})
}
