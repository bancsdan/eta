package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/match"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

func (a *App) paint(code, s string) string {
	if !a.Color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (a *App) render(stopName, route string, groups []Group, o Options) {
	w := a.Out
	fmt.Fprintln(w, a.paint(ansiCyan, stopName))
	if len(groups) == 0 {
		if route == "" {
			fmt.Fprintf(w, "No upcoming departures in the next %d min.\n", int(o.Window.Minutes()))
		} else {
			fmt.Fprintf(w, "No upcoming %s departures in the next %d min.\n", route, int(o.Window.Minutes()))
		}
		return
	}
	for _, g := range groups {
		line := route
		if g.Line != "" {
			line = g.Line
		}
		fmt.Fprintln(w, a.paint(ansiBold, line+" → "+g.Headsign))
		cells := make([]string, 0, len(g.Arrivals))
		width := 6 // "5m42s " — keeps short cells evenly spaced
		for _, ar := range g.Arrivals {
			c := formatArrival(ar, g.Now, o.ShowClock)
			if len([]rune(c)) > width {
				width = len([]rune(c))
			}
			cells = append(cells, c)
		}
		var sb strings.Builder
		sb.WriteString(" ")
		for i, c := range cells {
			pad := strings.Repeat(" ", width-len([]rune(c)))
			color := ansiGreen
			if !g.Arrivals[i].Live {
				color = ansiYellow
			}
			sb.WriteString(" " + a.paint(color, c) + pad)
		}
		fmt.Fprintln(w, strings.TrimRight(sb.String(), " "))
	}
}

func (a *App) renderLists(lists []RouteStops) {
	for _, rs := range lists {
		fmt.Fprintln(a.Out, a.paint(ansiBold, rs.Route))
		for _, d := range rs.Directions {
			fmt.Fprintln(a.Out, a.paint(ansiCyan+ansiBold, d.From+" → "+d.To))
			width := len(fmt.Sprint(len(d.Stops)))
			for i, st := range d.Stops {
				fmt.Fprintf(a.Out, "  %*d. %s\n", width, i+1, st.Name)
			}
		}
	}
}

type DeparturesJSON struct {
	City        string          `json:"city"`
	Stop        string          `json:"stop"`
	StopIDs     []string        `json:"stopIds"`
	Route       string          `json:"route"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Directions  []DirectionJSON `json:"directions"`
}

// Line is set only when the board covers every line at the stop.
type DirectionJSON struct {
	Line       string          `json:"line,omitempty"`
	Direction  string          `json:"direction"`
	Headsign   string          `json:"headsign"`
	Departures []DepartureJSON `json:"departures"`
}

// InSeconds counts from GeneratedAt and is clamped at zero.
type DepartureJSON struct {
	At        time.Time `json:"at"`
	InSeconds int       `json:"inSeconds"`
	Live      bool      `json:"live"`
}

func departuresJSON(city string, cand match.Candidate, route string, now time.Time, groups []Group) DeparturesJSON {
	if now.IsZero() {
		now = time.Now()
	}
	out := DeparturesJSON{City: city, Stop: cand.Name, StopIDs: cand.IDs, Route: route, GeneratedAt: now, Directions: []DirectionJSON{}}
	for _, g := range groups {
		d := DirectionJSON{Line: g.Line, Direction: g.Direction, Headsign: g.Headsign, Departures: []DepartureJSON{}}
		for _, ar := range g.Arrivals {
			in := int(ar.At.Sub(now).Seconds())
			if in < 0 {
				in = 0
			}
			d.Departures = append(d.Departures, DepartureJSON{At: ar.At, InSeconds: in, Live: ar.Live})
		}
		out.Directions = append(out.Directions, d)
	}
	return out
}

func formatArrival(ar Arrival, now time.Time, clock bool) string {
	d := ar.At.Sub(now).Truncate(time.Second)
	s := "now"
	if d > 0 {
		s = d.String()
	}
	if !ar.Live {
		s = "~" + s
	}
	if clock {
		s += " (" + ar.At.Local().Format("15:04") + ")"
	}
	return s
}
