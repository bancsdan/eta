package app

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/transit"
)

type Arrival struct {
	At   time.Time
	Live bool
}

type Group struct {
	Line      string // route short name; set when the board covers every line
	Direction string
	Headsign  string
	Arrivals  []Arrival
	Now       time.Time
}

// Native ids win when both sides have one; otherwise normalized short names.
func onRoute(d transit.Departure, routes []transit.Route) bool {
	if len(routes) == 0 {
		return true
	}
	line := match.Normalize(d.Line)
	for _, r := range routes {
		if d.RouteID != "" && r.ID != "" {
			if d.RouteID == r.ID {
				return true
			}
			continue
		}
		if r.ShortName != "" && line == match.Normalize(r.ShortName) {
			return true
		}
	}
	return false
}

func groupDepartures(deps *transit.Departures, routes []transit.Route, dirHeadsigns map[string]string, count int) []Group {
	allLines := len(routes) == 0
	now := deps.Now
	if now.IsZero() {
		now = time.Now()
	}
	byKey := map[string]*Group{}
	var order []string
	seen := map[string]bool{}
	for _, d := range deps.Departures {
		if !onRoute(d, routes) || d.Cancelled {
			continue
		}
		at, live := d.At, d.Live
		if at.IsZero() {
			at, live = d.Scheduled, false
		}
		if at.IsZero() || at.Before(now.Add(-time.Minute)) {
			continue
		}
		dedupe := d.TripID
		if dedupe == "" {
			dedupe = d.Line + "|" + match.Normalize(d.Headsign) + "|" + at.UTC().Format(time.RFC3339)
		}
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		headsign := d.Headsign
		if headsign == "" {
			headsign = dirHeadsigns[d.DirectionID]
		}
		line := ""
		if allLines {
			line = d.Line
		}
		key := match.Normalize(line) + "|" + d.DirectionID + "|" + match.Normalize(headsign)
		g, ok := byKey[key]
		if !ok {
			g = &Group{Line: line, Direction: d.DirectionID, Headsign: headsign, Now: now}
			byKey[key] = g
			order = append(order, key)
		}
		g.Arrivals = append(g.Arrivals, Arrival{At: at, Live: live})
	}
	groups := make([]Group, 0, len(order))
	for _, k := range order {
		g := byKey[k]
		sort.SliceStable(g.Arrivals, func(i, j int) bool { return g.Arrivals[i].At.Before(g.Arrivals[j].At) })
		if len(g.Arrivals) > count {
			g.Arrivals = g.Arrivals[:count]
		}
		groups = append(groups, *g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if c := lineLess(groups[i].Line, groups[j].Line); c != 0 {
			return c < 0
		}
		if groups[i].Direction != groups[j].Direction {
			return groups[i].Direction < groups[j].Direction
		}
		return groups[i].Headsign < groups[j].Headsign
	})
	return groups
}

// Riders expect "M4" < "M10" and "9" < "155", letters before their numbers.
func lineLess(a, b string) int {
	pa, na := splitLine(a)
	pb, nb := splitLine(b)
	if pa != pb {
		return strings.Compare(pa, pb)
	}
	if na != nb {
		if na < nb {
			return -1
		}
		return 1
	}
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

func splitLine(s string) (string, int) {
	s = strings.ToLower(s)
	i := 0
	for i < len(s) && (s[i] < '0' || s[i] > '9') {
		i++
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	n, _ := strconv.Atoi(s[i:j])
	return s[:i], n
}
