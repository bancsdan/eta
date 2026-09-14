package bvg

// Every field the feed can null out is a pointer so odd responses decode
// instead of panicking.

// GET /locations also returns points of interest and addresses, which we ask
// it not to and skip anyway.
type apiLocation struct {
	Type     string `json:"type"` // "stop", "station", "poi", "location"
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	Products map[string]bool `json:"products"`
}

// Name is the public label riders use: "M41"/"100" for buses, "S3", "U2",
// "RE1".
type apiLine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Product string `json:"product"`
	Mode    string `json:"mode"`
}

// When is null for a cancelled trip; Delay is null when the trip has no
// realtime data at all.
type apiDeparture struct {
	TripID string `json:"tripId"`
	Stop   *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"stop"`
	When            *string  `json:"when"`
	PlannedWhen     *string  `json:"plannedWhen"`
	Delay           *int     `json:"delay"`
	Platform        *string  `json:"platform"`
	PlannedPlatform *string  `json:"plannedPlatform"`
	Direction       *string  `json:"direction"`
	Line            *apiLine `json:"line"`
	Cancelled       *bool    `json:"cancelled"`
}

type apiDepartures struct {
	Departures []apiDeparture `json:"departures"`
}
