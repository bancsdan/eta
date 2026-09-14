package digitransit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// The fixtures are hand-written from the Digitransit GTFS GraphQL schema
// (no key was available); TestLive is the real check. fixtureNow is
// 09:00 Helsinki time on the fixtures' service day.
var fixtureNow = time.Date(2026, 9, 15, 9, 0, 0, 0, time.FixedZone("EEST", 3*3600))

// Keys are substrings of the GraphQL query; transittest matches them
// against the POST body because every call goes to one path.
var fixtures = map[string]string{
	"routes(name:":              "routes.graphql.json",
	"route(id:":                 "route.graphql.json",
	"stops(name:":               "stops.graphql.json",
	"stoptimesWithoutPatterns(": "board.graphql.json",
}

func newTestProvider(t *testing.T, base, key string) *Provider {
	t.Helper()
	p := New(registry.Deps{Key: func(string) string { return key }, Now: func() time.Time { return fixtureNow }}, Helsinki).(*Provider)
	p.BaseURL = base
	return p
}

func TestConform(t *testing.T) {
	srv := transittest.Serve(t, fixtures)
	transittest.Conform(t, newTestProvider(t, srv.URL, "k"), transittest.Case{
		Route: "550", Query: "pasila", WantStop: "Pasila", WantLines: []string{"550"}, Now: fixtureNow,
	})
}

func TestMapping(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL, "k")
	ctx := context.Background()
	deps, err := p.Departures(ctx, []string{"HSL:1174501", "HSL:1174502"}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Departures) != 4 {
		t.Fatalf("got %d departures", len(deps.Departures))
	}
	first := deps.Departures[0]
	if !first.Cancelled || first.Headsign != "Itäkeskus" || first.Platform != "2" {
		t.Errorf("cancelled row: %+v", first)
	}
	live := deps.Departures[1]
	if !live.Live || live.At.Sub(live.Scheduled) != 2*time.Minute || live.Line != "550" || live.RouteID != "HSL:2550" || live.DirectionID != "0" {
		t.Errorf("live row: %+v", live)
	}
	sched := deps.Departures[3]
	if sched.Live || !sched.At.Equal(sched.Scheduled) {
		t.Errorf("scheduled row: %+v", sched)
	}
	if got := live.At.In(fixtureNow.Location()).Format("15:04"); got != "09:07" {
		t.Errorf("serviceDay + seconds must give 09:07 local, got %s", got)
	}
	// Stop search keeps only the city's own feed.
	stops, err := p.SearchStops(ctx, "pasila")
	if err != nil || len(stops) != 2 || stops[0].ID != "HSL:1174501" {
		t.Errorf("stops: %+v %v", stops, err)
	}
	// The key travels as a header on every request.
	for _, r := range srv.Requests() {
		if r.Header.Get(keyHeader) != "k" {
			t.Errorf("missing %s header on %s", keyHeader, r.URL)
		}
	}
}

func TestMissingAndBadKey(t *testing.T) {
	srv := transittest.NewServer(t, fixtures)
	p := newTestProvider(t, srv.URL, "")
	if _, err := p.FindRoutes(context.Background(), "550"); !errors.Is(err, transit.ErrNoKey) {
		t.Errorf("missing key: %v", err)
	}
	if len(srv.Requests()) != 0 {
		t.Errorf("no request should be made without a key")
	}
}

func TestCitiesRegistered(t *testing.T) {
	for id, router := range map[string]string{"helsinki": "hsl", "hsl": "hsl", "tampere": "waltti", "turku": "waltti", "foli": "waltti"} {
		e, ok := registry.Lookup(id)
		if !ok || e.Info.Provider != "digitransit" {
			t.Fatalf("%s: %v %+v", id, ok, e.Info)
		}
		p := New(registry.Deps{}, e.Info).(*Provider)
		if p.city.router != router {
			t.Errorf("%s: router = %s, want %s", id, p.city.router, router)
		}
	}
}

func TestLive(t *testing.T) {
	for _, c := range cities {
		c := c
		t.Run(c.info.ID, func(t *testing.T) {
			transittest.Live(t, New(registry.Deps{Key: os.Getenv, Now: time.Now}, c.info), transittest.Case{Route: c.info.Probe.Route, Query: c.info.Probe.Query})
		})
	}
}
