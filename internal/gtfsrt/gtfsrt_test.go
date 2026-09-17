package gtfsrt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/bancsdan/eta/internal/httpx"
)

func TestFetchAndStopUpdates(t *testing.T) {
	ts := uint64(1_789_500_000)
	dep := func(secs int64) *gtfs.TripUpdate_StopTimeEvent { return &gtfs.TripUpdate_StopTimeEvent{Time: &secs} }
	skipped := gtfs.TripUpdate_StopTimeUpdate_SKIPPED
	canceled := gtfs.TripDescriptor_CANCELED
	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: &ts},
		Entity: []*gtfs.FeedEntity{
			{Id: proto.String("1"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("t1"), RouteId: proto.String("L"), DirectionId: proto.Uint32(1)},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{StopId: proto.String("L01N"), Departure: dep(1_789_500_100)},
					{StopId: proto.String("L02N"), Arrival: dep(1_789_500_200)},
					{StopId: proto.String("L03N"), Departure: &gtfs.TripUpdate_StopTimeEvent{Delay: proto.Int32(30)}},
					{StopId: proto.String("L06N"), Departure: dep(1_789_500_400), ScheduleRelationship: &skipped},
				},
			}},
			{Id: proto.String("2"), TripUpdate: &gtfs.TripUpdate{
				Trip:           &gtfs.TripDescriptor{TripId: proto.String("t2"), RouteId: proto.String("L"), ScheduleRelationship: &canceled},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{{StopId: proto.String("L02N"), Departure: dep(1_789_500_500)}},
			}},
			{Id: proto.String("3"), Vehicle: &gtfs.VehiclePosition{}},
		},
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) }))
	defer srv.Close()
	got, err := Fetch(context.Background(), httpx.New("t", srv.Client()), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !Timestamp(got).Equal(time.Unix(int64(ts), 0)) {
		t.Errorf("timestamp = %v", Timestamp(got))
	}
	ups := StopUpdates(got, map[string]bool{"L02N": true, "L03N": true, "L06N": true})
	if len(ups) != 3 {
		t.Fatalf("updates = %+v", ups)
	}
	if ups[0].StopID != "L02N" || ups[0].At.Unix() != 1_789_500_200 || ups[0].LastStopID != "L06N" || ups[0].Cancelled || ups[0].DirectionID != "1" {
		t.Errorf("arrival fallback: %+v", ups[0])
	}
	if !ups[1].Cancelled || ups[1].StopID != "L06N" {
		t.Errorf("skipped stop: %+v", ups[1])
	}
	if !ups[2].Cancelled || ups[2].TripID != "t2" || ups[2].DirectionID != "" {
		t.Errorf("cancelled trip: %+v", ups[2])
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer bad.Close()
	if _, err := Fetch(context.Background(), httpx.New("t", bad.Client()), bad.URL); err == nil {
		t.Errorf("garbage should not decode")
	}
}
