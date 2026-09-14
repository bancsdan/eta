package main

import (
	"context"
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

const citiesUsage = `usage: eta cities [--check]

List the supported cities, whether each is ready to use and where to get a
key for the ones that need one. --check makes one real request per city.`

func runCities(args []string, out io.Writer) error {
	check := false
	for _, a := range args {
		switch a {
		case "--check", "-c":
			check = true
		case "-h", "--help":
			fmt.Fprintln(out, citiesUsage)
			return nil
		default:
			return fmt.Errorf("cities: unknown argument %q\n%s", a, citiesUsage)
		}
	}
	entries := registry.All()
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
	header := "CITY\tNAME\tPROVIDER\tSTATUS\tKEY"
	if check {
		header += "\tCHECK"
	}
	fmt.Fprintln(tw, header)
	for i, e := range entries {
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
