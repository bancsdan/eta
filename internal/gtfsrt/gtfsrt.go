// Package gtfsrt fetches GTFS-Realtime TripUpdate feeds and flattens the
// stop-time updates for a set of stops into a form providers can map.
package gtfsrt

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/bancsdan/eta/internal/httpx"
)

// Fetch downloads and decodes one feed.
func Fetch(ctx context.Context, c *httpx.Client, url string) (*gtfs.FeedMessage, error) {
	body, err := c.GetBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("%s: bad GTFS-RT feed: %v", c.Name, err)
	}
	return &msg, nil
}

// Update is one predicted call at a stop of interest.
type Update struct {
	TripID      string
	RouteID     string
	DirectionID string // "0"/"1" when the feed sets direction_id, else ""
	StopID      string
	At          time.Time
	Cancelled   bool
	LastStopID  string // final stop of the trip update, a headsign fallback
}

// Timestamp returns the feed header's generation time, or zero.
func Timestamp(m *gtfs.FeedMessage) time.Time {
	if m == nil || m.Header == nil || m.Header.Timestamp == nil || *m.Header.Timestamp == 0 {
		return time.Time{}
	}
	return time.Unix(int64(*m.Header.Timestamp), 0)
}

// StopUpdates returns every stop-time update at one of stopIDs. The time is
// the departure when given, else the arrival; delay-only entries without a
// time are skipped. SKIPPED stops and CANCELED trips are marked Cancelled.
func StopUpdates(m *gtfs.FeedMessage, stopIDs map[string]bool) []Update {
	var out []Update
	for _, e := range m.GetEntity() {
		tu := e.GetTripUpdate()
		if tu == nil {
			continue
		}
		trip := tu.GetTrip()
		tripCancelled := trip.GetScheduleRelationship() == gtfs.TripDescriptor_CANCELED
		dir := ""
		if trip.DirectionId != nil {
			dir = fmt.Sprint(trip.GetDirectionId())
		}
		stus := tu.GetStopTimeUpdate()
		last := ""
		if len(stus) > 0 {
			last = stus[len(stus)-1].GetStopId()
		}
		for _, stu := range stus {
			sid := stu.GetStopId()
			if !stopIDs[sid] {
				continue
			}
			var secs int64
			if d := stu.GetDeparture(); d != nil && d.Time != nil {
				secs = d.GetTime()
			} else if a := stu.GetArrival(); a != nil && a.Time != nil {
				secs = a.GetTime()
			}
			if secs == 0 {
				continue
			}
			out = append(out, Update{
				TripID:      trip.GetTripId(),
				RouteID:     trip.GetRouteId(),
				DirectionID: dir,
				StopID:      sid,
				At:          time.Unix(secs, 0),
				Cancelled:   tripCancelled || stu.GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SKIPPED,
				LastStopID:  last,
			})
		}
	}
	return out
}

// TripUpdate is one trip's realtime state, for feeds that report delays
// against the static schedule rather than absolute times.
type TripUpdate struct {
	TripID    string
	RouteID   string
	StartDate string // YYYYMMDD when the feed sets it
	Cancelled bool
	Calls     []Call // by stop sequence
}

// Call is one stop-time update.
type Call struct {
	Seq      int
	StopID   string
	Delay    int   // seconds; valid when HasDelay
	Time     int64 // unix seconds; 0 when absent
	HasDelay bool
	Skipped  bool
}

// TripUpdates indexes the feed by trip id. ADDED trips without a static
// counterpart are included too; callers match on TripID (and StartDate).
func TripUpdates(m *gtfs.FeedMessage) map[string]*TripUpdate {
	out := map[string]*TripUpdate{}
	for _, e := range m.GetEntity() {
		tu := e.GetTripUpdate()
		if tu == nil {
			continue
		}
		trip := tu.GetTrip()
		t := &TripUpdate{
			TripID:    trip.GetTripId(),
			RouteID:   trip.GetRouteId(),
			StartDate: trip.GetStartDate(),
			Cancelled: trip.GetScheduleRelationship() == gtfs.TripDescriptor_CANCELED,
		}
		for _, stu := range tu.GetStopTimeUpdate() {
			c := Call{Seq: int(stu.GetStopSequence()), StopID: stu.GetStopId(), Skipped: stu.GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SKIPPED}
			ev := stu.GetDeparture()
			if ev == nil {
				ev = stu.GetArrival()
			}
			if ev != nil {
				if ev.Time != nil {
					c.Time = ev.GetTime()
				}
				if ev.Delay != nil {
					c.Delay, c.HasDelay = int(ev.GetDelay()), true
				}
			}
			t.Calls = append(t.Calls, c)
		}
		sort.SliceStable(t.Calls, func(i, j int) bool { return t.Calls[i].Seq < t.Calls[j].Seq })
		out[t.TripID] = t
	}
	return out
}

// Predict applies the trip's updates to the scheduled call at seq/stopID:
// an update at the stop itself wins, else the latest earlier delay carries
// forward (the GTFS-RT propagation rule). ok is false when no update
// covers the stop yet, in which case the schedule stands.
func (t *TripUpdate) Predict(seq int, stopID string, scheduled time.Time) (at time.Time, ok, skipped bool) {
	var atStop, lastDelay *Call
	for i := range t.Calls {
		c := &t.Calls[i]
		if c.StopID == stopID || (c.StopID == "" && c.Seq == seq) {
			atStop = c
			break
		}
		if c.Seq != 0 && c.Seq > seq {
			break
		}
		if c.HasDelay {
			lastDelay = c
		}
	}
	if atStop != nil {
		switch {
		case atStop.Skipped:
			return scheduled, true, true
		case atStop.Time != 0:
			return time.Unix(atStop.Time, 0), true, false
		case atStop.HasDelay:
			return scheduled.Add(time.Duration(atStop.Delay) * time.Second), true, false
		}
	}
	if lastDelay != nil {
		return scheduled.Add(time.Duration(lastDelay.Delay) * time.Second), true, false
	}
	return scheduled, false, false
}
