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

func TestTripUpdatesPredict(t *testing.T) {
	delay := func(secs int32) *gtfs.TripUpdate_StopTimeEvent { return &gtfs.TripUpdate_StopTimeEvent{Delay: &secs} }
	skipped := gtfs.TripUpdate_StopTimeUpdate_SKIPPED
	canceled := gtfs.TripDescriptor_CANCELED
	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0")},
		Entity: []*gtfs.FeedEntity{
			{Id: proto.String("1"), TripUpdate: &gtfs.TripUpdate{
				Trip: &gtfs.TripDescriptor{TripId: proto.String("t1"), RouteId: proto.String("R"), StartDate: proto.String("20260922")},
				StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
					{StopSequence: proto.Uint32(5), StopId: proto.String("S5"), Departure: delay(300)},
					{StopSequence: proto.Uint32(2), StopId: proto.String("S2"), Departure: delay(120)},
					{StopSequence: proto.Uint32(7), StopId: proto.String("S7"), ScheduleRelationship: &skipped},
					{StopSequence: proto.Uint32(9), StopId: proto.String("S9"), Arrival: &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(1_789_500_000)}},
				},
			}},
			{Id: proto.String("2"), TripUpdate: &gtfs.TripUpdate{Trip: &gtfs.TripDescriptor{TripId: proto.String("t2"), ScheduleRelationship: &canceled}}},
		},
	}
	tus := TripUpdates(msg)
	if len(tus) != 2 || !tus["t2"].Cancelled || tus["t1"].StartDate != "20260922" || tus["t1"].Calls[0].Seq != 2 {
		t.Fatalf("TripUpdates: %+v", tus)
	}
	sched := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		seq  int
		stop string
		want time.Time
		ok   bool
		skip bool
	}{
		{1, "S1", sched, false, false},                       // before the first update: schedule stands
		{2, "S2", sched.Add(2 * time.Minute), true, false},   // update at the stop
		{3, "S3", sched.Add(2 * time.Minute), true, false},   // delay carries forward
		{5, "S5", sched.Add(5 * time.Minute), true, false},   // next update replaces it
		{7, "S7", sched, true, true},                         // skipped
		{8, "S8", sched.Add(5 * time.Minute), true, false},   // a skip does not change the delay
		{9, "S9", time.Unix(1_789_500_000, 0), true, false},  // absolute time at the stop
		{10, "S10", sched.Add(5 * time.Minute), true, false}, // absolute times do not carry forward, the last delay does
	}
	for _, c := range cases {
		at, ok, skip := tus["t1"].Predict(c.seq, c.stop, sched)
		if !at.Equal(c.want) || ok != c.ok || skip != c.skip {
			t.Errorf("Predict(%d,%s) = %v,%v,%v want %v,%v,%v", c.seq, c.stop, at, ok, skip, c.want, c.ok, c.skip)
		}
	}
}
