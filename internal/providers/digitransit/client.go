package digitransit

// GraphQL payload shapes for the Digitransit routing API v2 (OpenTripPlanner
// GTFS schema). Times come as serviceDay (unix midnight of the service day)
// plus seconds since that midnight, which may exceed 24h after midnight.

type routesData struct {
	Routes []routeDTO `json:"routes"`
}

type routeDTO struct {
	GtfsID    string `json:"gtfsId"`
	ShortName string `json:"shortName"`
	LongName  string `json:"longName"`
	Mode      string `json:"mode"`
}

type routeData struct {
	Route *struct {
		Patterns []struct {
			Code        string    `json:"code"`
			Headsign    string    `json:"headsign"`
			DirectionID int       `json:"directionId"`
			Stops       []stopDTO `json:"stops"`
		} `json:"patterns"`
	} `json:"route"`
}

type stopDTO struct {
	GtfsID        string  `json:"gtfsId"`
	Name          string  `json:"name"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
	PlatformCode  *string `json:"platformCode"`
	ParentStation *struct {
		GtfsID string `json:"gtfsId"`
	} `json:"parentStation"`
}

type stopsData struct {
	Stops []stopDTO `json:"stops"`
}

type boardData struct {
	Stops []struct {
		GtfsID    string     `json:"gtfsId"`
		Stoptimes []stoptime `json:"stoptimesWithoutPatterns"`
	} `json:"stops"`
}

type stoptime struct {
	ServiceDay         int64  `json:"serviceDay"`
	ScheduledDeparture int64  `json:"scheduledDeparture"`
	RealtimeDeparture  int64  `json:"realtimeDeparture"`
	Realtime           bool   `json:"realtime"`
	RealtimeState      string `json:"realtimeState"` // SCHEDULED, UPDATED, CANCELED, ADDED, MODIFIED
	Headsign           string `json:"headsign"`
	Trip               *struct {
		GtfsID      string `json:"gtfsId"`
		DirectionID string `json:"directionId"`
		Route       *struct {
			GtfsID    string `json:"gtfsId"`
			ShortName string `json:"shortName"`
		} `json:"route"`
	} `json:"trip"`
	Stop *struct {
		GtfsID       string  `json:"gtfsId"`
		PlatformCode *string `json:"platformCode"`
	} `json:"stop"`
}
