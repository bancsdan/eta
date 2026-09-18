package tfi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/transit/transittest"
)

// testdata/gtfs.zip is the national feed of 2026-09-17 cut down to Dublin
// Bus 1 and 120, Go-Ahead 120, Bus Éireann 401 (Galway) and the DART, a
// dozen trips per direction, all running on Tuesday 2026-09-22.
var fixtureNow = time.Date(2026, 9, 22, 7, 45, 0, 0, time.FixedZone("IST", 3600))

const (
	oconnellLwr = "8220DB000271" // route 1 towards Shaw Street: 07:49:49 (5901_11), 08:08:19 (5901_12)
	oconnellUpr = "8220DB000278" // route 1 towards Shanard Road
)

func feedMessage(t *testing.T) []byte {
	t.Helper()
	ts := uint64(fixtureNow.Unix())
	delay := func(secs int32) *gtfs.TripUpdate_StopTimeEvent { return &gtfs.TripUpdate_StopTimeEvent{Delay: &secs} }
	canceled := gtfs.TripDescriptor_CANCELED
	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: &ts},
		Entity: []*gtfs.FeedEntity{
			{Id: proto.String("a"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("5901_12"), RouteId: proto.String("1 1 e a"), StartDate: proto.String("20260922")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{StopSequence: proto.Uint32(1), StopId: proto.String("8240DB000226"), Departure: delay(120)},
				},
			}},
			{Id: proto.String("b"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("5901_11"), RouteId: proto.String("1 1 e a"), StartDate: proto.String("20260922"), ScheduleRelationship: &canceled},
			}},
			{Id: proto.String("c"), TripUpdate: &gtfs.TripUpdate{
				// Yesterday's run of the 07:57 towards Shanard Road: must not touch today's.
				Trip: &gtfs.TripDescriptor{TripId: proto.String("5901_269"), StartDate: proto.String("20260921")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{StopSequence: proto.Uint32(1), Departure: delay(900)},
				},
			}},
		},
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

const santry = `[{"place_id":1,"lat":"53.3941","lon":"-6.2527","name":"Santry","display_name":"Santry, Dublin, Ireland","importance":0.5,"addresstype":"suburb","boundingbox":["53.3841","53.4041","-6.2727","-6.2327"],"address":{"suburb":"Santry","city":"Dublin"}}]`

func serve(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	feed := feedMessage(t)
	feedHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gtfs.zip":
			http.ServeFile(w, r, "testdata/gtfs.zip")
		case "/TripUpdates":
			feedHits++
			if r.Header.Get("x-api-key") != "k123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write(feed)
		case "/search":
			if r.URL.Query().Get("countrycodes") != "ie" || r.Header.Get("User-Agent") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.EqualFold(r.URL.Query().Get("q"), "santry") {
				_, _ = w.Write([]byte(santry))
			} else {
				_, _ = w.Write([]byte("[]"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &feedHits
}

// dir is shared by the providers of one test so the zip is indexed once.
func newProvider(t *testing.T, srv *httptest.Server, dir, town, key string) *Provider {
	t.Helper()
	d := registry.Deps{HTTP: srv.Client(), Now: func() time.Time { return fixtureNow }, Key: func(string) string { return key }}
	var p *Provider
	if e, ok := registry.LookupTown("ireland", town); ok {
		p = e.New(d).(*Provider)
	} else {
		p = NewTown(d, town).(*Provider)
	}
	p.Redirect(srv.URL, dir)
	p.Geocoder.Cache = &cache.Cache{Dir: dir, TTL: time.Hour, Now: func() time.Time { return fixtureNow }}
	return p
}

func TestConform(t *testing.T) {
	srv, _ := serve(t)
	dir := t.TempDir()
	transittest.Conform(t, newProvider(t, srv, dir, "dublin", "k123"), transittest.Case{
		Route: "1", Query: "o'connell st lwr", WantStop: "O'Connell St Lwr", WantLines: []string{"1"}, Now: fixtureNow,
	})
	transittest.Conform(t, newProvider(t, srv, dir, "galway", "k123"), transittest.Case{
		Route: "401", Query: "eyre square", WantStop: "Eyre Square", WantLines: []string{"401"}, Now: fixtureNow,
	})
}

func TestScopeAndRoutes(t *testing.T) {
	srv, _ := serve(t)
	dir := t.TempDir()
	ctx := context.Background()
	dublin := newProvider(t, srv, dir, "dublin", "k123")
	if !dublin.Cold() {
		t.Error("should be cold before the download")
	}
	rs, err := dublin.FindRoutes(ctx, "120")
	if err != nil || len(rs) != 2 {
		t.Fatalf("two 120s serve Dublin: %+v %v", rs, err)
	}
	if _, err := dublin.FindRoutes(ctx, "401"); err == nil || !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("401 is a Galway route: %v", err)
	}
	if rs, err := dublin.FindRoutes(ctx, "dart"); err != nil || len(rs) != 1 || rs[0].Mode != "rail" {
		t.Errorf("DART: %+v %v", rs, err)
	}
	if rs, err := dublin.FindRoutes(ctx, "howth"); err != nil || len(rs) != 1 || rs[0].ShortName != "DART" {
		t.Errorf("partial name match: %+v %v", rs, err)
	}
	if _, err := dublin.FindRoutes(ctx, "12"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("12 must not match 120: %v", err)
	}
	ps, err := dublin.RouteStops(ctx, transit.Route{ID: "1 1 e a", ShortName: "1"})
	if err != nil || len(ps) != 2 || ps[0].DirectionID != "0" || ps[0].Headsign != "Shaw street" || len(ps[0].Stops) != 24 {
		t.Fatalf("RouteStops: %v %v", err, ps)
	}
	if ps[0].Stops[21].ID != oconnellLwr {
		t.Errorf("stop 22 = %+v", ps[0].Stops[21])
	}
	if dublin.Cold() {
		t.Error("should be warm after the download")
	}
	galway := newProvider(t, srv, dir, "galway", "k123")
	if _, err := galway.FindRoutes(ctx, "1"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("Dublin Bus 1 in Galway: %v", err)
	}
	stops, err := galway.SearchStops(ctx, "eyre")
	if err != nil || len(stops) == 0 || !strings.Contains(stops[0].Name, "Eyre") {
		t.Errorf("Galway eyre: %+v %v", stops, err)
	}
	if stops, _ := dublin.SearchStops(ctx, "eyre square"); len(stops) > 0 && strings.Contains(stops[0].Name, "Eyre") {
		t.Errorf("Eyre Square is not in Dublin: %+v", stops)
	}
}

func TestAnyTownGeocoded(t *testing.T) {
	srv, _ := serve(t)
	dir := t.TempDir()
	ctx := context.Background()
	p := newProvider(t, srv, dir, "Santry", "k123")
	if p.Info().ID != "santry" || p.Info().Country != "ireland" {
		t.Errorf("info: %+v", p.Info())
	}
	stops, err := p.SearchStops(ctx, "shanard")
	if err != nil || len(stops) == 0 || !strings.HasPrefix(stops[0].Name, "Shanard") {
		t.Fatalf("Santry shanard: %+v %v", stops, err)
	}
	if stops, _ := p.SearchStops(ctx, "shaw street"); len(stops) > 0 && stops[0].Name == "Shaw Street" {
		t.Errorf("Shaw Street is 6 km from Santry: %+v", stops)
	}
	rs, err := p.FindRoutes(ctx, "1")
	if err != nil || len(rs) != 1 {
		t.Errorf("route 1 serves Santry: %+v %v", rs, err)
	}
	if _, err := p.FindRoutes(ctx, "401"); !errors.Is(err, transit.ErrUnknownRoute) {
		t.Errorf("no Galway 401 in Santry: %v", err)
	}
	q := newProvider(t, srv, dir, "Nowhere", "k123")
	if _, err := q.SearchStops(ctx, "x"); !errors.Is(err, transit.ErrNotFound) {
		t.Errorf("unknown town: %v", err)
	}
}

func TestDepartures(t *testing.T) {
	srv, hits := serve(t)
	dir := t.TempDir()
	ctx := context.Background()
	p := newProvider(t, srv, dir, "dublin", "k123")
	route := transit.Route{ID: "1 1 e a", ShortName: "1"}
	ds, err := p.Departures(ctx, []string{oconnellLwr, oconnellUpr}, []transit.Route{route}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ds.Now.Equal(fixtureNow) {
		t.Errorf("Now = %v", ds.Now)
	}
	if *hits != 1 {
		t.Errorf("feed fetched %d times", *hits)
	}
	byTrip := map[string]transit.Departure{}
	for _, d := range ds.Departures {
		byTrip[strings.TrimSuffix(d.TripID, "@20260922")] = d
		if d.Line != "1" || d.RouteID != route.ID || d.Headsign == "" {
			t.Errorf("bad departure %+v", d)
		}
	}
	in := func(h, m, s int) time.Time { return time.Date(2026, 9, 22, h, m, s, 0, p.loc) }
	d := byTrip["5901_12"]
	if !d.Live || !d.At.Equal(in(8, 10, 19)) || !d.Scheduled.Equal(in(8, 8, 19)) || d.Cancelled || d.DirectionID != "0" {
		t.Errorf("delayed 08:08: %+v", d)
	}
	if d := byTrip["5901_11"]; !d.Cancelled || !d.Live {
		t.Errorf("cancelled 07:49: %+v", d)
	}
	d = byTrip["5901_269"]
	if d.Live || !d.At.Equal(in(7, 57, 16)) || d.StopID != oconnellUpr || d.Headsign != "Shanard Road" || d.DirectionID != "1" {
		t.Errorf("scheduled-only 07:57 (yesterday's update ignored): %+v", d)
	}
	if _, ok := byTrip["5901_1"]; ok {
		t.Error("the 07:01 has gone")
	}
	for _, d := range ds.Departures {
		if d.At.After(fixtureNow.Add(time.Hour)) {
			t.Errorf("outside the window: %+v", d)
		}
	}
	// No route filter: the board still lists only Dublin Bus 1 here, but
	// through the route-less path.
	all, err := p.Departures(ctx, []string{oconnellLwr}, nil, 30*time.Minute)
	if err != nil || len(all.Departures) == 0 {
		t.Fatalf("board: %+v %v", all, err)
	}
	if empty, err := p.Departures(ctx, nil, nil, time.Hour); err != nil || len(empty.Departures) != 0 {
		t.Errorf("no stops: %+v %v", empty, err)
	}
}

func TestKeyErrors(t *testing.T) {
	srv, _ := serve(t)
	dir := t.TempDir()
	ctx := context.Background()
	p := newProvider(t, srv, dir, "dublin", "")
	if stops, err := p.SearchStops(ctx, "o'connell"); err != nil || len(stops) == 0 {
		t.Errorf("stop search needs no key: %v", err)
	}
	if _, err := p.Departures(ctx, []string{oconnellLwr}, nil, time.Hour); !errors.Is(err, transit.ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
	bad := newProvider(t, srv, dir, "dublin", "wrong")
	if _, err := bad.Departures(ctx, []string{oconnellLwr}, nil, time.Hour); !errors.Is(err, transit.ErrUnauthorized) {
		t.Errorf("wrong key: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	c, ok := registry.LookupCountry("ie")
	if !ok || c.ID != "ireland" || c.AnyTown == nil {
		t.Fatalf("country: %+v", c)
	}
	if _, ok := registry.LookupTown("ireland", "portlairge"); !ok {
		t.Error("Irish alias for Waterford")
	}
	info, _, err := registry.Resolve("ireland", "Sligo")
	if err != nil || info.ID != "sligo" || info.Provider != "tfi" {
		t.Errorf("Resolve Sligo: %+v %v", info, err)
	}
}

func TestLive(t *testing.T) {
	e, _ := registry.LookupTown("ireland", "dublin")
	p := e.New(registry.Deps{Key: func(env string) string { return keyFromEnv(env) }})
	transittest.Live(t, p, transittest.Case{Route: e.Info.Probe.Route, Query: e.Info.Probe.Query, WantStop: "O'Connell", WantLines: []string{"1"}})
}

func keyFromEnv(env string) string { return os.Getenv(env) }
