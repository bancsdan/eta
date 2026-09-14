package sl

import (
	"context"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live on 2026-09-14 (Slussen, site 9192) and
// trimmed; one CANCELLED row is kept. fixtureNow sits just before the
// earliest scheduled time.
var fixtureNow = time.Date(2026, 9, 14, 22, 1, 0, 0, time.FixedZone("CEST", 2*3600))

var fixtures = map[string]string{
	"/v1/sites":                 "sites.json",
	"/v1/sites/9192/departures": "departures-9192.json",
}

func newTestProvider(t *testing.T, base string) *Provider {
	t.Helper()
	p := New(registry.Deps{
		Now:   func() time.Time { return fixtureNow },
		Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }},
	}).(*Provider)
	p.BaseURL = base + "/v1"
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	transittest.Conform(t, newTestProvider(t, srv.URL), transittest.Case{
		Route: "19", Query: "slussen", WantStop: "Slussen", Now: fixtureNow,
	})
}

func TestSitesCachedAndCold(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL)
	if !p.Cold() {
		t.Errorf("should be cold before the first download")
	}
	for range 2 {
		stops, err := p.SearchStops(context.Background(), "gamla stan")
		if err != nil || len(stops) != 1 || stops[0].ID != "9193" {
			t.Fatalf("search: %+v %v", stops, err)
		}
	}
	if n := srv.Hits("sites.json"); n != 1 {
		t.Errorf("sites fetched %d times, want 1", n)
	}
	if p.Cold() {
		t.Errorf("should be warm after the download")
	}
	if stops, _ := p.SearchStops(context.Background(), "slusen"); len(stops) == 0 || stops[0].Name != "Slussen" {
		t.Errorf("typo should fall back to closest names, got %+v", stops)
	}
}

func TestMapping(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL)
	deps, err := p.Departures(context.Background(), []string{"9192"}, nil, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled, live int
	for _, d := range deps.Departures {
		if d.Line == "" || d.Headsign == "" || d.At.IsZero() {
			t.Errorf("bad mapping: %+v", d)
		}
		if d.At.Location().String() != "Europe/Stockholm" {
			t.Errorf("times must be Stockholm local, got %s", d.At.Location())
		}
		if d.Cancelled {
			cancelled++
		}
		if d.Live {
			live++
		}
	}
	if cancelled == 0 || live == 0 {
		t.Errorf("fixture should contain live and cancelled rows (live=%d cancelled=%d)", live, cancelled)
	}
	// A single route becomes the API's line filter.
	if _, err := p.Departures(context.Background(), []string{"9192"}, []transit.Route{{ShortName: "2"}}, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	if q := reqs[len(reqs)-1].URL.Query().Get("line"); q != "2" {
		t.Errorf("line filter = %q, want 2", q)
	}
	// The parameter only takes integers, so a non-numeric route is not sent.
	if _, err := p.Departures(context.Background(), []string{"9192"}, []transit.Route{{ShortName: "43X"}}, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	reqs = srv.Requests()
	if q := reqs[len(reqs)-1].URL.Query().Get("line"); q != "" {
		t.Errorf("non-numeric route must not become a line filter, got %q", q)
	}
}

func TestLive(t *testing.T) {
	transittest.Live(t, New(registry.Deps{Now: time.Now, Cache: cache.Default("sl-test", time.Hour)}), transittest.Case{Route: info.Probe.Route, Query: info.Probe.Query})
}
