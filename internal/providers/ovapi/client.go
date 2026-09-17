package ovapi

// areaEntry is one row of GET /stopareacode: a stop area with its town.
type areaEntry struct {
	StopAreaCode    string  `json:"StopAreaCode"`
	TimingPointName string  `json:"TimingPointName"`
	TimingPointTown string  `json:"TimingPointTown"`
	Latitude        float64 `json:"Latitude"`
	Longitude       float64 `json:"Longitude"`
}

// timingPoint is one platform of a stop area, with its upcoming passes.
type timingPoint struct {
	Stop struct {
		TimingPointName string `json:"TimingPointName"`
		TimingPointTown string `json:"TimingPointTown"`
		TimingPointCode string `json:"TimingPointCode"`
	} `json:"Stop"`
	Passes map[string]pass `json:"Passes"`
}

// pass is one vehicle passing a timing point. Times are local Dutch time
// without an offset. TripStopStatus is PLANNED (timetable only), DRIVING or
// ARRIVED (live), PASSED (already gone), CANCEL, or UNKNOWN.
type pass struct {
	LinePublicNumber      string `json:"LinePublicNumber"`
	LinePlanningNumber    string `json:"LinePlanningNumber"`
	LineDirection         int    `json:"LineDirection"`
	DestinationName50     string `json:"DestinationName50"`
	TargetDepartureTime   string `json:"TargetDepartureTime"`
	ExpectedDepartureTime string `json:"ExpectedDepartureTime"`
	TripStopStatus        string `json:"TripStopStatus"`
	TransportType         string `json:"TransportType"`
	DataOwnerCode         string `json:"DataOwnerCode"`
	JourneyNumber         int64  `json:"JourneyNumber"`
	UserStopCode          string `json:"UserStopCode"`
}
