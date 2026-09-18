package gtfs

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
)

func synthZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"stops.txt": "\ufeffstop_id,stop_name,stop_lat,stop_lon,location_type,parent_station\n" +
			"P1,Alpha,1.0,2.0,1,\nP1N,Alpha,1.0,2.0,,P1\nP1S,Alpha,1.0,2.0,,P1\n" +
			"P2,Beta,1.1,2.1,1,\nP2N,Beta,1.1,2.1,,P2\nP2S,Beta,1.1,2.1,,P2\n" +
			"P3,Gamma,1.2,2.2,1,\nP3N,Gamma,1.2,2.2,,P3\nP3S,Gamma,1.2,2.2,,P3\nX9,Entrance,0,0,2,P1\n",
		"routes.txt": "route_id,route_short_name,route_long_name,route_type\nL,L,Canarsie,1\nGS,S,42 St Shuttle,1\n",
		"trips.txt": "route_id,trip_id,service_id,trip_headsign,direction_id\n" +
			"L,t1,wk,Gamma,0\nL,t2,wk,Gamma,0\nL,t3,wk,Alpha,1\nGS,t4,wk,Shuttle,0\n",
		"stop_times.txt": "trip_id,stop_id,arrival_time,departure_time,stop_sequence\n" +
			"t1,P1N,08:00:00,08:00:00,1\nt1,P2N,08:05:00,08:05:00,2\nt1,P3N,08:10:00,08:10:00,3\n" +
			"t2,P2N,09:05:00,09:05:00,1\nt2,P3N,09:10:00,09:10:00,2\n" +
			"t3,P3S,08:00:00,08:00:00,1\nt3,P2S,08:05:00,08:05:00,2\nt3,P1S,08:10:00,08:10:00,3\n" +
			"t4,P1N,08:00:00,08:00:00,1\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"wk,1,1,1,1,1,0,0,20260901,20261231\n",
		"calendar_dates.txt": "service_id,date,exception_type\nwk,20260916,2\nwk,20260919,1\n",
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestFeed(t *testing.T) {
	data := synthZip(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var progress bytes.Buffer
	f := &Feed{URL: srv.URL + "/gtfs.zip", Path: filepath.Join(t.TempDir(), "gtfs", "x.zip"), TTL: 24 * time.Hour,
		HTTP: httpx.New("test", srv.Client()), Progress: &progress, Now: func() time.Time { return now }}
	if !f.Cold() {
		t.Fatal("should be cold before download")
	}
	ctx := context.Background()
	stops, err := f.Stops(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 9 || stops[1].ParentID != "P1" || stops[0].Lat != 1.0 {
		t.Errorf("stops = %+v", stops)
	}
	if f.Cold() || hits != 1 || !bytes.Contains(progress.Bytes(), []byte("downloading")) {
		t.Errorf("cold=%v hits=%d progress=%q", f.Cold(), hits, progress.String())
	}
	routes, err := f.Routes(ctx)
	if err != nil || len(routes) != 2 || routes[0].Mode != "metro" || routes[1].ShortName != "S" {
		t.Errorf("routes = %+v %v", routes, err)
	}
	trips, err := f.Trips(ctx)
	if err != nil || trips["t3"].Headsign != "Alpha" || trips["t3"].DirectionID != "1" {
		t.Errorf("trips = %+v %v", trips["t3"], err)
	}
	ps, err := f.Patterns(ctx, "L")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || len(ps[0].Stops) != 3 || ps[0].Stops[0].ID != "P1N" || ps[0].Headsign != "Gamma" || ps[1].Stops[0].ID != "P3S" {
		t.Errorf("patterns = %+v", ps)
	}
	if _, err := f.Patterns(ctx, "nope"); err == nil {
		t.Errorf("unknown route should fail")
	}
	// Second Stops call is memoised; a stale file is downloaded again.
	if _, err := f.Stops(ctx); err != nil || hits != 1 {
		t.Errorf("memo: hits=%d err=%v", hits, err)
	}
	_ = os.Chtimes(f.Path, now.Add(-48*time.Hour), now.Add(-48*time.Hour))
	if !f.Cold() {
		t.Errorf("48h old file should be cold with a 24h TTL")
	}
	if err := f.Ensure(ctx); err != nil || hits != 2 {
		t.Errorf("re-download: hits=%d err=%v", hits, err)
	}
}

func TestCalendar(t *testing.T) {
	data := synthZip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	f := &Feed{URL: srv.URL, Path: filepath.Join(t.TempDir(), "x.zip"), HTTP: httpx.New("test", srv.Client())}
	c, err := f.Calendar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[time.Time]bool{
		time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC): true,  // Tuesday
		time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC): false, // removed by exception
		time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC): true,  // Saturday, added
		time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC): false, // Sunday
		time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC):  false, // after end_date
	}
	for day, want := range cases {
		if got := c.Active(day)["wk"]; got != want {
			t.Errorf("%s: active=%v want %v", day.Format("2006-01-02 Mon"), got, want)
		}
	}
}

func TestIndex(t *testing.T) {
	data := synthZip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer srv.Close()
	dir := t.TempDir()
	f := &Feed{URL: srv.URL, Path: filepath.Join(dir, "x.zip"), HTTP: httpx.New("test", srv.Client())}
	var progress bytes.Buffer
	x := &Index{Feed: f, Progress: &progress}
	if !x.Cold() {
		t.Fatal("index should be cold before the download")
	}
	ctx := context.Background()
	sts, err := x.StopTimes(ctx, "P2N")
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 2 || sts[0].TripID != "t1" || sts[0].Departure != 8*3600+300 || sts[0].Seq != 2 || sts[1].TripID != "t2" {
		t.Errorf("P2N stop times: %+v", sts)
	}
	if sts, _ := x.StopTimes(ctx, "nope"); len(sts) != 0 {
		t.Errorf("unknown stop: %+v", sts)
	}
	ps, err := x.Patterns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := ps["L"]
	if len(l) != 2 || l[0].DirectionID != "0" || len(l[0].Stops) != 3 || l[0].Stops[2].Name != "Gamma" || l[1].Headsign != "Alpha" {
		t.Errorf("L patterns: %+v", l)
	}
	if !strings.Contains(progress.String(), "indexing") {
		t.Errorf("progress: %q", progress.String())
	}
	if x.Cold() {
		t.Error("index should be warm after the build")
	}
	y := &Index{Feed: &Feed{URL: srv.URL, Path: f.Path, HTTP: httpx.New("test", srv.Client())}}
	if y.Cold() {
		t.Error("a fresh Index over the same zip should reuse the digest")
	}
	if sts, err := y.StopTimes(ctx, "P1S"); err != nil || len(sts) != 1 || sts[0].TripID != "t3" {
		t.Errorf("reused digest: %+v %v", sts, err)
	}
	for _, c := range []struct {
		in   string
		want uint32
		ok   bool
	}{{"08:05:00", 29100, true}, {"25:00:30", 90030, true}, {"7:00:00", 25200, true}, {"", 0, false}, {"08:05", 0, false}, {"x", 0, false}} {
		if got, ok := parseClock(c.in); got != c.want || ok != c.ok {
			t.Errorf("parseClock(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
	loc, _ := time.LoadLocation("Europe/Dublin")
	// DST ends at 02:00 on 2026-10-25: the service day is 25 hours long
	// and 08:00:00 in the timetable still means 08:00 on the wall clock.
	sd := ServiceDay(time.Date(2026, 10, 25, 3, 0, 0, 0, loc), loc)
	if at := sd.Add(8 * time.Hour); at.Hour() != 8 || at.Day() != 25 {
		t.Errorf("ServiceDay+8h = %v", at)
	}
	if at := ServiceDay(time.Date(2026, 3, 29, 12, 0, 0, 0, loc), loc).Add(8 * time.Hour); at.Hour() != 8 {
		t.Errorf("DST start: ServiceDay+8h = %v", at)
	}
}
