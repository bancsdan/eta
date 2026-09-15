package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/config"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const citiesUsage = `usage: eta cities [<country>] [--check]
       eta countries

"eta countries" lists the covered countries with their providers and how
many cities each has. "eta cities" lists every city grouped by country;
give a country to list just that one. --check makes one real request per
listed city and reports ok or the error.`

// runCountries prints one line per country: cities, providers, key status.
func runCountries(out io.Writer) error {
	type agg struct {
		cities    int
		providers map[string]bool
		needKey   int
		ready     int
	}
	byCountry := map[string]*agg{}
	for _, e := range registry.All() {
		a := byCountry[e.Info.Country]
		if a == nil {
			a = &agg{providers: map[string]bool{}}
			byCountry[e.Info.Country] = a
		}
		a.cities++
		a.providers[e.Info.Provider] = true
		if _, missing := missingKey(e.Info); missing {
			a.needKey++
		} else {
			a.ready++
		}
	}
	names := make([]string, 0, len(byCountry))
	for n := range byCountry {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COUNTRY\tCITIES\tPROVIDERS\tSTATUS")
	for _, n := range names {
		a := byCountry[n]
		provs := make([]string, 0, len(a.providers))
		for p := range a.providers {
			provs = append(provs, p)
		}
		sort.Strings(provs)
		status := "ready"
		if a.needKey > 0 {
			status = fmt.Sprintf("%d ready, %d need a key", a.ready, a.needKey)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", n, a.cities, strings.Join(provs, ", "), status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%d cities in %d countries. \"eta cities <country>\" lists one country.\n", len(registry.All()), len(names))
	return nil
}

// runCities prints the cities grouped by country, optionally one country
// only. With --check every city that has its keys performs one live lookup.
func runCities(args []string, out io.Writer) error {
	check := false
	var country string
	for _, a := range args {
		switch a {
		case "--check", "-c":
			check = true
		case "-h", "--help":
			fmt.Fprintln(out, citiesUsage)
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("cities: unknown flag %q\n%s", a, citiesUsage)
			}
			country += " " + a
		}
	}
	country = strings.TrimSpace(country)
	var entries []registry.Entry
	for _, e := range registry.All() {
		if country != "" && !countryMatches(e.Info.Country, country) {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no cities in %q (run \"eta countries\")", country)
	}
	results := make([]string, len(entries))
	if check {
		var wg sync.WaitGroup
		for i, e := range entries {
			if _, missing := missingKey(e.Info); missing {
				results[i] = "skipped (no key)"
				continue
			}
			wg.Add(1)
			go func(i int, e registry.Entry) {
				defer wg.Done()
				results[i] = checkCity(e)
			}(i, e)
		}
		wg.Wait()
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Info.Country != entries[j].Info.Country {
			return entries[i].Info.Country < entries[j].Info.Country
		}
		return entries[i].Info.ID < entries[j].Info.ID
	})
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "CITY\tNAME\tPROVIDER\tSTATUS\tKEY"
	if check {
		header += "\tCHECK"
	}
	last := ""
	for i, e := range entries {
		if e.Info.Country != last {
			if last != "" {
				fmt.Fprintln(tw, "\t\t\t\t")
			}
			fmt.Fprintf(tw, "%s\t\t\t\t\n", strings.ToUpper(e.Info.Country))
			fmt.Fprintln(tw, header)
			last = e.Info.Country
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", cityLabel(e), e.Info.Name, e.Info.Provider, status(e.Info), keyHelp(e.Info))
		if check {
			row += "\t" + results[i]
		}
		fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nKeys: export ETA_<PROVIDER>_API_KEY=..., add \"<provider> = <key>\" lines to %s, or run eta <city> --setup\n", config.KeysFile())
	return nil
}

func countryMatches(have, want string) bool {
	h, w := match.Normalize(have), match.Normalize(want)
	return h == w || strings.HasPrefix(h, w)
}

func cityLabel(e registry.Entry) string {
	if len(e.Aliases) == 0 {
		return e.Info.ID
	}
	return e.Info.ID + " (" + strings.Join(e.Aliases, ", ") + ")"
}

func status(info transit.Info) string {
	var missing []string
	optionalMissing := false
	for _, k := range info.Keys {
		if config.Key(k.Env) != "" {
			continue
		}
		switch k.Req {
		case transit.KeyRequired:
			missing = append(missing, k.Env)
		case transit.KeyOptional:
			optionalMissing = true
		}
	}
	switch {
	case len(missing) > 0:
		return "needs key: " + strings.Join(missing, ", ")
	case optionalMissing:
		return "ready (no key: rate-limited)"
	default:
		return "ready"
	}
}

func keyHelp(info transit.Info) string {
	if len(info.Keys) == 0 {
		return "none needed"
	}
	parts := make([]string, 0, len(info.Keys))
	for _, k := range info.Keys {
		s := k.SignupURL
		if k.Label != "" {
			s = k.Label + ": " + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

func checkCity(e registry.Entry) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	p := e.New(registry.Deps{
		Key:   config.Key,
		HTTP:  &http.Client{},
		Cache: cache.Default(e.Info.ID, cacheTTL),
		Now:   time.Now,
	})
	probe := e.Info.Probe
	var stopIDs []string
	var routes []transit.Route
	switch v := p.(type) {
	case transit.RouteLister:
		rs, err := v.FindRoutes(ctx, probe.Route)
		if err != nil {
			return "FAIL: " + err.Error()
		}
		if len(rs) == 0 {
			return "FAIL: probe route " + probe.Route + " not found"
		}
		ps, err := v.RouteStops(ctx, rs[0])
		if err != nil {
			return "FAIL: " + err.Error()
		}
		if len(ps) == 0 || len(ps[0].Stops) == 0 {
			return "FAIL: probe route has no stops"
		}
		routes = rs
		stopIDs = []string{ps[0].Stops[len(ps[0].Stops)/2].ID}
	case transit.StopSearcher:
		ss, err := v.SearchStops(ctx, probe.Query)
		if err != nil {
			return "FAIL: " + err.Error()
		}
		if len(ss) == 0 {
			return "FAIL: probe stop " + probe.Query + " not found"
		}
		stopIDs = []string{ss[0].ID}
	default:
		return "FAIL: provider cannot search"
	}
	if _, err := p.Departures(ctx, stopIDs, routes, 60*time.Minute); err != nil {
		return "FAIL: " + err.Error()
	}
	return "ok"
}
