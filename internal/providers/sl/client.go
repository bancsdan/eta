package sl

type site struct {
	ID   int     `json:"id"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type departuresResp struct {
	Departures []departure `json:"departures"`
}

// departure is one board row. scheduled/expected are local Stockholm times
// without a UTC offset. state is EXPECTED or ATSTOP when live, CANCELLED, or
// NOTEXPECTED/others when only the timetable is known.
type departure struct {
	Destination   string `json:"destination"`
	DirectionCode int    `json:"direction_code"`
	State         string `json:"state"`
	Scheduled     string `json:"scheduled"`
	Expected      string `json:"expected"`
	Journey       *struct {
		ID int64 `json:"id"`
	} `json:"journey"`
	StopPoint *struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Designation string `json:"designation"`
	} `json:"stop_point"`
	Line *struct {
		ID            int    `json:"id"`
		Designation   string `json:"designation"`
		TransportMode string `json:"transport_mode"`
	} `json:"line"`
}
