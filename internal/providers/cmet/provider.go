// Package cmet is the Carris Metropolitana provider: the bus network of
// the Lisbon metropolitan area (23 municipalities from Sintra and Cascais
// to Setúbal) from its keyless public API, with real-time estimates. Stops
// carry their municipality and locality, which is what a town scopes to.
// Inside Lisbon proper only Carris Metropolitana's own stops are covered:
// the city's Carris buses and the Metro have no open API.
package cmet

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/httpx"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const (
	DefaultBaseURL = "https://api.carrismetropolitana.pt/v2"
	tablesTTL      = 24 * time.Hour
	defaultWindow  = time.Hour
	notes          = "Carris Metropolitana buses; in Lisbon itself only their stops (Carris city buses and the Metro are not covered); first run downloads the stop list (7 MB)"
)

type town struct {
	id, name, muni string
	aliases        []string
	probe          transit.Probe
}

// Probes were verified live on 2026-09-21 against the arrivals endpoint.
var towns = []town{
	{"lisbon", "Lisbon", "1106", []string{"lisboa"}, transit.Probe{Route: "2713", Query: "campo grande av brasil"}},
	{"sintra", "Sintra", "1111", nil, transit.Probe{Route: "1253", Query: "sintra estação"}},
	{"cascais", "Cascais", "1105", nil, transit.Probe{Route: "1627", Query: "cascaishopping terminal"}},
	{"almada", "Almada", "1503", nil, transit.Probe{Route: "3007", Query: "cacilhas terminal"}},
	{"setubal", "Setúbal", "1512", nil, transit.Probe{Route: "4403", Query: "ciprestes its"}},
	{"amadora", "Amadora", "1115", nil, transit.Probe{Route: "1003", Query: "amadora estação"}},
	{"oeiras", "Oeiras", "1110", nil, transit.Probe{Route: "1120", Query: "oeiras estação"}},
	{"loures", "Loures", "1107", nil, transit.Probe{Route: "2725", Query: "r república 14"}},
	{"odivelas", "Odivelas", "1116", nil, transit.Probe{Route: "2812", Query: "sr roubado"}},
	{"seixal", "Seixal", "1510", nil, transit.Probe{Route: "3118", Query: "cruz pau centro"}},
	{"barreiro", "Barreiro", "1504", nil, transit.Probe{Route: "3601", Query: "coina centro"}},
	{"montijo", "Montijo", "1507", nil, transit.Probe{Route: "4600", Query: "josé joaquim marques"}},
	{"moita", "Moita", "1506", nil, transit.Probe{Route: "4701", Query: "classe operária"}},
	{"alcochete", "Alcochete", "1502", nil, transit.Probe{Route: "4702", Query: "av restauração"}},
	{"palmela", "Palmela", "1508", nil, transit.Probe{Route: "4562", Query: "visconde tojal"}},
	{"sesimbra", "Sesimbra", "1511", nil, transit.Probe{Route: "3536", Query: "sesimbra terminal"}},
	{"mafra", "Mafra", "1109", nil, transit.Probe{Route: "2740", Query: "terreiro d joão v"}},
	{"vila-franca-de-xira", "Vila Franca de Xira", "1114", []string{"vfx"}, transit.Probe{Route: "2796", Query: "egas moniz vialonga"}},
	{"alenquer", "Alenquer", "1101", nil, transit.Probe{Route: "2926", Query: "urb santa maria 17 cadafais"}},
	{"arruda-dos-vinhos", "Arruda dos Vinhos", "1102", nil, transit.Probe{Route: "2921", Query: "adriano brito"}},
	{"torres-vedras", "Torres Vedras", "1113", nil, transit.Probe{Route: "2905", Query: "principal 17 escola assenta"}},
	{"vendas-novas", "Vendas Novas", "0712", nil, transit.Probe{Route: "4901", Query: "landeira"}},
}

func townInfo(id, name string, probe transit.Probe) transit.Info {
	return transit.Info{
		ID:       id,
		Name:     name + " (Carris Metropolitana)",
		Country:  "portugal",
		Provider: "cmet",
		TZ:       "Europe/Lisbon",
		Realtime: true,
		Notes:    notes,
		Probe:    probe,
	}
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "portugal", Name: "Portugal", Aliases: []string{"pt"}, Providers: []string{"cmet"},
		Coverage: "the Lisbon metropolitan area: its 23 municipalities and their localities (Sintra, Cascais, Almada, Setúbal, Estoril, Queluz…)", AnyTown: NewTown})
	for _, t := range towns {
		t := t
		registry.Register(registry.Entry{Info: townInfo(t.id, t.name, t.probe), Aliases: t.aliases,
			New: func(d registry.Deps) transit.Provider {
				return build(d, townInfo(t.id, t.name, t.probe), t.name, t.muni)
			}})
	}
}

type Provider struct {
	BaseURL string

	info   transit.Info
	town   string
	muni   string // municipality id when the town is a registered municipality
	http   *httpx.Client
	tables *cache.Cache // provider-wide: stops, lines and places are shared by every town
	now    func() time.Time
	loc    *time.Location

	mu    sync.Mutex
	scope *scope
	stops []stop
	lines []line
}

// NewTown builds a provider for any municipality or locality of the area.
func NewTown(d registry.Deps, town string) transit.Provider {
	return build(d, townInfo(strings.ReplaceAll(match.Normalize(town), " ", "-"), town, transit.Probe{}), town, "")
}

func build(d registry.Deps, info transit.Info, town, muni string) *Provider {
	return &Provider{BaseURL: DefaultBaseURL, info: info, town: town, muni: muni, http: httpx.New("cmet", d.HTTP),
		tables: cache.Default("cmet", tablesTTL), now: d.Clock(), loc: info.Location()}
}

func (p *Provider) Info() transit.Info { return p.info }

// Cold reports whether the stop list still has to be downloaded.
func (p *Provider) Cold() bool {
	var ss []stop
	return !p.tables.Load("stops", &ss) || len(ss) == 0
}

type stop struct {
	ID    string   `json:"id"`
	Name  string   `json:"long_name"`
	Lat   float64  `json:"lat"`
	Lon   float64  `json:"lon"`
	Muni  string   `json:"municipality_id"`
	Loc   string   `json:"locality_id"`
	Lines []string `json:"line_ids"`
}

type line struct {
	ID        string   `json:"id"`
	ShortName string   `json:"short_name"`
	LongName  string   `json:"long_name"`
	Patterns  []string `json:"pattern_ids"`
}

type place struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Muni string `json:"municipality_id"`
}

type pattern struct {
	ID        string `json:"id"`
	Direction int    `json:"direction_id"`
	Headsign  string `json:"headsign"`
	Path      []struct {
		StopID string `json:"stop_id"`
		Seq    int    `json:"stop_sequence"`
	} `json:"path"`
	ValidOn []string `json:"valid_on"`
}

type arrival struct {
	Estimated *int64 `json:"estimated_arrival_unix"`
	Observed  *int64 `json:"observed_arrival_unix"`
	Scheduled *int64 `json:"scheduled_arrival_unix"`
	Headsign  string `json:"headsign"`
	LineID    string `json:"line_id"`
	PatternID string `json:"pattern_id"`
	TripID    string `json:"trip_id"`
	Seq       int    `json:"stop_sequence"`
}

// table fetches one shared list, cached for a day.
func table[T any](ctx context.Context, p *Provider, key, path string, unwrap bool) ([]T, error) {
	var out []T
	if p.tables.Load(key, &out) && len(out) > 0 {
		return out, nil
	}
	u := httpx.URL(p.BaseURL, path, nil)
	if unwrap {
		var env struct {
			Data []T `json:"data"`
		}
		if err := p.http.GetJSON(ctx, u, &env); err != nil {
			return nil, err
		}
		out = env.Data
	} else if err := p.http.GetJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cmet: %s came back empty", path)
	}
	_ = p.tables.Store(key, out)
	return out, nil
}

func (p *Provider) allStops(ctx context.Context) ([]stop, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stops != nil {
		return p.stops, nil
	}
	ss, err := table[stop](ctx, p, "stops", "stops", false)
	if err != nil {
		return nil, err
	}
	p.stops = ss
	return ss, nil
}

func (p *Provider) allLines(ctx context.Context) ([]line, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lines != nil {
		return p.lines, nil
	}
	ls, err := table[line](ctx, p, "lines", "lines", false)
	if err != nil {
		return nil, err
	}
	p.lines = ls
	return ls, nil
}

// scope is the set of municipality or locality ids a town stands for.
type scope struct {
	munis map[string]bool
	locs  map[string]bool
}

func (s *scope) has(st stop) bool { return s.munis[st.Muni] || s.locs[st.Loc] }

func (p *Provider) area(ctx context.Context) (*scope, error) {
	p.mu.Lock()
	if p.scope != nil {
		defer p.mu.Unlock()
		return p.scope, nil
	}
	p.mu.Unlock()
	sc := &scope{munis: map[string]bool{}, locs: map[string]bool{}}
	if p.muni != "" {
		sc.munis[p.muni] = true
	} else {
		want := match.Normalize(p.town)
		if want == "lisbon" {
			want = "lisboa"
		}
		munis, err := table[place](ctx, p, "municipalities", "locations/municipalities", true)
		if err != nil {
			return nil, err
		}
		for _, m := range munis {
			if match.Normalize(m.Name) == want {
				sc.munis[m.ID] = true
			}
		}
		if len(sc.munis) == 0 {
			locs, err := table[place](ctx, p, "localities", "locations/localities", true)
			if err != nil {
				return nil, err
			}
			for _, l := range locs {
				if match.Normalize(l.Name) == want {
					sc.locs[l.ID] = true
				}
			}
		}
		if len(sc.munis) == 0 && len(sc.locs) == 0 {
			return nil, fmt.Errorf("cmet: no municipality or locality named %q in the Lisbon metropolitan area: %w", p.town, transit.ErrNotFound)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scope = sc
	return sc, nil
}

func (p *Provider) scopedStops(ctx context.Context) ([]stop, error) {
	ss, err := p.allStops(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := p.area(ctx)
	if err != nil {
		return nil, err
	}
	var out []stop
	for _, s := range ss {
		if sc.has(s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cmet: no stops in %s: %w", p.town, transit.ErrNotFound)
	}
	return out, nil
}

func toRoute(l line) transit.Route {
	return transit.Route{ID: l.ID, ShortName: l.ShortName, LongName: l.LongName, Mode: "bus"}
}

// FindRoutes matches the line number among the lines calling in the town,
// or a unique word of a line's long name ("cascais" → 1627 Rio de Mouro -
// Cascais, when no other in-town line mentions it).
func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	stops, err := p.scopedStops(ctx)
	if err != nil {
		return nil, err
	}
	lines, err := p.allLines(ctx)
	if err != nil {
		return nil, err
	}
	served := map[string]bool{}
	for _, s := range stops {
		for _, id := range s.Lines {
			served[id] = true
		}
	}
	want := match.Normalize(short)
	var exact, partial []transit.Route
	var names []string
	for _, l := range lines {
		if !served[l.ID] {
			continue
		}
		names = append(names, l.ShortName)
		switch {
		case match.Normalize(l.ShortName) == want:
			exact = append(exact, toRoute(l))
		case len(want) >= 3 && strings.ContainsAny(want, "abcdefghijklmnopqrstuvwxyz") && strings.Contains(match.Normalize(l.LongName), want):
			partial = append(partial, toRoute(l))
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(partial) == 1 {
		return partial, nil
	}
	sort.Strings(names)
	return nil, transit.UnknownRoute(short, names)
}

// RouteStops fetches the line's patterns and keeps the ones valid today.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	lines, err := p.allLines(ctx)
	if err != nil {
		return nil, err
	}
	stops, err := p.allStops(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]stop, len(stops))
	for _, s := range stops {
		byID[s.ID] = s
	}
	var l *line
	for i := range lines {
		if lines[i].ID == r.ID {
			l = &lines[i]
		}
	}
	if l == nil {
		return nil, fmt.Errorf("cmet: line %s: %w", r.ShortName, transit.ErrNotFound)
	}
	today := p.now().In(p.loc).Format("20060102")
	var current, any []transit.Pattern
	for _, pid := range l.Patterns {
		var versions []pattern
		if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "patterns/"+pid, nil), &versions); err != nil {
			return nil, err
		}
		for _, v := range versions {
			tp := transit.Pattern{DirectionID: fmt.Sprint(v.Direction), Headsign: v.Headsign}
			for _, c := range v.Path {
				if s, ok := byID[c.StopID]; ok {
					tp.Stops = append(tp.Stops, transit.Stop{ID: s.ID, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
				}
			}
			if len(tp.Stops) == 0 {
				continue
			}
			any = append(any, tp)
			for _, d := range v.ValidOn {
				if d == today {
					current = append(current, tp)
					break
				}
			}
		}
	}
	if len(current) == 0 {
		current = any
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("cmet: line %s has no stops: %w", r.ShortName, transit.ErrNotFound)
	}
	sort.SliceStable(current, func(i, j int) bool { return current[i].DirectionID < current[j].DirectionID })
	return current, nil
}

// SearchStops matches names among the town's stops.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	stops, err := p.scopedStops(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]match.Stop, 0, len(stops))
	byID := make(map[string]stop, len(stops))
	for _, s := range stops {
		all = append(all, match.Stop{ID: s.ID, Name: s.Name})
		byID[s.ID] = s
	}
	cands := match.Find(query, all)
	if len(cands) == 0 {
		cands = match.Closest(query, all, 8)
	}
	var out []transit.Stop
	for _, c := range cands {
		for _, id := range c.IDs {
			s := byID[id]
			out = append(out, transit.Stop{ID: s.ID, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
		}
	}
	return out, nil
}

// Departures reads each stop's arrivals for the day and keeps the window:
// estimated times are live, the rest is the timetable. Buses that have
// already called are dropped, as are arrivals of a bus terminating here.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, routes []transit.Route, window time.Duration) (*transit.Departures, error) {
	now := p.now().In(p.loc)
	out := &transit.Departures{Now: now}
	if len(stopIDs) == 0 {
		return out, nil
	}
	if window <= 0 {
		window = defaultWindow
	}
	want := map[string]bool{}
	for _, r := range routes {
		want[r.ID] = true
	}
	names := map[string]string{}
	if stops, err := p.allStops(ctx); err == nil {
		for _, s := range stops {
			names[s.ID] = s.Name
		}
	}
	from, to := now.Add(-time.Minute), now.Add(window)
	ds, err := transit.Gather(ctx, stopIDs, func(ctx context.Context, stopID string) ([]transit.Departure, error) {
		var as []arrival
		if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "arrivals/by_stop/"+stopID, nil), &as); err != nil {
			if errors.Is(err, transit.ErrNotFound) {
				return nil, nil // a stop without service today
			}
			return nil, err
		}
		here := match.Normalize(names[stopID])
		var out []transit.Departure
		for _, a := range as {
			if a.Observed != nil || a.Scheduled == nil || (len(want) > 0 && !want[a.LineID]) {
				continue
			}
			d := transit.Departure{
				Scheduled:   time.Unix(*a.Scheduled, 0).In(p.loc),
				Headsign:    a.Headsign,
				Line:        a.LineID,
				RouteID:     a.LineID,
				TripID:      a.TripID,
				StopID:      stopID,
				DirectionID: direction(a.PatternID),
			}
			d.At = d.Scheduled
			if a.Estimated != nil {
				d.At, d.Live = time.Unix(*a.Estimated, 0).In(p.loc), true
			}
			if d.TripID == "" {
				d.TripID = a.PatternID + "|" + d.Scheduled.Format("15:04")
			}
			if d.At.Before(from) || d.At.After(to) || terminating(a, here) {
				continue
			}
			out = append(out, d)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out.Departures = ds
	transit.SortDepartures(out.Departures)
	return out, nil
}

// direction reads the pattern id "[version]1630_2_1": the last number is
// odd for the outbound direction and even for the return.
func direction(patternID string) string {
	i := strings.LastIndex(patternID, "_")
	if i < 0 || i+1 >= len(patternID) {
		return ""
	}
	var n int
	if _, err := fmt.Sscan(patternID[i+1:], &n); err != nil || n <= 0 {
		return ""
	}
	return fmt.Sprint((n + 1) % 2)
}

// terminating guesses an arrival that ends its trip at this stop: past
// the first call, with the stop's own name as headsign, or the name plus
// a platform or alighting-only marker ("Cacilhas (Terminal)" at "Cacilhas
// (Terminal) P5", "Campo Grande" at "Campo Grande (Desembarque Norte)").
// Loop lines start and end at the same stop, hence the sequence check; a
// headsign that is merely the first words of a longer name ("Campo Grande"
// at "Campo Grande - Av. Brasil") is a bus passing through.
func terminating(a arrival, stopName string) bool {
	if a.Seq <= 1 || stopName == "" {
		return false
	}
	h := match.Normalize(a.Headsign)
	if h == "" {
		return false
	}
	if h == stopName {
		return true
	}
	if !strings.HasPrefix(stopName, h+" ") {
		return false
	}
	rest := strings.TrimPrefix(stopName, h+" ")
	return strings.HasPrefix(rest, "desembarque") || platformRE.MatchString(rest)
}

var platformRE = regexp.MustCompile(`^p[0-9]+( |$)`)

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
