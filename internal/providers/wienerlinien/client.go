package wienerlinien

// monitorResp is GET /monitor?diva=…: one monitor per platform (RBL) of
// the requested stations. Times look like "2026-09-18T10:00:00.000+0200".
type monitorResp struct {
	Data struct {
		Monitors []monitor `json:"monitors"`
	} `json:"data"`
	Message struct {
		Value       string `json:"value"`
		MessageCode int    `json:"messageCode"`
		ServerTime  string `json:"serverTime"`
	} `json:"message"`
}

type monitor struct {
	LocationStop struct {
		Properties struct {
			Name  string `json:"name"` // the DIVA station number
			Title string `json:"title"`
		} `json:"properties"`
	} `json:"locationStop"`
	Lines []monitorLine `json:"lines"`
}

type monitorLine struct {
	Name        string `json:"name"`
	Towards     string `json:"towards"`
	Direction   string `json:"direction"` // "H" or "R"
	Platform    string `json:"platform"`
	RichtungsID string `json:"richtungsId"`
	LineID      int    `json:"lineId"`
	Departures  struct {
		Departure []struct {
			DepartureTime struct {
				TimePlanned string `json:"timePlanned"`
				TimeReal    string `json:"timeReal"` // absent without real-time data
				Countdown   int    `json:"countdown"`
			} `json:"departureTime"`
			Vehicle *struct {
				Towards string `json:"towards"`
			} `json:"vehicle"`
		} `json:"departure"`
	} `json:"departures"`
}
