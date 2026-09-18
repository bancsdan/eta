// Package wienerlinien is the Vienna provider, backed by Wiener Linien's
// keyless real-time monitor and its open-data CSVs (stations, platforms,
// lines and platform sequences), downloaded daily.
package wienerlinien

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
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
	DefaultBaseURL = "https://www.wienerlinien.at/ogd_realtime"
	dataTTL        = 24 * time.Hour
)

var vienna = transit.Info{
	ID:       "vienna",
	Name:     "Vienna (Wiener Linien)",
	Country:  "austria",
	Provider: "wienerlinien",
	TZ:       "Europe/Vienna",
	Realtime: true,
	Notes:    "first run downloads the open-data stop and line tables (2 MB)",
	Probe:    transit.Probe{Route: "5", Query: "praterstern"},
}

func init() {
	registry.RegisterCountry(registry.Country{ID: "austria", Name: "Austria", Aliases: []string{"at", "österreich", "oesterreich"}, Providers: []string{"wienerlinien"},
		Coverage: "Vienna and the towns Wiener Linien reaches", AnyTown: NewTown})
	registry.Register(registry.Entry{Info: vienna, Aliases: []string{"wien"}, New: New})
}

// data is the open-data tables, loaded once and shared across towns.
type data struct {
	stations  map[string]station        // by DIVA
	platforms map[string]string         // RBL → DIVA
	lines     []line                    // from linien.csv
	patterns  map[string][][]patternRow // LineID → rows per pattern
}

type station struct {
	DIVA, Name, Municipality string
	Lat, Lon                 float64
}

type line struct {
	ID, Text, Mode string
}

type patternRow struct {
	seq       int
	rbl       string
	direction string
}

type Provider struct {
	BaseURL string
	info    transit.Info
	town    string // municipality to keep; "" keeps everything
	http    *httpx.Client
	shared  *cache.Cache // provider-wide: the CSVs are network-wide
	now     func() time.Time
	loc     *time.Location

	mu   sync.Mutex
	data *data
}

func New(d registry.Deps) transit.Provider { return build(d, vienna, "Wien") }

// NewTown builds a provider for any municipality in the Wiener Linien
// network (the CSVs carry one per station).
func NewTown(d registry.Deps, town string) transit.Provider {
	info := vienna
	info.ID = strings.ReplaceAll(match.Normalize(town), " ", "-")
	info.Name = town + " (Wiener Linien)"
	info.Probe = transit.Probe{}
	return build(d, info, town)
}

func build(d registry.Deps, info transit.Info, town string) *Provider {
	return &Provider{BaseURL: DefaultBaseURL, info: info, town: town, http: httpx.New("wienerlinien", d.HTTP), shared: cache.Default("wienerlinien", dataTTL), now: d.Clock(), loc: info.Location()}
}

func (p *Provider) Info() transit.Info { return p.info }

// Cold reports whether the open-data tables still have to be downloaded.
func (p *Provider) Cold() bool {
	var raw map[string]string
	return !p.shared.Load("csv", &raw) || len(raw) < 4
}

var csvFiles = []string{"haltestellen", "haltepunkte", "linien", "fahrwegverlaeufe"}

// load fetches the four CSVs (cached as raw text for a day) and indexes them.
func (p *Provider) load(ctx context.Context) (*data, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data != nil {
		return p.data, nil
	}
	var raw map[string]string
	if !p.shared.Load("csv", &raw) || len(raw) < len(csvFiles) {
		raw = map[string]string{}
		for _, name := range csvFiles {
			b, err := p.http.GetBytes(ctx, httpx.Join(p.BaseURL, "doku/ogd/wienerlinien-ogd-"+name+".csv"))
			if err != nil {
				return nil, err
			}
			raw[name] = string(b)
		}
		_ = p.shared.Store("csv", raw)
	}
	d := &data{stations: map[string]station{}, platforms: map[string]string{}, patterns: map[string][][]patternRow{}}
	err := each(raw["haltestellen"], func(r map[string]string) {
		s := station{DIVA: r["DIVA"], Name: r["PlatformText"], Municipality: r["Municipality"]}
		s.Lat, _ = strconv.ParseFloat(r["Latitude"], 64)
		s.Lon, _ = strconv.ParseFloat(r["Longitude"], 64)
		if s.DIVA != "" && s.Name != "" {
			d.stations[s.DIVA] = s
		}
	})
	if err != nil {
		return nil, err
	}
	if err := each(raw["haltepunkte"], func(r map[string]string) { d.platforms[r["StopID"]] = r["DIVA"] }); err != nil {
		return nil, err
	}
	if err := each(raw["linien"], func(r map[string]string) {
		d.lines = append(d.lines, line{ID: r["LineID"], Text: r["LineText"], Mode: mode(r["MeansOfTransport"])})
	}); err != nil {
		return nil, err
	}
	byPattern := map[string]map[string][]patternRow{} // line → pattern id → rows
	if err := each(raw["fahrwegverlaeufe"], func(r map[string]string) {
		seq, _ := strconv.Atoi(r["StopSeqCount"])
		if byPattern[r["LineID"]] == nil {
			byPattern[r["LineID"]] = map[string][]patternRow{}
		}
		byPattern[r["LineID"]][r["PatternID"]] = append(byPattern[r["LineID"]][r["PatternID"]], patternRow{seq, r["StopID"], r["Direction"]})
	}); err != nil {
		return nil, err
	}
	for lineID, pats := range byPattern {
		for _, rows := range pats {
			sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
			d.patterns[lineID] = append(d.patterns[lineID], rows)
		}
	}
	p.data = d
	return d, nil
}

func mode(means string) string {
	switch means {
	case "ptMetro":
		return "metro"
	case "ptTram", "ptTramWLB":
		return "tram"
	case "ptBusCity", "ptBusNight", "ptBusRegional":
		return "bus"
	}
	return ""
}

// each parses a semicolon-separated CSV with a header row.
func each(text string, fn func(map[string]string)) error {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\ufeff")))
	r.Comma = ';'
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("wienerlinien: csv: %v", err)
	}
	for {
		row, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wienerlinien: csv: %v", err)
		}
		rec := make(map[string]string, len(header))
		for i, h := range header {
			if i < len(row) {
				rec[h] = row[i]
			}
		}
		fn(rec)
	}
}

func (p *Provider) FindRoutes(ctx context.Context, short string) ([]transit.Route, error) {
	d, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	want := match.Normalize(short)
	var out []transit.Route
	var others []string
	for _, l := range d.lines {
		if match.Normalize(l.Text) == want {
			out = append(out, transit.Route{ID: l.ID, ShortName: l.Text, Mode: l.Mode})
			continue
		}
		others = append(others, l.Text)
	}
	if len(out) == 0 {
		return nil, transit.UnknownRoute(short, others)
	}
	return out, nil
}

// RouteStops maps each platform sequence of the line to its stations
// (consecutive platforms of one station collapse), one pattern per
// sequence; the app merges them per direction, longest first.
func (p *Provider) RouteStops(ctx context.Context, r transit.Route) ([]transit.Pattern, error) {
	d, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	pats, ok := d.patterns[r.ID]
	if !ok {
		return nil, fmt.Errorf("wienerlinien: line %s: %w", r.ShortName, transit.ErrNotFound)
	}
	var out []transit.Pattern
	for _, rows := range pats {
		pat := transit.Pattern{}
		last := ""
		for _, row := range rows {
			pat.DirectionID = row.direction
			diva := d.platforms[row.rbl]
			s, ok := d.stations[diva]
			if !ok || diva == last {
				continue
			}
			last = diva
			pat.Stops = append(pat.Stops, transit.Stop{ID: diva, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
		}
		if len(pat.Stops) == 0 {
			continue
		}
		pat.Headsign = pat.Stops[len(pat.Stops)-1].Name
		out = append(out, pat)
	}
	return out, nil
}

// SearchStops matches station names within the town's municipality.
func (p *Provider) SearchStops(ctx context.Context, query string) ([]transit.Stop, error) {
	d, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	town := match.Normalize(p.town)
	var all []match.Stop
	for _, s := range d.stations {
		if town != "" && match.Normalize(s.Municipality) != town {
			continue
		}
		all = append(all, match.Stop{ID: s.DIVA, Name: s.Name})
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("wienerlinien: no stations in a municipality named %q", p.town)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].ID < all[j].ID
	})
	cands := match.Find(query, all)
	if len(cands) == 0 {
		cands = match.Closest(query, all, 8)
	}
	var out []transit.Stop
	for _, c := range cands {
		for _, id := range c.IDs {
			s := d.stations[id]
			out = append(out, transit.Stop{ID: id, Name: s.Name, Lat: s.Lat, Lon: s.Lon})
		}
	}
	return out, nil
}

const monitorTime = "2006-01-02T15:04:05.000-0700"

// Departures asks the monitor for every requested station in one call. The
// monitor has no line filter; the app filters by Line.
func (p *Provider) Departures(ctx context.Context, stopIDs []string, _ []transit.Route, window time.Duration) (*transit.Departures, error) {
	out := &transit.Departures{Now: p.now()}
	if len(stopIDs) == 0 {
		return out, nil
	}
	q := url.Values{"diva": stopIDs}
	var resp monitorResp
	if err := p.http.GetJSON(ctx, httpx.URL(p.BaseURL, "monitor", q), &resp); err != nil {
		return nil, err
	}
	if resp.Message.MessageCode != 1 && resp.Message.MessageCode != 0 {
		return nil, fmt.Errorf("wienerlinien monitor: %s (%d)", resp.Message.Value, resp.Message.MessageCode)
	}
	if t, err := time.Parse(monitorTime, resp.Message.ServerTime); err == nil {
		out.Now = t.In(p.loc)
	}
	cutoff := out.Now.Add(window)
	for _, m := range resp.Data.Monitors {
		diva := m.LocationStop.Properties.Name
		for _, l := range m.Lines {
			for _, dep := range l.Departures.Departure {
				d := transit.Departure{
					Headsign:    l.Towards,
					Line:        l.Name,
					RouteID:     strconv.Itoa(l.LineID),
					DirectionID: l.RichtungsID,
					Platform:    l.Platform,
					StopID:      diva,
				}
				if dep.Vehicle != nil && dep.Vehicle.Towards != "" {
					d.Headsign = dep.Vehicle.Towards
				}
				if d.DirectionID == "" {
					d.DirectionID = l.Direction
				}
				if t, err := time.Parse(monitorTime, dep.DepartureTime.TimePlanned); err == nil {
					d.Scheduled = t.In(p.loc)
				}
				d.At = d.Scheduled
				if t, err := time.Parse(monitorTime, dep.DepartureTime.TimeReal); err == nil {
					d.At, d.Live = t.In(p.loc), true
				}
				if d.At.IsZero() || (window > 0 && d.At.After(cutoff)) {
					continue
				}
				out.Departures = append(out.Departures, d)
			}
		}
	}
	transit.SortDepartures(out.Departures)
	return out, nil
}

var (
	_ transit.RouteLister  = (*Provider)(nil)
	_ transit.StopSearcher = (*Provider)(nil)
	_ transit.ColdStarter  = (*Provider)(nil)
)
