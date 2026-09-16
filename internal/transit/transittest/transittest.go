// Package transittest is the conformance harness every provider's tests run:
// Conform checks the contract against fixtures, Live does one real round
// trip when the city's keys are present, and Serve replays testdata files.
package transittest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/transit"
)

type Case struct {
	Route     string
	Query     string
	WantStop  string   // expected stop name after matching (substring, normalized)
	WantLines []string // every filtered departure's Line normalizes to one of these; nil skips
	Now       time.Time
}

func Conform(t *testing.T, p transit.Provider, c Case) {
	t.Helper()
	info := p.Info()
	if info.ID == "" || info.ID != strings.ToLower(info.ID) || strings.ContainsAny(info.ID, " /") {
		t.Fatalf("Info().ID %q must be a lowercase slug", info.ID)
	}
	if info.Name == "" || info.Provider == "" || info.Country == "" {
		t.Fatalf("Info() needs Name, Provider and Country: %+v", info)
	}
	if _, err := time.LoadLocation(info.TZ); err != nil {
		t.Fatalf("Info().TZ %q: %v", info.TZ, err)
	}
	for _, k := range info.Keys {
		if !strings.HasPrefix(k.Env, "ETA_") || !strings.HasSuffix(k.Env, "_API_KEY") {
			t.Fatalf("key env %q must look like ETA_<PROVIDER>_API_KEY", k.Env)
		}
	}
	ctx := context.Background()
	rl, hasRoutes := p.(transit.RouteLister)
	ss, hasSearch := p.(transit.StopSearcher)
	if !hasRoutes && !hasSearch {
		t.Fatalf("provider implements neither RouteLister nor StopSearcher")
	}

	var stopIDs []string
	var routes []transit.Route
	if hasRoutes {
		var err error
		routes, err = rl.FindRoutes(ctx, c.Route)
		if err != nil {
			t.Fatalf("FindRoutes(%q): %v", c.Route, err)
		}
		if len(routes) == 0 {
			t.Fatalf("FindRoutes(%q) returned nothing", c.Route)
		}
		for _, r := range routes {
			if r.ID == "" || r.ShortName == "" {
				t.Fatalf("route %+v needs ID and ShortName", r)
			}
			if match.Normalize(r.ShortName) != match.Normalize(c.Route) {
				t.Fatalf("FindRoutes(%q) returned non-matching short name %q", c.Route, r.ShortName)
			}
		}
		if _, err := rl.FindRoutes(ctx, "zz-no-such-route-zz"); err == nil {
			t.Errorf("FindRoutes on a bogus route should fail")
		} else if _, ok := err.(*transit.UnknownRouteError); !ok && !isUnknown(err) {
			t.Logf("note: bogus route returned %T (%v), not *transit.UnknownRouteError", err, err)
		}
		var stops []match.Stop
		for _, r := range routes {
			ps, err := rl.RouteStops(ctx, r)
			if err != nil {
				t.Fatalf("RouteStops(%s): %v", r.ID, err)
			}
			if len(ps) == 0 {
				t.Fatalf("RouteStops(%s) returned no patterns", r.ID)
			}
			for _, pat := range ps {
				if len(pat.Stops) == 0 {
					t.Fatalf("pattern %+v has no stops", pat.DirectionID)
				}
				if pat.Headsign == "" && pat.DirectionID == "" {
					t.Errorf("pattern has neither headsign nor direction id")
				}
				for _, s := range pat.Stops {
					checkStop(t, s)
					stops = append(stops, match.Stop{ID: s.ID, Name: s.Name})
				}
			}
		}
		cands := match.Find(c.Query, stops)
		if len(cands) != 1 {
			names := make([]string, 0, len(cands))
			for _, cd := range cands {
				names = append(names, cd.Name)
			}
			t.Fatalf("query %q matched %d stops on route %s: %v", c.Query, len(cands), c.Route, names)
		}
		checkName(t, cands[0].Name, c.WantStop)
		stopIDs = cands[0].IDs
	} else {
		found, err := ss.SearchStops(ctx, c.Query)
		if err != nil {
			t.Fatalf("SearchStops(%q): %v", c.Query, err)
		}
		if len(found) == 0 {
			t.Fatalf("SearchStops(%q) returned nothing", c.Query)
		}
		for _, s := range found {
			checkStop(t, s)
		}
		checkName(t, found[0].Name, c.WantStop)
		stopIDs = []string{found[0].ID}
		routes = []transit.Route{{ShortName: c.Route}}
	}

	deps, err := p.Departures(ctx, stopIDs, routes, 90*time.Minute)
	if err != nil {
		t.Fatalf("Departures(%v): %v", stopIDs, err)
	}
	if deps.Now.IsZero() {
		t.Errorf("Departures.Now is zero; return the server clock or Now()")
	}
	if len(deps.Departures) == 0 {
		t.Fatalf("Departures(%v) returned no entries", stopIDs)
	}
	if !sort.SliceIsSorted(deps.Departures, func(i, j int) bool { return deps.Departures[i].At.Before(deps.Departures[j].At) }) {
		t.Errorf("departures are not sorted by At")
	}
	now := c.Now
	if now.IsZero() {
		now = deps.Now
	}
	want := map[string]bool{}
	for _, l := range c.WantLines {
		want[match.Normalize(l)] = true
	}
	for i, d := range deps.Departures {
		if d.Line == "" {
			t.Errorf("departure %d has no Line", i)
		}
		if d.At.IsZero() && d.Scheduled.IsZero() {
			t.Errorf("departure %d has neither At nor Scheduled", i)
		}
		if !d.Live && !d.Scheduled.IsZero() && !d.At.IsZero() && !d.At.Equal(d.Scheduled) {
			t.Errorf("departure %d: not Live but At %v != Scheduled %v", i, d.At, d.Scheduled)
		}
		if !d.Cancelled && !d.At.IsZero() && d.At.Before(now.Add(-time.Minute)) {
			t.Errorf("departure %d: At %v is before now %v", i, d.At, now)
		}
		if len(want) > 0 && !want[match.Normalize(d.Line)] {
			t.Errorf("departure %d: line %q not in %v", i, d.Line, c.WantLines)
		}
	}

	if _, err := p.Departures(ctx, nil, routes, time.Minute); err == nil {
		t.Logf("note: Departures with no stop ids succeeded (returned no error)")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Departures(cctx, stopIDs, routes, time.Minute); err == nil {
		t.Errorf("Departures with a cancelled context should fail")
	}
}

// Skips unless every required key is set; keyless cities additionally need
// ETA_LIVE=1.
func Live(t *testing.T, p transit.Provider, c Case) {
	t.Helper()
	info := p.Info()
	needsKey := false
	for _, k := range info.Keys {
		if k.Req == transit.KeyRequired {
			needsKey = true
			if os.Getenv(k.Env) == "" {
				t.Skipf("set %s to run the live test", k.Env)
			}
		}
	}
	if !needsKey && os.Getenv("ETA_LIVE") == "" {
		t.Skip("set ETA_LIVE=1 to run the live test")
	}
	Conform(t, p, c)
}

type Server struct {
	*httptest.Server
	Auth func(r *http.Request) bool

	mu       sync.Mutex
	hits     map[string]int
	requests []*http.Request
}

// A key with a query string ("/locations?query=alex") matches when the path is
// equal and every listed parameter has that value, in any order; a key
// without one matches by path prefix. The most specific key wins. Files
// ending in ".graphql.json" are matched against the POST body instead: the
// key is a substring the query must contain.
func NewServer(t *testing.T, routes map[string]string) *Server {
	t.Helper()
	s := &Server{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.mu.Lock()
		s.requests = append(s.requests, r.Clone(r.Context()))
		s.mu.Unlock()
		if s.Auth != nil && !s.Auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		best, bestLen := "", -1
		for key, file := range routes {
			score := -1
			switch {
			case strings.HasSuffix(file, ".graphql.json"):
				if bytes.Contains(body, []byte(key)) {
					score = len(key)
				}
			case strings.Contains(key, "?"):
				score = queryScore(key, r.URL)
			case strings.HasPrefix(r.URL.Path, key):
				score = len(key)
			}
			if score > bestLen {
				best, bestLen = file, score
			}
		}
		if bestLen < 0 {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.hits[best]++
		s.mu.Unlock()
		b, err := os.ReadFile(filepath.Join("testdata", best))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func queryScore(key string, u *url.URL) int {
	path, rawq, _ := strings.Cut(key, "?")
	if u.Path != path {
		return -1
	}
	want, err := url.ParseQuery(rawq)
	if err != nil {
		return -1
	}
	got := u.Query()
	for k, vs := range want {
		if len(vs) == 0 || got.Get(k) != vs[0] {
			return -1
		}
	}
	return 1000*len(want) + len(path)
}

func (s *Server) Hits(file string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[file]
}

func (s *Server) Requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

func Serve(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	return NewServer(t, routes).Server
}

func checkStop(t *testing.T, s transit.Stop) {
	t.Helper()
	if s.ID == "" || s.Name == "" {
		t.Fatalf("stop %+v needs ID and Name", s)
	}
}

func checkName(t *testing.T, got, want string) {
	t.Helper()
	if want != "" && !strings.Contains(match.Normalize(got), match.Normalize(want)) {
		t.Fatalf("matched stop %q, want one containing %q", got, want)
	}
}

func isUnknown(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unknown route")
}
