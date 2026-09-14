package opendatach

// Nullable fields are pointers: the feed nulls delay, platform and the whole
// prognosis block when it has no real-time data.

type locationsResp struct {
	Stations []station `json:"stations"`
}

type station struct {
	ID         *string `json:"id"`
	Name       string  `json:"name"`
	Coordinate *struct {
		X *float64 `json:"x"` // latitude
		Y *float64 `json:"y"` // longitude
	} `json:"coordinate"`
}

type stationboardResp struct {
	Station      station `json:"station"`
	Stationboard []entry `json:"stationboard"`
}

// entry is one departure. Name is the operator's trip number, Number the
// line riders know ("7", "S3"), Category the vehicle kind ("T", "B", "S").
type entry struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Number   string `json:"number"`
	To       string `json:"to"`
	Stop     struct {
		Station struct {
			ID   *string `json:"id"`
			Name string  `json:"name"`
		} `json:"station"`
		Departure          string  `json:"departure"` // "2006-01-02T15:04:05-0700", no colon in the offset
		DepartureTimestamp *int64  `json:"departureTimestamp"`
		Delay              *int    `json:"delay"` // minutes; null without real-time data
		Platform           *string `json:"platform"`
		Prognosis          *struct {
			Departure *string `json:"departure"`
			Platform  *string `json:"platform"`
		} `json:"prognosis"`
	} `json:"stop"`
}
