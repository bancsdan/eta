// Command eta prints the next real-time departures at a public-transport stop, in any supported city.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/app"
	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/config"
	_ "github.com/bancsdan/eta/internal/providers/all"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

const (
	usage = `usage: eta <country> <town> <stop-query> [-c N] [-t] [-j] [-r] [-w]
       eta <country> <town> <route> <stop-query> [flags]
       eta <country> <town> <route> -l [-j]
       eta <town> ...              (with default_country set)
       eta <route> <stop-query>    (with default_country and default_town set)
       eta <alias> [flags]
       eta <country> <town> --setup
       eta countries | eta cities [<country>] [--check]

Print the next departures at a stop: every line, or just one route.

  <country>     norway, switzerland, uk, usa ... ("eta countries" lists them)
  <town>        the town the stop is in: oslo, halden, zurich, london ...
                (any town where the country's provider can place it;
                "eta cities <country>" lists the verified ones)
  <route>       route short name as riders know it: 155, M4, Northern, Red
  <stop-query>  free-text stop name; accent-insensitive and fuzzy
                ("viranyos" matches "Virányos út")
  <alias>       a name from the config file (see -a)

  -c, --count N   departures to show per direction (default 1)
  -t, --times     also print clock times, e.g. 5m42s (22:41)
  -j, --json      print JSON instead of text
  -l, --list      list the route's stops by direction instead of departures
  -r, --refresh   ignore the route/stop cache (~/.cache/eta)
  -w, --watch     keep the board on screen, refreshing every 30 s
      --every N   refresh interval in seconds for -w
  -a, --aliases   show the configured aliases and exit
      --setup     prompt for the provider's API key and save it
      --save NAME after a successful lookup, save the command as alias NAME
      --version   print the version and exit
  -h, --help      show this help

Times are real-time predictions; ~ marks schedule-only entries. When a stop
query is ambiguous eta asks you to choose (or, with a route, picks the best
match and says what else matched).

API keys are named after the data provider: ETA_<PROVIDER>_API_KEY
(e.g. ETA_BKK_API_KEY for Budapest) or $XDG_CONFIG_HOME/eta/keys
(~/.config/eta/keys), one "provider = key" per line. "eta countries" shows
which need a key and where to get one; "eta <country> <town> --setup" saves it.

$XDG_CONFIG_HOME/eta/config (~/.config/eta/config) holds the defaults and
aliases, one per line as "name = args"; "default" runs with no args.
"eta norway oslo 31 jernbanetorget -c 2 --save home" writes the "home" line:

  default_country = norway
  default_town    = oslo
  home            = 31 jernbanetorget -c 2
  work            = uk london Central bank -t
  default         = home`
)

// Set by GoReleaser through -ldflags "-X main.version=..."; "dev" for local builds.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

const (
	cacheTTL    = 24 * time.Hour
	timeout     = 10 * time.Second
	coldTimeout = 90 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ue *app.UsageError
		var ce *config.UsageError
		switch {
		case errors.As(err, &ue):
			fmt.Fprintln(os.Stderr, ue.Msg)
		case errors.As(err, &ce):
			fmt.Fprintln(os.Stderr, ce.Msg)
		default:
			fmt.Fprintln(os.Stderr, "eta:", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "cities" {
		return runCities(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "countries" {
		return runCountries(os.Stdout)
	}
	cfg, err := config.Load(config.ConfigFile())
	if err != nil {
		return err
	}
	opts, err := parseArgs(args, cfg)
	if err != nil {
		return err
	}
	if opts == nil { // help or alias listing already printed
		return nil
	}
	info, newProvider, err := registry.Resolve(opts.Country, opts.Town)
	if err != nil {
		return &app.UsageError{Msg: err.Error() + " (run \"eta countries\")"}
	}
	opts.Country, opts.Town = info.Country, info.ID
	if opts.setup {
		return runSetup(info, os.Stdin, os.Stdout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ns := info.Provider + "/" + info.ID
	p := newProvider(registry.Deps{
		Key:   config.Key,
		HTTP:  &http.Client{},
		Cache: cache.Default(ns, cacheTTL),
		Now:   time.Now,
	})
	budget := timeout
	if cs, ok := p.(transit.ColdStarter); ok && cs.Cold() {
		budget = coldTimeout
		fmt.Fprintf(os.Stderr, "eta: first run for %s downloads its stop list; this can take a minute\n", info.Name)
	}

	interactive := isTTY(os.Stdin) && isTTY(os.Stdout) && !opts.JSON
	a := &app.App{
		Provider: p,
		Cache:    cache.Default(ns, cacheTTL),
		Out:      os.Stdout,
		Err:      os.Stderr,
		Color:    colorEnabled() && !opts.JSON,
	}
	if interactive {
		a.Pick = pickFromTerminal
	}
	once := func() error {
		rctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		err := a.Run(rctx, opts.Options)
		if errors.Is(err, transit.ErrNoKey) || errors.Is(err, transit.ErrUnauthorized) {
			if k, ok := missingKey(info); ok {
				return config.MissingKeyError(info, k)
			}
			for _, k := range info.Keys {
				if errors.Is(err, transit.ErrUnauthorized) {
					return fmt.Errorf("%v; check %s", err, k.Env)
				}
			}
		}
		return err
	}
	if !opts.watch {
		if err := once(); err != nil {
			return err
		}
		return saveAlias(opts)
	}
	return watch(ctx, once, opts.every, isTTY(os.Stdout), func() error { return saveAlias(opts) })
}

// saveAlias records the command as opts.save once a lookup succeeded, so a
// mistyped stop never becomes an alias.
func saveAlias(opts *cliOptions) error {
	if opts.save == "" {
		return nil
	}
	value := aliasValue(opts)
	if err := config.SetAlias(opts.save, value); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "eta: saved alias %s = %s (in %s)\n", opts.save, value, config.ConfigFile())
	return nil
}

// aliasValue rebuilds the command line from the resolved options: the city
// is always included so the alias survives a change of default_city, and a
// multi-word stop query is quoted so it is not read back as route + stop.
func aliasValue(o *cliOptions) string {
	parts := []string{o.Country, quoteArg(o.Town)}
	if o.Route != "" {
		parts = append(parts, quoteArg(o.Route))
	}
	if o.Query != "" {
		parts = append(parts, quoteArg(o.Query))
	}
	if o.List {
		parts = append(parts, "-l")
	}
	if o.Count > 1 {
		parts = append(parts, "-c", strconv.Itoa(o.Count))
	}
	if o.ShowClock {
		parts = append(parts, "-t")
	}
	if o.JSON {
		parts = append(parts, "-j")
	}
	return strings.Join(parts, " ")
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	return s
}

// splitArgs tokenizes an alias value like a shell would for the simple
// cases: whitespace separates, single or double quotes group.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// Usage errors stop the loop: a retry cannot fix them.
func watch(ctx context.Context, once func() error, every time.Duration, tty bool, onFirstSuccess func() error) error {
	for {
		if tty {
			fmt.Print("\x1b[2J\x1b[H")
		}
		err := once()
		var ue *app.UsageError
		if errors.As(err, &ue) {
			return err
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "eta:", err)
		} else if onFirstSuccess != nil {
			if err := onFirstSuccess(); err != nil {
				return err
			}
			onFirstSuccess = nil
		}
		fmt.Fprintf(os.Stdout, "\n(refreshing every %s, Ctrl-C to stop)\n", every)
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case <-time.After(every):
		}
	}
}

func pickFromTerminal(names []string) int {
	fmt.Fprintln(os.Stderr, "Several stops match; which one?")
	for i, n := range names {
		fmt.Fprintf(os.Stderr, "  %d. %s\n", i+1, n)
	}
	fmt.Fprint(os.Stderr, "> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr)
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(names) {
		return -1
	}
	return n - 1
}

func runSetup(info transit.Info, in io.Reader, out io.Writer) error {
	if len(info.Keys) == 0 {
		fmt.Fprintf(out, "%s needs no API key.\n", info.Name)
		return nil
	}
	rd := bufio.NewReader(in)
	for _, k := range info.Keys {
		label := k.Env
		if k.Label != "" {
			label = k.Label + " key (" + k.Env + ")"
		}
		req := "required"
		if k.Req == transit.KeyOptional {
			req = "optional, raises the rate limit"
		}
		fmt.Fprintf(out, "%s: %s (%s)\n", info.Name, label, req)
		if k.SignupURL != "" {
			fmt.Fprintf(out, "  get one at %s\n", k.SignupURL)
		}
		if cur := config.Key(k.Env); cur != "" {
			fmt.Fprintf(out, "  currently set (%s…); press Enter to keep it\n", cur[:min(4, len(cur))])
		}
		fmt.Fprint(out, "  key: ")
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return nil
		}
		val := strings.TrimSpace(line)
		if val == "" {
			continue
		}
		if err := config.SetKey(k.ConfigName(), val); err != nil {
			return err
		}
		fmt.Fprintf(out, "  saved to %s as %q\n", config.KeysFile(), k.ConfigName())
	}
	return nil
}

func versionString() string {
	s := "eta " + version
	if commit != "" {
		s += " (" + commit
		if date != "" {
			s += ", " + date
		}
		s += ")"
	}
	return s
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func missingKey(info transit.Info) (transit.KeySpec, bool) {
	for _, k := range info.Keys {
		if k.Req == transit.KeyRequired && config.Key(k.Env) == "" {
			return k, true
		}
	}
	return transit.KeySpec{}, false
}

type cliOptions struct {
	app.Options
	watch bool
	every time.Duration
	setup bool
	save  string
}

func parseArgs(args []string, cfg *config.Config) (*cliOptions, error) {
	o, pos, err := parseOnce(args)
	if err != nil || o == nil {
		return o, err
	}
	aliases := cfg.Aliases
	switch {
	case len(pos) > 0 && aliases[pos[0]] != "":
		expanded := append(splitArgs(aliases[pos[0]]), removeFirst(args, pos[0])...)
		o, pos, err = parseOnce(expanded)
	case len(pos) == 0 && aliases["default"] != "":
		def := aliases["default"]
		if aliases[def] != "" { // "default = home"
			def = aliases[def]
		}
		o, pos, err = parseOnce(append(splitArgs(def), args...))
	}
	if err != nil || o == nil {
		return o, err
	}
	// Country: from the arguments when the first word names one, else the
	// configured default.
	if len(pos) > 0 {
		if c, ok := registry.LookupCountry(pos[0]); ok {
			o.Country = c.ID
			pos = pos[1:]
		}
	}
	if o.Country == "" {
		if cfg.DefaultCountry == "" {
			if len(pos) == 0 {
				return nil, &app.UsageError{Msg: usage}
			}
			return nil, &app.UsageError{Msg: fmt.Sprintf("unknown country %q (run \"eta countries\", or set default_country in %s)\n%s", pos[0], config.ConfigFile(), usage)}
		}
		o.Country = cfg.DefaultCountry
	}
	// Town: required unless default_town is set; a registered town in the
	// arguments always wins over the default.
	switch {
	case len(pos) > 0 && cfg.DefaultTown == "":
		o.Town = pos[0]
		pos = pos[1:]
	case len(pos) > 0 && cfg.DefaultTown != "":
		if _, ok := registry.LookupTown(o.Country, pos[0]); ok {
			o.Town = pos[0]
			pos = pos[1:]
		} else {
			o.Town = cfg.DefaultTown
		}
	case cfg.DefaultTown != "":
		o.Town = cfg.DefaultTown
	default:
		return nil, &app.UsageError{Msg: usage}
	}
	if o.setup {
		return o, nil
	}
	switch {
	case len(pos) == 0:
		return nil, &app.UsageError{Msg: usage}
	case o.List:
		o.Route = pos[0]
	case len(pos) == 1:
		o.Query = pos[0]
	default:
		o.Route = pos[0]
		o.Query = strings.Join(pos[1:], " ")
	}
	return o, nil
}

func parseOnce(args []string) (*cliOptions, []string, error) {
	o := &cliOptions{Options: app.Options{Count: 1}, every: 30 * time.Second}
	var pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, val, hasVal := strings.Cut(arg, "=")
		switch name {
		case "-h", "--help":
			fmt.Println(usage)
			return nil, nil, nil
		case "--version":
			fmt.Println(versionString())
			return nil, nil, nil
		case "-a", "--aliases":
			printAliases()
			return nil, nil, nil
		case "-t", "--times":
			o.ShowClock = true
		case "-j", "--json":
			o.JSON = true
		case "-r", "--refresh":
			o.Refresh = true
		case "-l", "--list":
			o.List = true
		case "-w", "--watch":
			o.watch = true
		case "--setup":
			o.setup = true
		case "--save":
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, &app.UsageError{Msg: "--save needs an alias name\n" + usage}
				}
				i++
				val = args[i]
			}
			if strings.TrimSpace(val) == "" || strings.HasPrefix(val, "-") {
				return nil, nil, &app.UsageError{Msg: fmt.Sprintf("--save: %q is not an alias name", val)}
			}
			o.save = val
		case "--every":
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, &app.UsageError{Msg: "--every needs a number of seconds\n" + usage}
				}
				i++
				val = args[i]
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 5 {
				return nil, nil, &app.UsageError{Msg: fmt.Sprintf("--every: %q is not a number of seconds (minimum 5)", val)}
			}
			o.every = time.Duration(n) * time.Second
		case "-c", "--count":
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, &app.UsageError{Msg: "-c needs a number\n" + usage}
				}
				i++
				val = args[i]
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, nil, &app.UsageError{Msg: fmt.Sprintf("-c: %q is not a positive number", val)}
			}
			o.Count = n
		default:
			if strings.HasPrefix(arg, "-") && len(arg) > 1 {
				return nil, nil, &app.UsageError{Msg: fmt.Sprintf("unknown flag %s\n%s", arg, usage)}
			}
			pos = append(pos, arg)
		}
	}
	return o, pos, nil
}

func removeFirst(args []string, tok string) []string {
	out := make([]string, 0, len(args))
	removed := false
	for _, a := range args {
		if !removed && a == tok {
			removed = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func printAliases() {
	path := config.ConfigFile()
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Println(err)
		return
	}
	if len(cfg.Aliases) == 0 {
		fmt.Printf("no aliases; add lines like \"home = berlin M4 alexanderplatz -c 3\" to %s\n", path)
		return
	}
	names := make([]string, 0, len(cfg.Aliases))
	width := 0
	for n := range cfg.Aliases {
		names = append(names, n)
		if len(n) > width {
			width = len(n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("%-*s = %s\n", width, n, cfg.Aliases[n])
	}
}

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
