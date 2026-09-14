package tfl

// Every response carries a "$type" discriminator and a pile of presentation
// fields we ignore; only what the mapping needs is declared here.

type lineSearchResponse struct {
	Input         string            `json:"input"`
	SearchMatches []lineSearchMatch `json:"searchMatches"`
}

// The live API sends a single "mode" string; the published schema documents
// a "modes" array, so both are accepted.
type lineSearchMatch struct {
	LineID   string   `json:"lineId"`
	LineName string   `json:"lineName"`
	Mode     string   `json:"mode"`
	Modes    []string `json:"modes"`
}

func (m lineSearchMatch) mode() string {
	if len(m.Modes) > 0 {
		return m.Modes[0]
	}
	return m.Mode
}

type routeSequence struct {
	LineID             string              `json:"lineId"`
	LineName           string              `json:"lineName"`
	Direction          string              `json:"direction"`
	Mode               string              `json:"mode"`
	IsOutboundOnly     bool                `json:"isOutboundOnly"`
	StopPointSequences []stopPointSequence `json:"stopPointSequences"`
	OrderedLineRoutes  []orderedLineRoute  `json:"orderedLineRoutes"`
}

type stopPointSequence struct {
	BranchID    int           `json:"branchId"`
	ServiceType string        `json:"serviceType"`
	StopPoint   []matchedStop `json:"stopPoint"`
}

type matchedStop struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	ParentID        string  `json:"parentId"`
	StationID       string  `json:"stationId"`
	TopMostParentID string  `json:"topMostParentId"`
	StopType        string  `json:"stopType"`
	Lat             float64 `json:"lat"`
	Lon             float64 `json:"lon"`
}

// Name reads as an end-to-end itinerary ("Epping ↔ West Ruislip"); NaptanIDs
// are in travel order.
type orderedLineRoute struct {
	Name        string   `json:"name"`
	NaptanIDs   []string `json:"naptanIds"`
	ServiceType string   `json:"serviceType"`
}

type stopSearchResponse struct {
	Query   string            `json:"query"`
	Total   int               `json:"total"`
	Matches []stopSearchMatch `json:"matches"`
}

// Ids starting with "HUB" are interchange hubs that have to be expanded
// before they can be asked for arrivals.
type stopSearchMatch struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	TopMostParentID string   `json:"topMostParentId"`
	Modes           []string `json:"modes"`
	Zone            string   `json:"zone"`
	Lat             float64  `json:"lat"`
	Lon             float64  `json:"lon"`
}

// Hubs and stop areas nest their real platforms one or two levels down in
// children.
type stopPoint struct {
	ID         string      `json:"id"`
	NaptanID   string      `json:"naptanId"`
	CommonName string      `json:"commonName"`
	StopType   string      `json:"stopType"`
	StopLetter string      `json:"stopLetter"`
	Modes      []string    `json:"modes"`
	Lat        float64     `json:"lat"`
	Lon        float64     `json:"lon"`
	Children   []stopPoint `json:"children"`
}

func (s stopPoint) naptan() string {
	if s.NaptanID != "" {
		return s.NaptanID
	}
	return s.ID
}

// Every prediction is live; TfL publishes no scheduled time and no
// cancellations here.
type arrival struct {
	ID                  string `json:"id"`
	VehicleID           string `json:"vehicleId"`
	NaptanID            string `json:"naptanId"`
	StationName         string `json:"stationName"`
	LineID              string `json:"lineId"`
	LineName            string `json:"lineName"`
	PlatformName        string `json:"platformName"`
	Direction           string `json:"direction"`
	DestinationNaptanID string `json:"destinationNaptanId"`
	DestinationName     string `json:"destinationName"`
	Towards             string `json:"towards"`
	ExpectedArrival     string `json:"expectedArrival"`
	TimeToStation       int    `json:"timeToStation"`
	ModeName            string `json:"modeName"`
	Timestamp           string `json:"timestamp"`
}
