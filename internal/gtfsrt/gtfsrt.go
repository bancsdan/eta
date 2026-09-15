// Package gtfsrt fetches GTFS-Realtime TripUpdate feeds and flattens the
// stop-time updates for a set of stops into a form providers can map.
package gtfsrt

import (
	"context"
	"fmt"
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
	TripID     string
	RouteID    string
	StopID     string
	At         time.Time
	Cancelled  bool
	LastStopID string // final stop of the trip update, a headsign fallback
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
				TripID:     trip.GetTripId(),
				RouteID:    trip.GetRouteId(),
				StopID:     sid,
				At:         time.Unix(secs, 0),
				Cancelled:  tripCancelled || stu.GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SKIPPED,
				LastStopID: last,
			})
		}
	}
	return out
}
