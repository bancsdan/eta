package gtfs

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/transit"
)

// Index is a one-pass digest of stop_times.txt kept next to the zip: the
// scheduled calls at every stop and one pattern per direction of every
// route. National feeds run to hundreds of megabytes of stop times, so the
// digest is built once per download (Build) and read piecemeal (StopTimes
// reads only one stop's slice of the record file).
type Index struct {
	Feed     *Feed
	Progress io.Writer

	mu   sync.Mutex
	meta *indexMeta
}

// StopTime is one scheduled call at a stop.
type StopTime struct {
	TripID    string
	Departure int // seconds after the service day's midnight; may exceed 24h
	Seq       int
}

type indexMeta struct {
	ZipModTime int64             `json:"zipModTime"`
	Trips      []string          `json:"trips"`
	Stops      map[string][2]int `json:"stops"` // stop id → first record, count
}

const recordSize = 12 // trip index, departure seconds, stop sequence: three uint32s

func (x *Index) metaPath() string     { return x.Feed.Path + ".index.json" }
func (x *Index) recordsPath() string  { return x.Feed.Path + ".index.bin" }
func (x *Index) patternsPath() string { return x.Feed.Path + ".patterns.json" }

// Cold reports whether the zip must be downloaded or the digest rebuilt.
func (x *Index) Cold() bool {
	if x.Feed.Cold() {
		return true
	}
	zi, err := os.Stat(x.Feed.Path)
	if err != nil {
		return true
	}
	b, err := os.ReadFile(x.metaPath())
	if err != nil {
		return true
	}
	var m indexMeta
	if json.Unmarshal(b, &m) != nil || m.ZipModTime != zi.ModTime().Unix() {
		return true
	}
	_, err = os.Stat(x.recordsPath())
	return err != nil
}

// Ensure downloads the zip when needed and (re)builds the digest when it
// is missing or belongs to an older zip.
func (x *Index) Ensure(ctx context.Context) error {
	if err := x.Feed.Ensure(ctx); err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.meta != nil {
		return nil
	}
	if !x.Cold() {
		b, err := os.ReadFile(x.metaPath())
		if err == nil {
			var m indexMeta
			if json.Unmarshal(b, &m) == nil {
				x.meta = &m
				return nil
			}
		}
	}
	return x.build(ctx)
}

type record struct {
	stop, trip, dep, seq uint32
}

func (x *Index) build(ctx context.Context) error {
	if x.Progress != nil {
		fmt.Fprintf(x.Progress, "eta: indexing the timetable (once per download)\n")
	}
	trips, err := x.Feed.Trips(ctx)
	if err != nil {
		return err
	}
	tripIDs := make([]string, 0, len(trips))
	for id := range trips {
		tripIDs = append(tripIDs, id)
	}
	sort.Strings(tripIDs)
	tripIdx := make(map[string]uint32, len(tripIDs))
	for i, id := range tripIDs {
		tripIdx[id] = uint32(i)
	}
	stopIDs := []string{}
	stopIdx := map[string]uint32{}
	var recs []record
	err = x.Feed.rows("stop_times.txt", []string{"trip_id", "departure_time", "arrival_time", "stop_id", "stop_sequence"}, func(v []string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ti, ok := tripIdx[v[0]]
		if !ok {
			return nil
		}
		dep, ok := parseClock(v[1])
		if !ok {
			if dep, ok = parseClock(v[2]); !ok {
				return nil
			}
		}
		si, ok := stopIdx[v[3]]
		if !ok {
			si = uint32(len(stopIDs))
			stopIdx[v[3]] = si
			stopIDs = append(stopIDs, v[3])
		}
		recs = append(recs, record{stop: si, trip: ti, dep: dep, seq: parseUint(v[4])})
		return nil
	})
	if err != nil {
		return err
	}
	if err := x.writePatterns(ctx, recs, tripIDs, stopIDs, trips); err != nil {
		return err
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].stop != recs[j].stop {
			return recs[i].stop < recs[j].stop
		}
		return recs[i].dep < recs[j].dep
	})
	zi, err := os.Stat(x.Feed.Path)
	if err != nil {
		return err
	}
	m := &indexMeta{ZipModTime: zi.ModTime().Unix(), Trips: tripIDs, Stops: make(map[string][2]int, len(stopIDs))}
	f, err := os.Create(x.recordsPath() + ".tmp")
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	buf := make([]byte, recordSize)
	for i, r := range recs {
		if i == 0 || recs[i-1].stop != r.stop {
			m.Stops[stopIDs[r.stop]] = [2]int{i, 0}
		}
		e := m.Stops[stopIDs[r.stop]]
		e[1]++
		m.Stops[stopIDs[r.stop]] = e
		binary.LittleEndian.PutUint32(buf[0:], r.trip)
		binary.LittleEndian.PutUint32(buf[4:], r.dep)
		binary.LittleEndian.PutUint32(buf[8:], r.seq)
		if _, err := w.Write(buf); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(x.recordsPath()+".tmp", x.recordsPath()); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(x.metaPath(), b, 0o600); err != nil {
		return err
	}
	x.meta = m
	return nil
}

// writePatterns keeps, per route and direction, the trip with the most
// calls and stores its stops in travel order.
func (x *Index) writePatterns(ctx context.Context, recs []record, tripIDs, stopIDs []string, trips map[string]Trip) error {
	stops, err := x.Feed.Stops(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]transit.Stop, len(stops))
	for _, s := range stops {
		byID[s.ID] = s
	}
	counts := make([]int, len(tripIDs))
	for _, r := range recs {
		counts[r.trip]++
	}
	type key struct{ route, dir string }
	longest := map[key]uint32{}
	for i, id := range tripIDs {
		if counts[i] == 0 {
			continue
		}
		t := trips[id]
		k := key{t.RouteID, t.DirectionID}
		if cur, ok := longest[k]; !ok || counts[i] > counts[cur] || (counts[i] == counts[cur] && id < tripIDs[cur]) {
			longest[k] = uint32(i)
		}
	}
	winner := make(map[uint32]key, len(longest))
	for k, ti := range longest {
		winner[ti] = k
	}
	calls := map[uint32][]record{}
	for _, r := range recs {
		if _, ok := winner[r.trip]; ok {
			calls[r.trip] = append(calls[r.trip], r)
		}
	}
	out := map[string][]transit.Pattern{}
	for ti, k := range winner {
		rs := calls[ti]
		sort.Slice(rs, func(i, j int) bool { return rs[i].seq < rs[j].seq })
		p := transit.Pattern{DirectionID: k.dir, Headsign: trips[tripIDs[ti]].Headsign}
		for _, r := range rs {
			if s, ok := byID[stopIDs[r.stop]]; ok {
				p.Stops = append(p.Stops, s)
			}
		}
		if p.Headsign == "" && len(p.Stops) > 0 {
			p.Headsign = p.Stops[len(p.Stops)-1].Name
		}
		if len(p.Stops) > 0 {
			out[k.route] = append(out[k.route], p)
		}
	}
	for _, ps := range out {
		sort.Slice(ps, func(i, j int) bool { return ps[i].DirectionID < ps[j].DirectionID })
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(x.patternsPath(), b, 0o600)
}

// StopTimes returns every scheduled call at stopID, by departure time.
func (x *Index) StopTimes(ctx context.Context, stopID string) ([]StopTime, error) {
	if err := x.Ensure(ctx); err != nil {
		return nil, err
	}
	x.mu.Lock()
	m := x.meta
	x.mu.Unlock()
	e, ok := m.Stops[stopID]
	if !ok {
		return nil, nil
	}
	f, err := os.Open(x.recordsPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, e[1]*recordSize)
	if _, err := f.ReadAt(buf, int64(e[0]*recordSize)); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := make([]StopTime, 0, e[1])
	for i := 0; i < e[1]; i++ {
		b := buf[i*recordSize:]
		ti := binary.LittleEndian.Uint32(b[0:])
		if int(ti) >= len(m.Trips) {
			continue
		}
		out = append(out, StopTime{
			TripID:    m.Trips[ti],
			Departure: int(binary.LittleEndian.Uint32(b[4:])),
			Seq:       int(binary.LittleEndian.Uint32(b[8:])),
		})
	}
	return out, nil
}

// Patterns returns the stored patterns of every route.
func (x *Index) Patterns(ctx context.Context) (map[string][]transit.Pattern, error) {
	if err := x.Ensure(ctx); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(x.patternsPath())
	if err != nil {
		return nil, err
	}
	var out map[string][]transit.Pattern
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("gtfs: %s: %v", x.patternsPath(), err)
	}
	return out, nil
}

// ServiceDay is the midnight that starts day in loc, robust to DST shifts:
// GTFS times count from noon minus twelve hours.
func ServiceDay(day time.Time, loc *time.Location) time.Time {
	y, m, d := day.In(loc).Date()
	return time.Date(y, m, d, 12, 0, 0, 0, loc).Add(-12 * time.Hour)
}

// parseClock reads "HH:MM:SS" (hours may exceed 23) as seconds.
func parseClock(s string) (uint32, bool) {
	var parts [3]uint32
	n := 0
	cur := uint32(0)
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			cur = cur*10 + uint32(c-'0')
			digits++
		case c == ':' && n < 2 && digits > 0:
			parts[n] = cur
			n++
			cur, digits = 0, 0
		default:
			return 0, false
		}
	}
	if n != 2 || digits == 0 {
		return 0, false
	}
	parts[2] = cur
	return parts[0]*3600 + parts[1]*60 + parts[2], true
}

func parseUint(s string) uint32 {
	var v uint32
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return v
		}
		v = v*10 + uint32(s[i]-'0')
	}
	return v
}
