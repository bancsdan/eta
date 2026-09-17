package mta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/gtfsrt"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// testdata holds the real MTA static feeds trimmed to one line each (the
// subway's L, the LIRR's route 4) and the real L and LIRR feeds captured on
// 2026-09-15 and 2026-09-17. fixtureNow is each feed's own timestamp.

func serve(t *testing.T) (*httptest.Server, map[string]int) {
	t.Helper()
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/gtfs_subway.zip", "/gtfs_lirr.zip":
			http.ServeFile(w, r, filepath.Join("testdata", strings.TrimPrefix(r.URL.Path, "/")))
		case "/feeds/nyct%2Fgtfs-l", "/feeds/nyct/gtfs-l":
			http.ServeFile(w, r, filepath.Join("testdata", "feed-l.pb"))
		case "/feeds/lirr%2Fgtfs-lirr", "/feeds/lirr/gtfs-lirr":
			http.ServeFile(w, r, filepath.Join("testdata", "feed-lirr.pb"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func feedNow(t *testing.T, file string) time.Time {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }))
	defer srv.Close()
	msg, err := gtfsrt.Fetch(context.Background(), httpx.New("t", srv.Client()), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return gtfsrt.Timestamp(msg)
}

func deps(t *testing.T, srv *httptest.Server, now time.Time) registry.Deps {
	t.Helper()
	return registry.Deps{
		HTTP:  srv.Client(),
		Cache: &cache.Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return now }},
		Now:   func() time.Time { return now },
	}
}

func subwayProvider(t *testing.T, srv *httptest.Server, now time.Time) *Provider {
	t.Helper()
	p := New(deps(t, srv, now)).(*Provider)
	p.Redirect(srv.URL+"/feeds/", srv.URL, t.TempDir())
	return p
}

func TestConformSubway(t *testing.T) {
	srv, _ := serve(t)
	now := feedNow(t, "feed-l.pb")
	transittest.Conform(t, subwayProvider(t, srv, now), transittest.Case{
		Route: "L", Query: "bedford av", WantStop: "Bedford Av", WantLines: []string{"L"}, Now: now,
	})
}

func TestSubwayStationsPlatformsAndFeeds(t *testing.T) {
	srv, hits := serve(t)
	now := feedNow(t, "feed-l.pb")
	p := subwayProvider(t, srv, now)
	ctx := context.Background()
	if !p.Cold() {
		t.Errorf("should be cold before the download")
	}
	rs, err := p.FindRoutes(ctx, "L")
	if err != nil || len(rs) != 1 || rs[0].ID != "L" {
		t.Fatalf("FindRoutes: %+v %v", rs, err)
	}
	if s, err := p.FindRoutes(ctx, "S"); err != nil || len(s) != 3 {
		t.Errorf("the three shuttles share S: %+v %v", s, err)
	}
	ps, err := p.RouteStops(ctx, rs[0])
	if err != nil || len(ps) != 2 {
		t.Fatalf("RouteStops: %+v %v", ps, err)
	}
	// Patterns are reported as parent stations, not platforms.
	if ps[0].Stops[0].ID != "L29" || ps[0].Headsign != "8 Av" || ps[1].Stops[0].ID != "L01" || ps[1].Headsign != "Canarsie-Rockaway Pkwy" {
		t.Errorf("patterns: %+v | %+v", ps[0].Stops[0], ps[1].Stops[0])
	}
	if hits["/gtfs_subway.zip"] != 1 || p.Cold() {
		t.Errorf("static zip downloads=%d cold=%v", hits["/gtfs_subway.zip"], p.Cold())
	}
	deps, err := p.Departures(ctx, []string{"L08"}, rs, time.Hour) // Bedford Av
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Departures) == 0 {
		t.Fatalf("no departures at Bedford Av in the captured feed")
	}
	if !deps.Now.Equal(now) {
		t.Errorf("Now should be the feed timestamp: %v vs %v", deps.Now, now)
	}
	dirs := map[string]bool{}
	for _, d := range deps.Departures {
		if d.Line != "L" || d.RouteID != "L" || !d.Live || d.Headsign == "" {
			t.Errorf("bad mapping: %+v", d)
		}
		if d.StopID != "L08N" && d.StopID != "L08S" {
			t.Errorf("departure must be at a Bedford Av platform: %+v", d)
		}
		dirs[d.DirectionID] = true
	}
	if !dirs["N"] || !dirs["S"] {
		t.Errorf("expected both directions, got %v", dirs)
	}
	if hits["/feeds/nyct%2Fgtfs-l"]+hits["/feeds/nyct/gtfs-l"] != 1 {
		t.Errorf("only the L feed should be fetched for route L: %v", hits)
	}
	if _, err := p.RouteStops(ctx, rs[0]); err != nil {
		t.Fatal(err)
	}
}

// townProvider is NewTown restricted to the systems the fixtures cover.
func townProvider(t *testing.T, srv *httptest.Server, now time.Time, town string) *Provider {
	t.Helper()
	info := NewTown(registry.Deps{}, town).Info()
	p := build(deps(t, srv, now), info, town, "subway", "lirr")
	p.Redirect(srv.URL+"/feeds/", srv.URL, t.TempDir())
	return p
}

func TestConformLIRRTown(t *testing.T) {
	srv, _ := serve(t)
	now := feedNow(t, "feed-lirr.pb")
	transittest.Conform(t, townProvider(t, srv, now, "Hicksville"), transittest.Case{
		Route: "Ronkonkoma", Query: "hicksville", WantStop: "Hicksville", Now: now,
	})
}

func TestTownSearchesEverySystem(t *testing.T) {
	srv, hits := serve(t)
	now := feedNow(t, "feed-lirr.pb")
	p := townProvider(t, srv, now, "Hicksville")
	ctx := context.Background()
	if p.Info().ID != "hicksville" || p.Info().Country != "usa" {
		t.Errorf("info: %+v", p.Info())
	}
	stops, err := p.SearchStops(ctx, "hicksville")
	if err != nil || len(stops) != 1 || stops[0].ID != "lirr:92" {
		t.Fatalf("SearchStops: %+v %v", stops, err)
	}
	// Ids are qualified per system across the provider.
	rs, err := p.FindRoutes(ctx, "ronkonkoma")
	if err != nil || len(rs) != 1 || rs[0].ID != "lirr:4" || rs[0].ShortName != "Ronkonkoma" {
		t.Fatalf("FindRoutes: %+v %v", rs, err)
	}
	if l, err := p.FindRoutes(ctx, "L"); err != nil || len(l) != 1 || l[0].ID != "subway:L" {
		t.Errorf("subway route through the town provider: %+v %v", l, err)
	}
	if _, err := p.FindRoutes(ctx, "nowhere"); err == nil {
		t.Errorf("unknown route should fail")
	}
	ps, err := p.RouteStops(ctx, rs[0])
	if err != nil || len(ps) != 2 || !strings.HasPrefix(ps[0].Stops[0].ID, "lirr:") {
		t.Fatalf("RouteStops: %+v %v", ps, err)
	}
	deps, err := p.Departures(ctx, []string{"lirr:92"}, nil, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Departures) == 0 {
		t.Fatalf("no LIRR departures at Hicksville in the captured feed")
	}
	dirs := map[string]bool{}
	for _, d := range deps.Departures {
		if !strings.HasPrefix(d.StopID, "lirr:") || d.Line == "" || d.Headsign == "" || !d.Live {
			t.Errorf("bad mapping: %+v", d)
		}
		if d.Headsign == "Hicksville" {
			t.Errorf("a train terminating here is not a departure: %+v", d)
		}
		if strings.HasPrefix(d.Line, "lirr") || d.Line == d.RouteID {
			t.Errorf("Line must be the branch name, got %+v", d)
		}
		dirs[d.DirectionID] = true
	}
	if !dirs["0"] && !dirs["1"] {
		t.Errorf("railroad direction should come from the feed, got %v", dirs)
	}
	if hits["/feeds/lirr%2Fgtfs-lirr"]+hits["/feeds/lirr/gtfs-lirr"] != 1 {
		t.Errorf("one LIRR feed fetch expected: %v", hits)
	}
	if n := hits["/feeds/nyct%2Fgtfs"] + hits["/feeds/nyct/gtfs"]; n != 0 {
		t.Errorf("no subway feed should be fetched for an LIRR-only stop: %v", hits)
	}
	// A town with no station anywhere is refused clearly.
	none := townProvider(t, srv, now, "Atlantis")
	if _, err := none.SearchStops(ctx, "x"); err == nil || !strings.Contains(err.Error(), "no station named") {
		t.Errorf("err = %v", err)
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{HTTP: &http.Client{}, Cache: cache.Default("mta/test", time.Hour), Now: time.Now})
	transittest.Live(t, p, transittest.Case{Route: newYork.Probe.Route, Query: newYork.Probe.Query, WantLines: []string{"L"}})
}

var _ transit.Provider = (*Provider)(nil)
