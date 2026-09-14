package mbta

import "github.com/bancsdan/eta/internal/xutil"

// The MBTA v3 API is a JSON:API feed: every resource is
// {id, type, attributes, relationships} and related resources arrive
// flattened in a top-level "included" array. Nullable attributes are
// pointers so odd responses decode instead of panicking.

// Data is null for stops with no parent station, so it stays a pointer.
// To-many relationships (a schedule's "added_routes") are simply absent from
// the structs below and ignored.
type rel struct {
	Data *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"data"`
}

func (r rel) id() string {
	if r.Data == nil {
		return ""
	}
	return r.Data.ID
}

type routesResp struct {
	Data []routeRes `json:"data"`
}

// short_name is "" for the subway lines and commuter rail ("Red",
// "CR-Fitchburg"); the id is the rider-facing name there.
// direction_destinations is indexed by direction_id and gives the headsign
// riders see on the platform ("Alewife", "Ashmont/Braintree").
type routeRes struct {
	ID         string `json:"id"`
	Attributes struct {
		ShortName             string   `json:"short_name"`
		LongName              string   `json:"long_name"`
		Type                  *int     `json:"type"`
		DirectionNames        []string `json:"direction_names"`
		DirectionDestinations []string `json:"direction_destinations"`
	} `json:"attributes"`
}

type stopsResp struct {
	Data []stopRes `json:"data"`
}

// For a subway or commuter rail route the feed returns parent stations
// (location_type 1, "place-pktrm"); for buses it returns the individual
// kerbside stops.
type stopRes struct {
	ID         string `json:"id"`
	Attributes struct {
		Name         string  `json:"name"`
		Municipality *string `json:"municipality"`
		PlatformCode *string `json:"platform_code"`
		PlatformName *string `json:"platform_name"`
		LocationType *int    `json:"location_type"`
	} `json:"attributes"`
	Relationships struct {
		ParentStation rel `json:"parent_station"`
	} `json:"relationships"`
}

type predictionsResp struct {
	Data     []predictionRes `json:"data"`
	Included []includedRes   `json:"included"`
}

// Both times are null on a prediction that neither arrives nor departs (a
// dropped trip); schedule_relationship is null for an ordinary running trip
// and "CANCELLED"/"SKIPPED"/"NO_DATA"/"ADDED"/"UNSCHEDULED" otherwise.
type predictionRes struct {
	ID         string `json:"id"`
	Attributes struct {
		ArrivalTime          *string `json:"arrival_time"`
		DepartureTime        *string `json:"departure_time"`
		DirectionID          *int    `json:"direction_id"`
		Status               *string `json:"status"`
		ScheduleRelationship *string `json:"schedule_relationship"`
		TripHeadsign         *string `json:"trip_headsign"`
	} `json:"attributes"`
	Relationships struct {
		Route rel `json:"route"`
		Stop  rel `json:"stop"`
		Trip  rel `json:"trip"`
	} `json:"relationships"`
}

type schedulesResp struct {
	Data     []scheduleRes `json:"data"`
	Included []includedRes `json:"included"`
}

// departure_time is null at a trip's last stop, arrival_time at its first.
type scheduleRes struct {
	ID         string `json:"id"`
	Attributes struct {
		ArrivalTime   *string `json:"arrival_time"`
		DepartureTime *string `json:"departure_time"`
		DirectionID   *int    `json:"direction_id"`
		StopHeadsign  *string `json:"stop_headsign"`
	} `json:"attributes"`
	Relationships struct {
		Route rel `json:"route"`
		Stop  rel `json:"stop"`
		Trip  rel `json:"trip"`
	} `json:"relationships"`
}

// One "included" entry covers both trips (Headsign) and stops (PlatformCode,
// PlatformName, ParentStation); the Type field says which.
type includedRes struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Headsign     *string `json:"headsign"`
		PlatformCode *string `json:"platform_code"`
		PlatformName *string `json:"platform_name"`
	} `json:"attributes"`
	Relationships struct {
		ParentStation rel `json:"parent_station"`
	} `json:"relationships"`
}

type included struct {
	trips map[string]includedRes
	stops map[string]includedRes
}

func indexIncluded(in []includedRes) included {
	ix := included{trips: map[string]includedRes{}, stops: map[string]includedRes{}}
	for _, r := range in {
		switch r.Type {
		case "trip":
			ix.trips[r.ID] = r
		case "stop":
			ix.stops[r.ID] = r
		}
	}
	return ix
}

func (ix included) headsign(tripID string) string {
	return xutil.Str(ix.trips[tripID].Attributes.Headsign)
}

// The short platform code ("1", "B") is missing on the subway, which exposes
// a platform name instead ("Ashmont/Braintree").
func (ix included) platform(stopID string) string {
	s, ok := ix.stops[stopID]
	if !ok {
		return ""
	}
	if c := xutil.Str(s.Attributes.PlatformCode); c != "" {
		return c
	}
	return xutil.Str(s.Attributes.PlatformName)
}
