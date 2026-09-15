// Package gtfs reads the parts of a static GTFS zip that eta needs (stops,
// routes, trips, stop_times), straight out of the archive, and caches the
// zip on disk with a TTL so a city's first run is the only slow one.
package gtfs

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/transit"
)

// Feed is one static GTFS zip.
type Feed struct {
	URL      string
	Path     string        // where the zip is kept, e.g. <cache>/gtfs/subway.zip
	TTL      time.Duration // re-download when the file is older; 0 means never
	HTTP     *httpx.Client
	Progress io.Writer // "downloading…" note on a cold start; nil is silent
	Now      func() time.Time

	mu    sync.Mutex
	stops []transit.Stop
	trips map[string]Trip
}

// Trip is the per-trip metadata departures need to label a board.
type Trip struct {
	RouteID     string
	DirectionID string
	Headsign    string
}

// Cold reports whether Ensure would download.
func (f *Feed) Cold() bool {
	fi, err := os.Stat(f.Path)
	if err != nil {
		return true
	}
	return f.TTL > 0 && f.now().Sub(fi.ModTime()) > f.TTL
}

func (f *Feed) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Ensure downloads the zip when missing or stale, atomically.
func (f *Feed) Ensure(ctx context.Context) error {
	if !f.Cold() {
		return nil
	}
	if f.Progress != nil {
		fmt.Fprintf(f.Progress, "eta: downloading %s (first run, or older than %s)\n", f.URL, f.TTL)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	body, err := f.HTTP.GetBytes(ctx, f.URL)
	if err != nil {
		return err
	}
	if _, err := zip.NewReader(strings.NewReader(string(body)), int64(len(body))); err != nil {
		return fmt.Errorf("gtfs: %s is not a zip: %v", f.URL, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".gtfs-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	f.mu.Lock()
	f.stops, f.trips = nil, nil
	f.mu.Unlock()
	return os.Rename(tmp.Name(), f.Path)
}

// Stops returns every stop and station (location_type 0 and 1), memoised.
func (f *Feed) Stops(ctx context.Context) ([]transit.Stop, error) {
	if err := f.Ensure(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stops != nil {
		return f.stops, nil
	}
	var out []transit.Stop
	err := f.each("stops.txt", func(rec map[string]string) error {
		if lt := rec["location_type"]; lt != "" && lt != "0" && lt != "1" {
			return nil
		}
		s := transit.Stop{ID: rec["stop_id"], Name: rec["stop_name"], ParentID: rec["parent_station"]}
		fmt.Sscan(rec["stop_lat"], &s.Lat) //nolint:errcheck // optional column
		fmt.Sscan(rec["stop_lon"], &s.Lon) //nolint:errcheck
		if s.ID != "" && s.Name != "" {
			out = append(out, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	f.stops = out
	return out, nil
}

// Routes returns routes.txt as transit routes; ShortName falls back to
// route_long_name, then route_id.
func (f *Feed) Routes(ctx context.Context) ([]transit.Route, error) {
	if err := f.Ensure(ctx); err != nil {
		return nil, err
	}
	var out []transit.Route
	err := f.each("routes.txt", func(rec map[string]string) error {
		r := transit.Route{ID: rec["route_id"], ShortName: rec["route_short_name"], LongName: rec["route_long_name"]}
		if r.ShortName == "" {
			r.ShortName = r.LongName
		}
		if r.ShortName == "" {
			r.ShortName = r.ID
		}
		var rt int
		if _, err := fmt.Sscan(rec["route_type"], &rt); err == nil {
			r.Mode = transit.ModeFromGTFS(rt)
		}
		if r.ID != "" {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// Trips returns trip_id → metadata, memoised.
func (f *Feed) Trips(ctx context.Context) (map[string]Trip, error) {
	if err := f.Ensure(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trips != nil {
		return f.trips, nil
	}
	out := map[string]Trip{}
	err := f.each("trips.txt", func(rec map[string]string) error {
		out[rec["trip_id"]] = Trip{RouteID: rec["route_id"], DirectionID: rec["direction_id"], Headsign: rec["trip_headsign"]}
		return nil
	})
	if err != nil {
		return nil, err
	}
	f.trips = out
	return out, nil
}

// Patterns derives one pattern per direction of routeID: the stops of the
// longest trip in that direction, in stop_sequence order. Reading
// stop_times.txt is the expensive part, so callers should cache the result.
func (f *Feed) Patterns(ctx context.Context, routeID string) ([]transit.Pattern, error) {
	trips, err := f.Trips(ctx)
	if err != nil {
		return nil, err
	}
	stops, err := f.Stops(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]transit.Stop, len(stops))
	for _, s := range stops {
		byID[s.ID] = s
	}
	want := map[string]bool{}
	for id, t := range trips {
		if t.RouteID == routeID {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("gtfs: route %s: %w", routeID, transit.ErrNotFound)
	}
	type seqStop struct {
		seq  int
		stop string
	}
	perTrip := map[string][]seqStop{}
	err = f.each("stop_times.txt", func(rec map[string]string) error {
		id := rec["trip_id"]
		if !want[id] {
			return nil
		}
		var seq int
		fmt.Sscan(rec["stop_sequence"], &seq) //nolint:errcheck
		perTrip[id] = append(perTrip[id], seqStop{seq, rec["stop_id"]})
		return nil
	})
	if err != nil {
		return nil, err
	}
	longest := map[string]string{} // direction → trip id
	for id, sts := range perTrip {
		d := trips[id].DirectionID
		if cur, ok := longest[d]; !ok || len(sts) > len(perTrip[cur]) || (len(sts) == len(perTrip[cur]) && id < cur) {
			longest[d] = id
		}
	}
	dirs := make([]string, 0, len(longest))
	for d := range longest {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var out []transit.Pattern
	for _, d := range dirs {
		id := longest[d]
		sts := perTrip[id]
		sort.Slice(sts, func(i, j int) bool { return sts[i].seq < sts[j].seq })
		p := transit.Pattern{DirectionID: d, Headsign: trips[id].Headsign}
		for _, st := range sts {
			if s, ok := byID[st.stop]; ok {
				p.Stops = append(p.Stops, s)
			}
		}
		if p.Headsign == "" && len(p.Stops) > 0 {
			p.Headsign = p.Stops[len(p.Stops)-1].Name
		}
		if len(p.Stops) > 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

// each streams one CSV file of the zip, calling fn with a header-keyed
// record per row.
func (f *Feed) each(name string, fn func(map[string]string) error) error {
	zr, err := zip.OpenReader(f.Path)
	if err != nil {
		return fmt.Errorf("gtfs: %s: %v", f.Path, err)
	}
	defer func() { _ = zr.Close() }()
	var file *zip.File
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) == name {
			file = zf
			break
		}
	}
	if file == nil {
		return fmt.Errorf("gtfs: %s has no %s", filepath.Base(f.Path), name)
	}
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	r := csv.NewReader(rc)
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	first, err := r.Read()
	if err != nil {
		return fmt.Errorf("gtfs: %s: %v", name, err)
	}
	// ReuseRecord recycles the returned slice on the next Read, so the
	// header must be copied before the rows overwrite it.
	header := append([]string(nil), first...)
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	rec := make(map[string]string, len(header))
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("gtfs: %s: %v", name, err)
		}
		for i, h := range header {
			if i < len(row) {
				rec[h] = row[i]
			} else {
				rec[h] = ""
			}
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// Client is a helper to build a Feed's HTTP client with the provider's name.
func Client(name string, h *http.Client) *httpx.Client { return httpx.New(name, h) }
