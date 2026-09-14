// Package bkk is the BKK FUTÁR provider, over the OneBusAway-style
// FUTÁR OpenData API (https://opendata.bkk.hu/docs/futar-openapi.yaml).
package bkk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/transit"
	"github.com/bancsdan/eta/internal/xutil"
)

type route struct {
	ID          string `json:"id"`
	ShortName   string `json:"shortName"`
	LongName    string `json:"longName"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

type stop struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type variant struct {
	Direction string   `json:"direction"`
	Headsign  string   `json:"headsign"`
	StopIDs   []string `json:"stopIds"`
}

type trip struct {
	ID          string `json:"id"`
	RouteID     string `json:"routeId"`
	DirectionID string `json:"directionId"`
	Headsign    string `json:"tripHeadsign"`
}

// Times are epoch seconds (occasionally millis); zero means absent.
type stopTime struct {
	StopID                 string `json:"stopId"`
	TripID                 string `json:"tripId"`
	StopHeadsign           string `json:"stopHeadsign"`
	ArrivalTime            int64  `json:"arrivalTime"`
	DepartureTime          int64  `json:"departureTime"`
	PredictedArrivalTime   int64  `json:"predictedArrivalTime"`
	PredictedDepartureTime int64  `json:"predictedDepartureTime"`
	PredictionScheduled    bool   `json:"predictionScheduled"`
	PickupAllowed          *bool  `json:"pickupAllowed"`
	Uncertain              bool   `json:"uncertain"`
}

// routeIds and stopIds are both filled, and includeReferences decides which
// reference maps come along.
type searchData struct {
	Entry struct {
		RouteIDs []string `json:"routeIds"`
		StopIDs  []string `json:"stopIds"`
	} `json:"entry"`
	References struct {
		Routes map[string]route `json:"routes"`
		Stops  map[string]stop  `json:"stops"`
	} `json:"references"`
}

type routeDetailsData struct {
	Entry struct {
		route
		Variants []variant `json:"variants"`
	} `json:"entry"`
	References struct {
		Stops map[string]stop `json:"stops"`
	} `json:"references"`
}

type arrivalRefs struct {
	Trips  map[string]trip  `json:"trips"`
	Stops  map[string]stop  `json:"stops"`
	Routes map[string]route `json:"routes"`
}

type arrivalsData struct {
	Entry struct {
		StopTimes []stopTime `json:"stopTimes"`
	} `json:"entry"`
	References arrivalRefs `json:"references"`
}

// An error can be reported inside an HTTP 200: code 404 with a status text
// is how an unknown route id comes back.
type envelope struct {
	Code        int             `json:"code"`
	CurrentTime int64           `json:"currentTime"`
	Status      string          `json:"status"`
	Text        string          `json:"text"`
	Data        json.RawMessage `json:"data"`
}

type client struct {
	http *httpx.Client
	key  string
}

func (c *client) get(ctx context.Context, u string, out any) (time.Time, error) {
	if c.key == "" {
		return time.Time{}, fmt.Errorf("budapest: %w: set %s (free key: %s)", transit.ErrNoKey, keyEnv, signupURL)
	}
	var env envelope
	if err := c.http.GetJSON(ctx, u, &env); err != nil {
		return time.Time{}, err
	}
	if err := envErr(env); err != nil {
		return time.Time{}, err
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return time.Time{}, fmt.Errorf("budapest: bad JSON: %v", err)
		}
	}
	return xutil.EpochTime(env.CurrentTime), nil
}

// FUTÁR answers HTTP 200 with code 404 for an unknown route id.
func envErr(env envelope) error {
	switch env.Code {
	case 0, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("budapest: %w: %s", transit.ErrNotFound, env.Text)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("budapest: %w: %s", transit.ErrUnauthorized, env.Text)
	case http.StatusTooManyRequests:
		return fmt.Errorf("budapest: %w: %s", transit.ErrRateLimited, env.Text)
	default:
		return fmt.Errorf("budapest: %d %s", env.Code, env.Text)
	}
}

func first(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
