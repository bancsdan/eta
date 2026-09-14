package opendatach

import (
	"context"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live on 2026-09-14 (Zürich, Bellevue, station
// 8576193) and trimmed; the first two entries had their delay hand-edited to
// null and 2 to cover both paths. fixtureNow sits just before the earliest
// departure.
var fixtureNow = time.Date(2026, 9, 14, 21, 59, 0, 0, time.FixedZone("CEST", 2*3600))

var fixtures = map[string]string{
	"/v1/locations?query=Bellevue":     "locations-bellevue.json",
	"/v1/stationboard?station=8576193": "stationboard-8576193.json",
}

func newTestProvider(t *testing.T, base string) *Provider {
	t.Helper()
	p := New(registry.Deps{Now: func() time.Time { return fixtureNow }}, Zurich).(*Provider)
	p.BaseURL = base + "/v1"
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	transittest.Conform(t, newTestProvider(t, srv.URL), transittest.Case{
		Route: "11", Query: "Bellevue", WantStop: "Zürich, Bellevue", Now: fixtureNow,
	})
}

func TestMapping(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	p := newTestProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"8576193"}, nil, 90*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Departures) < 3 {
		t.Fatalf("got %d departures", len(deps.Departures))
	}
	var sawSched, sawLive bool
	for _, d := range deps.Departures {
		if d.Line == "" || d.Headsign == "" || d.StopID != "8576193" {
			t.Errorf("bad mapping: %+v", d)
		}
		if d.Live {
			sawLive = true
		} else {
			sawSched = true
			if !d.At.Equal(d.Scheduled) {
				t.Errorf("schedule-only entry must have At == Scheduled: %+v", d)
			}
		}
		if d.At.Location().String() != "Europe/Zurich" {
			t.Errorf("times must be in the city zone, got %s", d.At.Location())
		}
	}
	if !sawSched || !sawLive {
		t.Errorf("fixture should yield both live and schedule-only entries (live=%v sched=%v)", sawLive, sawSched)
	}
}

func TestCitiesRegistered(t *testing.T) {
	for _, id := range []string{"zurich", "bern", "basel", "geneva", "lausanne", "ch", "genf"} {
		e, ok := registry.Lookup(id)
		if !ok || e.Info.Provider != "opendatach" {
			t.Errorf("%s: %v %+v", id, ok, e.Info)
		}
	}
}

func TestLive(t *testing.T) {
	for _, c := range cities {
		c := c
		t.Run(c.info.ID, func(t *testing.T) {
			transittest.Live(t, New(registry.Deps{Now: time.Now}, c.info), transittest.Case{Route: c.info.Probe.Route, Query: c.info.Probe.Query})
		})
	}
}

var _ transit.Provider = (*Provider)(nil)
