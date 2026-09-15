package mta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/gtfsrt"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// testdata/gtfs_subway.zip is the real MTA static feed trimmed to the L line
// (all routes, L stops, two trips per direction); testdata/feed-l.pb is the
// real L feed captured on 2026-09-15. fixtureNow is the feed's own timestamp.

func serve(t *testing.T) (*httptest.Server, map[string]int) {
	t.Helper()
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/gtfs_subway.zip":
			http.ServeFile(w, r, filepath.Join("testdata", "gtfs_subway.zip"))
		case "/feeds/nyct%2Fgtfs-l", "/feeds/nyct/gtfs-l":
			http.ServeFile(w, r, filepath.Join("testdata", "feed-l.pb"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func feedNow(t *testing.T) time.Time {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "feed-l.pb"))
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

func newTestProvider(t *testing.T, srv *httptest.Server, now time.Time) *Provider {
	t.Helper()
	dir := t.TempDir()
	p := New(registry.Deps{
		HTTP:  srv.Client(),
		Cache: &cache.Cache{Dir: dir, TTL: time.Hour, Now: func() time.Time { return now }},
		Now:   func() time.Time { return now },
	}).(*Provider)
	p.FeedURL = srv.URL + "/feeds/nyct%2Fgtfs"
	p.Static.URL = srv.URL + "/gtfs_subway.zip"
	p.Static.Path = filepath.Join(dir, "gtfs", "subway.zip")
	return p
}

func TestConform(t *testing.T) {
	srv, _ := serve(t)
	now := feedNow(t)
	transittest.Conform(t, newTestProvider(t, srv, now), transittest.Case{
		Route: "L", Query: "bedford av", WantStop: "Bedford Av", WantLines: []string{"L"}, Now: now,
	})
}

func TestStationsPlatformsAndFeeds(t *testing.T) {
	srv, hits := serve(t)
	now := feedNow(t)
	p := newTestProvider(t, srv, now)
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
	// A second RouteStops call is served from the disk cache.
	if _, err := p.RouteStops(ctx, rs[0]); err != nil {
		t.Fatal(err)
	}
}

func TestLive(t *testing.T) {
	p := New(registry.Deps{HTTP: &http.Client{}, Cache: cache.Default("mta-test", time.Hour), Now: time.Now})
	transittest.Live(t, p, transittest.Case{Route: info.Probe.Route, Query: info.Probe.Query, WantLines: []string{"L"}})
}

var _ transit.Provider = (*Provider)(nil)
