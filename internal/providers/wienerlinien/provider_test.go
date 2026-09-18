package wienerlinien

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// Fixtures were captured live on 2026-09-18: the open-data CSVs trimmed to
// tram 5 (line id 105) and its stations, and the Praterstern monitor
// trimmed to a few lines and departures. fixtureNow is the monitor's
// serverTime.
var fixtureNow = time.Date(2026, 9, 18, 9, 58, 50, 0, time.FixedZone("CEST", 2*3600))

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/doku/ogd/wienerlinien-ogd-"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/doku/ogd/wienerlinien-ogd-"), ".csv")
			http.ServeFile(w, r, filepath.Join("testdata", name+".csv"))
		case r.URL.Path == "/monitor":
			http.ServeFile(w, r, filepath.Join("testdata", "monitor-60201040.json"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(t *testing.T, srv *httptest.Server, town string) *Provider {
	t.Helper()
	info := vienna
	if town != "Wien" {
		info = NewTown(registry.Deps{}, town).Info()
	}
	p := build(registry.Deps{HTTP: srv.Client(), Now: func() time.Time { return fixtureNow }}, info, town)
	p.BaseURL = srv.URL
	p.shared = &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	return p
}

func TestConform(t *testing.T) {
	srv := serve(t)
	transittest.Conform(t, newTestProvider(t, srv, "Wien"), transittest.Case{
		Route: "5", Query: "praterstern", WantStop: "Praterstern", Now: fixtureNow,
	})
}

func TestTablesAndMonitor(t *testing.T) {
	srv := serve(t)
	p := newTestProvider(t, srv, "Wien")
	ctx := context.Background()
	if !p.Cold() {
		t.Errorf("should be cold before the download")
	}
	rs, err := p.FindRoutes(ctx, "5")
	if err != nil || len(rs) != 1 || rs[0].ID != "105" || rs[0].Mode != "tram" {
		t.Fatalf("FindRoutes: %+v %v", rs, err)
	}
	if u, err := p.FindRoutes(ctx, "u1"); err != nil || len(u) != 1 || u[0].Mode != "metro" {
		t.Errorf("U1: %+v %v", u, err)
	}
	ps, err := p.RouteStops(ctx, rs[0])
	if err != nil || len(ps) < 2 {
		t.Fatalf("RouteStops: %+v %v", ps, err)
	}
	dirs := map[string]bool{}
	for _, pat := range ps {
		dirs[pat.DirectionID] = true
		for i := 1; i < len(pat.Stops); i++ {
			if pat.Stops[i].ID == pat.Stops[i-1].ID {
				t.Errorf("consecutive platforms of one station must collapse: %+v", pat.Stops[i])
			}
		}
	}
	if !dirs["1"] || !dirs["2"] {
		t.Errorf("expected both directions, got %v", dirs)
	}
	if p.Cold() {
		t.Errorf("should be warm after the download")
	}
	deps, err := p.Departures(ctx, []string{"60201040"}, nil, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !deps.Now.Equal(fixtureNow) {
		t.Errorf("Now should be the monitor's serverTime: %v", deps.Now)
	}
	if len(deps.Departures) == 0 {
		t.Fatal("no departures")
	}
	for _, d := range deps.Departures {
		if d.Line == "" || d.Headsign == "" || d.StopID != "60201040" || d.At.Location().String() != "Europe/Vienna" {
			t.Errorf("bad mapping: %+v", d)
		}
		if !d.Live && !d.At.Equal(d.Scheduled) {
			t.Errorf("planned-only entry must have At == Scheduled: %+v", d)
		}
	}
	// Any-town scoping refuses municipalities outside the network.
	none := newTestProvider(t, srv, "Atlantis")
	if _, err := none.SearchStops(ctx, "praterstern"); err == nil || !strings.Contains(err.Error(), "no stations") {
		t.Errorf("unknown municipality: %v", err)
	}
}

func TestLive(t *testing.T) {
	transittest.Live(t, New(registry.Deps{HTTP: &http.Client{}, Now: time.Now}), transittest.Case{Route: vienna.Probe.Route, Query: vienna.Probe.Query})
}
