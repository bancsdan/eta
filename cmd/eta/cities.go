package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/config"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const citiesUsage = `usage: eta cities [<country>] [--check]
       eta countries

"eta countries" lists the covered countries with their providers and
coverage: every town, or the named ones. "eta cities" lists the towns eta
probes regularly, grouped by country; give a country to list just that one.
--check makes one real request per listed town and reports ok or the error.`

// runCountries prints one line per country: id, providers, verified towns,
// whether any other town works, and key status.
func runCountries(out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COUNTRY\tNAME\tPROVIDERS\tCOVERAGE\tSTATUS")
	for _, c := range registry.Countries() {
		towns := registry.Towns(c.ID)
		ready, needKey := 0, 0
		for _, e := range towns {
			if _, missing := missingKey(e.Info); missing {
				needKey++
			} else {
				ready++
			}
		}
		status := "ready"
		if needKey > 0 {
			status = fmt.Sprintf("%d ready, %d need a key", ready, needKey)
		}
		names := make([]string, 0, len(towns))
		for _, e := range towns {
			names = append(names, e.Info.ID)
		}
		coverage := strings.Join(names, ", ")
		switch {
		case c.AnyTown != nil && c.Coverage != "":
			coverage = fmt.Sprintf("%s (%d probed)", c.Coverage, len(towns))
		case c.AnyTown != nil:
			coverage = fmt.Sprintf("every town (%d probed)", len(towns))
		}
		id := c.ID
		if len(c.Aliases) > 0 {
			id += " (" + strings.Join(c.Aliases, ", ") + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, c.Name, strings.Join(c.Providers, ", "), coverage, status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%d countries. \"eta cities <country>\" lists the towns probed regularly; any town works where coverage says so.\n", len(registry.Countries()))
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
	if country == "" {
		entries = registry.Towns("")
	} else {
		c, ok := registry.LookupCountry(country)
		if !ok {
			return fmt.Errorf("unknown country %q (run \"eta countries\")", country)
		}
		entries = registry.Towns(c.ID)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no verified towns in %q (run \"eta countries\")", country)
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
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "TOWN\tNAME\tPROVIDER\tSTATUS\tKEY"
	if check {
		header += "\tCHECK"
	}
	last := ""
	for i, e := range entries {
		if e.Info.Country != last {
			if last != "" {
				fmt.Fprintln(tw, "\t\t\t\t")
			}
			c, _ := registry.LookupCountry(e.Info.Country)
			title := strings.ToUpper(c.Name) + " (" + c.ID + ")"
			if c.AnyTown != nil {
				title += "  every town works; these are probed regularly"
			}
			fmt.Fprintf(tw, "%s\t\t\t\t\n", title)
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
	fmt.Fprintf(out, "\nKeys: export ETA_<PROVIDER>_API_KEY=..., add \"<provider> = <key>\" lines to %s, or run eta <country> <town> --setup\n", config.KeysFile())
	return nil
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
		Cache: cache.Default(e.Info.Provider+"/"+e.Info.ID, cacheTTL),
		Now:   time.Now,
	})
	probe := e.Info.Probe
	var stopIDs []string
	var routes []transit.Route
	rl, hasRoutes := p.(transit.RouteLister)
	ss, hasSearch := p.(transit.StopSearcher)
	if hasRoutes {
		rs, err := rl.FindRoutes(ctx, probe.Route)
		switch {
		case err == nil && len(rs) > 0:
			ps, err := rl.RouteStops(ctx, rs[0])
			if err != nil {
				return "FAIL: " + err.Error()
			}
			if len(ps) == 0 || len(ps[0].Stops) == 0 {
				return "FAIL: probe route has no stops"
			}
			routes = rs
			stopIDs = []string{ps[0].Stops[len(ps[0].Stops)/2].ID}
		case hasSearch && errors.Is(err, transit.ErrNoRouteLookup):
			// Same fallback as the app: match the route at the stop.
		case err != nil:
			return "FAIL: " + err.Error()
		default:
			return "FAIL: probe route " + probe.Route + " not found"
		}
	}
	if stopIDs == nil {
		if !hasSearch {
			return "FAIL: provider cannot search"
		}
		found, err := ss.SearchStops(ctx, probe.Query)
		if err != nil {
			return "FAIL: " + err.Error()
		}
		if len(found) == 0 {
			return "FAIL: probe stop " + probe.Query + " not found"
		}
		stopIDs = []string{found[0].ID}
	}
	if _, err := p.Departures(ctx, stopIDs, routes, 60*time.Minute); err != nil {
		return "FAIL: " + err.Error()
	}
	return "ok"
}
