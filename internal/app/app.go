// Package app wires route resolution, stop matching, the live departures
// call and output formatting together, independent of the city.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/transit"
)

type Options struct {
	City      string // registry id, for output only
	Route     string // public short name, e.g. "155"; "" shows every line at the stop
	Query     string
	Count     int
	ShowClock bool
	Refresh   bool
	List      bool
	JSON      bool
	Window    time.Duration
}

type App struct {
	Provider transit.Provider
	Cache    *cache.Cache
	Out      io.Writer
	Err      io.Writer // hints and warnings; nil discards them
	Color    bool
	// MaxProbes caps the departures calls made to disambiguate a stop by route.
	MaxProbes int
	Pick      func(names []string) int
}

// UsageError is printed bare by main, without the "eta:" prefix.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func (a *App) Run(ctx context.Context, o Options) error {
	if o.Count < 1 {
		o.Count = 1
	}
	if o.Window <= 0 {
		o.Window = 90 * time.Minute
	}
	if a.MaxProbes <= 0 {
		a.MaxProbes = 4
	}
	rl, hasRoutes := a.Provider.(transit.RouteLister)
	ss, hasSearch := a.Provider.(transit.StopSearcher)
	if !hasRoutes && !hasSearch {
		return fmt.Errorf("provider %s implements neither RouteLister nor StopSearcher", a.Provider.Info().ID)
	}
	if o.Route == "" {
		return a.runBoard(ctx, ss, hasSearch, o)
	}

	var (
		routes       []transit.Route
		cand         match.Candidate
		deps         *transit.Departures
		dirHeadsigns map[string]string
		err          error
	)
	if hasRoutes {
		routes, err = a.resolveRoutes(ctx, rl, o.Route, o.Refresh)
		if err != nil && (!hasSearch || !errors.Is(err, transit.ErrUnknownRoute)) {
			return err
		}
	}
	switch {
	case len(routes) > 0:
		if o.List {
			lists, err := a.stopLists(ctx, rl, routes, o.Refresh)
			if err != nil {
				return err
			}
			if o.JSON {
				return a.writeJSON(lists)
			}
			a.renderLists(lists)
			return nil
		}
		var stops []match.Stop
		stops, dirHeadsigns, err = a.routeStops(ctx, rl, routes, o.Refresh)
		if err != nil {
			return err
		}
		cand, err = a.pickStop(o.Route, o.Query, stops)
		if err != nil {
			return err
		}
	default:
		if o.List {
			return &UsageError{fmt.Sprintf("-l is not supported for %s: its API has no route → stops call", a.Provider.Info().ID)}
		}
		routes = []transit.Route{{ShortName: o.Route}}
		cand, deps, err = a.searchStop(ctx, ss, o, routes)
		if err != nil {
			return err
		}
	}
	if deps == nil {
		deps, err = a.Provider.Departures(ctx, cand.IDs, routes, o.Window)
		if err != nil {
			return err
		}
	}
	groups := groupDepartures(deps, routes, dirHeadsigns, o.Count)
	label := routes[0].ShortName
	if label == "" {
		label = o.Route
	}
	if o.JSON {
		return a.writeJSON(departuresJSON(o.City, cand, label, deps.Now, groups))
	}
	a.render(cand.Name, label, groups, o)
	return nil
}

func (a *App) runBoard(ctx context.Context, ss transit.StopSearcher, hasSearch bool, o Options) error {
	id := a.Provider.Info().ID
	if o.List {
		return &UsageError{fmt.Sprintf("-l needs a route: eta %s <route> -l", id)}
	}
	if !hasSearch {
		return &UsageError{fmt.Sprintf("%s cannot search stops by name; give a route: eta %s <route> <stop>", id, id)}
	}
	cand, deps, err := a.searchStop(ctx, ss, o, nil)
	if err != nil {
		return err
	}
	if deps == nil {
		deps, err = a.Provider.Departures(ctx, cand.IDs, nil, o.Window)
		if err != nil {
			return err
		}
	}
	groups := groupDepartures(deps, nil, nil, o.Count)
	if o.JSON {
		return a.writeJSON(departuresJSON(o.City, cand, "", deps.Now, groups))
	}
	a.render(cand.Name, "", groups, o)
	return nil
}

func (a *App) writeJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) resolveRoutes(ctx context.Context, rl transit.RouteLister, short string, refresh bool) ([]transit.Route, error) {
	key := "route-" + match.Normalize(short)
	var routes []transit.Route
	if !refresh && a.Cache.Load(key, &routes) && len(routes) > 0 {
		return routes, nil
	}
	found, err := rl.FindRoutes(ctx, short)
	var ure *transit.UnknownRouteError
	if errors.As(err, &ure) {
		return nil, &UsageError{ure.Error()}
	}
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, &UsageError{(&transit.UnknownRouteError{Route: short}).Error()}
	}
	_ = a.Cache.Store(key, found)
	return found, nil
}

func (a *App) patterns(ctx context.Context, rl transit.RouteLister, r transit.Route, refresh bool) ([]transit.Pattern, error) {
	key := "stops-" + r.ID
	var ps []transit.Pattern
	if refresh || !a.Cache.Load(key, &ps) || len(ps) == 0 {
		got, err := rl.RouteStops(ctx, r)
		if errors.Is(err, transit.ErrNotFound) {
			return nil, &UsageError{fmt.Sprintf("route %s (%s) has no details; try -r to refresh the cache", r.ShortName, r.ID)}
		}
		if err != nil {
			return nil, err
		}
		ps = got
		_ = a.Cache.Store(key, ps)
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].DirectionID < ps[j].DirectionID })
	return ps, nil
}

func (a *App) routeStops(ctx context.Context, rl transit.RouteLister, routes []transit.Route, refresh bool) ([]match.Stop, map[string]string, error) {
	var stops []match.Stop
	seen := map[string]bool{}
	headsigns := map[string]string{}
	for _, r := range routes {
		ps, err := a.patterns(ctx, rl, r, refresh)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range ps {
			if _, ok := headsigns[p.DirectionID]; !ok && p.Headsign != "" {
				headsigns[p.DirectionID] = p.Headsign
			}
			for _, s := range p.Stops {
				if seen[s.ID] {
					continue
				}
				seen[s.ID] = true
				name := s.Name
				if name == "" {
					name = s.ID
				}
				stops = append(stops, match.Stop{ID: s.ID, Name: name})
			}
		}
	}
	if len(stops) == 0 {
		return nil, nil, &UsageError{fmt.Sprintf("route %s has no stops in the timetable", routes[0].ShortName)}
	}
	return stops, headsigns, nil
}

func (a *App) searchStop(ctx context.Context, ss transit.StopSearcher, o Options, routes []transit.Route) (match.Candidate, *transit.Departures, error) {
	key := "search-" + match.Normalize(o.Query)
	var found []transit.Stop
	if o.Refresh || !a.Cache.Load(key, &found) || len(found) == 0 {
		var err error
		found, err = ss.SearchStops(ctx, o.Query)
		if err != nil {
			return match.Candidate{}, nil, err
		}
		if len(found) > 0 {
			_ = a.Cache.Store(key, found)
		}
	}
	if len(found) == 0 {
		return match.Candidate{}, nil, &UsageError{fmt.Sprintf("no stop matching %q in %s", o.Query, a.Provider.Info().ID)}
	}
	if len(routes) == 0 && !o.JSON && a.Pick != nil && len(found) > 1 {
		// No route to disambiguate by: let the rider choose.
		cands := match.Group(stopsOf(found))
		if len(cands) > 1 {
			if i := a.Pick(names(cands)); i >= 0 && i < len(cands) {
				return cands[i], nil, nil
			}
		}
	}
	cands := match.Group(stopsOf(found))
	q := match.Normalize(o.Query)
	var exact []match.Candidate
	for _, c := range cands {
		if match.Normalize(c.Name) == q {
			exact = append(exact, c)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil, nil
	case len(exact) > 1:
		cands = exact
	}
	if len(routes) == 0 {
		// Nothing to probe by: the search API ranked the hits, take its first.
		return cands[0], nil, nil
	}
	if len(cands) > a.MaxProbes {
		cands = cands[:a.MaxProbes]
	}
	survivors, boards := a.probe(ctx, cands, routes, o.Window)
	switch len(survivors) {
	case 1:
		return survivors[0], boards[survivors[0].Name], nil
	case 0:
		return match.Candidate{}, nil, &UsageError{fmt.Sprintf("no stop matching %q is served by route %s", o.Query, o.Route)}
	}
	// Several stops carry the route. Ask when we can; otherwise the search
	// API ranked them by relevance, so its first hit is the rider's most
	// likely intent, and the rest are worth a hint.
	if a.Pick != nil && !o.JSON {
		if i := a.Pick(names(survivors)); i >= 0 && i < len(survivors) {
			return survivors[i], boards[survivors[i].Name], nil
		}
	}
	pick := survivors[0]
	if a.Err != nil {
		var sb strings.Builder
		fmt.Fprintf(&sb, "eta: %q also matches", o.Query)
		for i, c := range survivors[1:] {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(" " + c.Name)
		}
		fmt.Fprintln(a.Err, sb.String())
	}
	return pick, boards[pick.Name], nil
}

// A failed probe keeps its candidate so the user still gets a useful error.
func (a *App) probe(ctx context.Context, cands []match.Candidate, routes []transit.Route, window time.Duration) ([]match.Candidate, map[string]*transit.Departures) {
	keep := make([]bool, len(cands))
	boards := make([]*transit.Departures, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c match.Candidate) {
			defer wg.Done()
			deps, err := a.Provider.Departures(ctx, c.IDs, routes, window)
			if err != nil {
				keep[i] = true
				return
			}
			for _, d := range deps.Departures {
				if onRoute(d, routes) {
					keep[i] = true
					boards[i] = deps
					return
				}
			}
		}(i, c)
	}
	wg.Wait()
	var out []match.Candidate
	byName := map[string]*transit.Departures{}
	for i, c := range cands {
		if keep[i] {
			out = append(out, c)
			if boards[i] != nil {
				byName[c.Name] = boards[i]
			}
		}
	}
	return out, byName
}

type RouteStops struct {
	City       string           `json:"city"`
	Route      string           `json:"route"`
	RouteID    string           `json:"routeId"`
	Directions []DirectionStops `json:"directions"`
}

// The longest pattern of a direction is the backbone; stops only served by
// shorter or branch patterns are appended after it.
type DirectionStops struct {
	Direction string     `json:"direction"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Stops     []StopInfo `json:"stops"`
}

type StopInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (a *App) stopLists(ctx context.Context, rl transit.RouteLister, routes []transit.Route, refresh bool) ([]RouteStops, error) {
	var out []RouteStops
	for _, r := range routes {
		ps, err := a.patterns(ctx, rl, r, refresh)
		if err != nil {
			return nil, err
		}
		type dir struct {
			headsign string
			stops    []transit.Stop
		}
		byDir := map[string]*dir{}
		var order []string
		for _, p := range ps {
			d, ok := byDir[p.DirectionID]
			if !ok {
				d = &dir{headsign: p.Headsign}
				byDir[p.DirectionID] = d
				order = append(order, p.DirectionID)
			}
			if len(p.Stops) > len(d.stops) {
				d.stops = append([]transit.Stop(nil), p.Stops...)
				if p.Headsign != "" {
					d.headsign = p.Headsign
				}
			}
		}
		for _, p := range ps {
			d := byDir[p.DirectionID]
			for _, s := range p.Stops {
				if !containsStop(d.stops, s.ID) {
					d.stops = append(d.stops, s)
				}
			}
		}
		rs := RouteStops{City: a.Provider.Info().ID, Route: r.ShortName, RouteID: r.ID}
		if rs.Route == "" {
			rs.Route = r.ID
		}
		for _, k := range order {
			d := byDir[k]
			if len(d.stops) == 0 {
				continue
			}
			ds := DirectionStops{Direction: k, From: nameOf(d.stops[0]), To: d.headsign}
			if ds.To == "" {
				ds.To = nameOf(d.stops[len(d.stops)-1])
			}
			for _, s := range d.stops {
				ds.Stops = append(ds.Stops, StopInfo{ID: s.ID, Name: nameOf(s)})
			}
			rs.Directions = append(rs.Directions, ds)
		}
		out = append(out, rs)
	}
	return out, nil
}

func nameOf(s transit.Stop) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func containsStop(xs []transit.Stop, id string) bool {
	for _, s := range xs {
		if s.ID == id {
			return true
		}
	}
	return false
}

func stopsOf(in []transit.Stop) []match.Stop {
	out := make([]match.Stop, 0, len(in))
	for _, s := range in {
		out = append(out, match.Stop{ID: s.ID, Name: s.Name})
	}
	return out
}

func names(cands []match.Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Name
	}
	return out
}

func (a *App) pickStop(route, query string, stops []match.Stop) (match.Candidate, error) {
	cands := match.Find(query, stops)
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		all := match.Group(stops)
		var sb strings.Builder
		fmt.Fprintf(&sb, "no stop matching %q on route %s.", query, route)
		if len(all) <= 40 {
			sb.WriteString(" Stops on this route:\n")
			for _, c := range all {
				sb.WriteString("  " + c.Name + "\n")
			}
		} else {
			sb.WriteString(" Closest names:\n")
			for _, c := range match.Closest(query, stops, 8) {
				sb.WriteString("  " + c.Name + "\n")
			}
		}
		return match.Candidate{}, &UsageError{strings.TrimRight(sb.String(), "\n")}
	default:
		if a.Pick != nil {
			if i := a.Pick(names(cands)); i >= 0 && i < len(cands) {
				return cands[i], nil
			}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%q matches several stops on route %s; be more specific:\n", query, route)
		for _, c := range cands {
			sb.WriteString("  " + c.Name + "\n")
		}
		return match.Candidate{}, &UsageError{strings.TrimRight(sb.String(), "\n")}
	}
}
