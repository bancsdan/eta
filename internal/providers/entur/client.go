package entur

type linesData struct {
	Lines []lineDTO `json:"lines"`
}

// Line is null when the id is unknown.
type lineData struct {
	Line *lineDTO `json:"line"`
}

type lineDTO struct {
	ID              string              `json:"id"`
	PublicCode      string              `json:"publicCode"`
	Name            string              `json:"name"`
	TransportMode   string              `json:"transportMode"`
	JourneyPatterns []journeyPatternDTO `json:"journeyPatterns"`
}

// A line has many journey patterns per direction (short turns, branches);
// the quays are in travel order.
type journeyPatternDTO struct {
	ID            string    `json:"id"`
	DirectionType string    `json:"directionType"`
	Name          string    `json:"name"`
	Quays         []quayDTO `json:"quays"`
}

// StopPlace is the station the platform belongs to and may be null for
// isolated quays.
type quayDTO struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	PublicCode string      `json:"publicCode"`
	StopPlace  *stopRefDTO `json:"stopPlace"`
}

type stopRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Null when the requested id does not exist.
type placeDTO struct {
	Name           string             `json:"name"`
	EstimatedCalls []estimatedCallDTO `json:"estimatedCalls"`
}

// Every nested object is a pointer: Entur nulls them for unusual journeys.
type estimatedCallDTO struct {
	AimedDepartureTime    string             `json:"aimedDepartureTime"`
	ExpectedDepartureTime string             `json:"expectedDepartureTime"`
	Realtime              bool               `json:"realtime"`
	Cancellation          bool               `json:"cancellation"`
	PredictionInaccurate  bool               `json:"predictionInaccurate"`
	DestinationDisplay    *destinationDTO    `json:"destinationDisplay"`
	ServiceJourney        *serviceJourneyDTO `json:"serviceJourney"`
	Quay                  *callQuayDTO       `json:"quay"`
}

type destinationDTO struct {
	FrontText string `json:"frontText"`
}

type serviceJourneyDTO struct {
	ID            string       `json:"id"`
	Line          *callLineDTO `json:"line"`
	DirectionType string       `json:"directionType"`
}

type callLineDTO struct {
	ID            string `json:"id"`
	PublicCode    string `json:"publicCode"`
	TransportMode string `json:"transportMode"`
}

type callQuayDTO struct {
	ID         string `json:"id"`
	PublicCode string `json:"publicCode"`
}

type geoResponse struct {
	Features []geoFeature `json:"features"`
}

type geoFeature struct {
	Geometry   geoGeometry   `json:"geometry"`
	Properties geoProperties `json:"properties"`
}

type geoGeometry struct {
	Coordinates []float64 `json:"coordinates"` // [lon, lat]
}

type geoProperties struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Locality string   `json:"locality"`
	County   string   `json:"county"`
	Category []string `json:"category"`
}
