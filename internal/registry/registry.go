// Package registry maps countries and towns to provider constructors.
// Provider packages register their country and the towns they have verified
// probes for from init(); cmd/eta blank-imports internal/providers/all.
package registry

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/match"
	"github.com/bancsdan/eta/internal/transit"
)

type Deps struct {
	// Key returns "" when unset so the provider fails with transit.ErrNoKey on
	// its first network call rather than at construction.
	Key   func(env string) string
	HTTP  *http.Client
	Cache *cache.Cache // namespaced to provider and town
	Now   func() time.Time
}

func (d Deps) Clock() func() time.Time {
	if d.Now == nil {
		return time.Now
	}
	return d.Now
}

func (d Deps) KeyFor(env string) string {
	if d.Key == nil {
		return ""
	}
	return strings.TrimSpace(d.Key(env))
}

// Country is a covered country. AnyTown, when set, builds a provider for a
// town that has no registered entry (the provider scopes searches to it by
// locality or proximity); nil means only the registered towns work.
type Country struct {
	ID        string   // "norway"
	Name      string   // "Norway"
	Aliases   []string // "no", "norge"
	Providers []string // provider ids serving it
	Coverage  string   // overrides the listing's "every town" when AnyTown is regional ("Berlin and Brandenburg")
	AnyTown   func(d Deps, town string) transit.Provider
}

// Entry is one registered town with a verified probe.
type Entry struct {
	Info    transit.Info // Info.Country is the country id
	Aliases []string
	New     func(Deps) transit.Provider
}

var (
	countries    = map[string]*Country{}
	countryAlias = map[string]string{}
	towns        = map[string]Entry{} // "<country>/<town>"
	townAlias    = map[string]string{}
)

// RegisterCountry adds or extends a country; several providers may serve
// one country, but only one may offer AnyTown.
func RegisterCountry(c Country) {
	id := strings.ToLower(c.ID)
	if id == "" {
		panic("registry: country needs an id")
	}
	cur, ok := countries[id]
	if !ok {
		cur = &Country{ID: id, Name: c.Name}
		countries[id] = cur
	}
	if cur.Name == "" {
		cur.Name = c.Name
	}
	if c.Coverage != "" {
		cur.Coverage = c.Coverage
	}
	for _, p := range c.Providers {
		if !contains(cur.Providers, p) {
			cur.Providers = append(cur.Providers, p)
		}
	}
	sort.Strings(cur.Providers)
	if c.AnyTown != nil {
		if cur.AnyTown != nil {
			panic(fmt.Sprintf("registry: country %q already has an AnyTown provider", id))
		}
		cur.AnyTown = c.AnyTown
	}
	for _, a := range c.Aliases {
		a = strings.ToLower(a)
		if prev, dup := countryAlias[a]; dup && prev != id {
			panic(fmt.Sprintf("registry: country alias %q already used by %q", a, prev))
		}
		countryAlias[a] = id
	}
}

// Register adds a town; it panics on a duplicate id or alias within the
// country, or when the country is unknown.
func Register(e Entry) {
	country := strings.ToLower(e.Info.Country)
	id := strings.ToLower(e.Info.ID)
	if id == "" || e.New == nil || country == "" {
		panic("registry: entry needs an id, a country and a constructor")
	}
	if _, ok := countries[country]; !ok {
		panic(fmt.Sprintf("registry: town %q names unknown country %q", id, country))
	}
	key := country + "/" + id
	if _, dup := towns[key]; dup {
		panic(fmt.Sprintf("registry: duplicate town %q", key))
	}
	e.Info.ID, e.Info.Country = id, country
	towns[key] = e
	for _, a := range e.Aliases {
		ak := country + "/" + strings.ToLower(a)
		if _, dup := townAlias[ak]; dup {
			panic(fmt.Sprintf("registry: duplicate town alias %q", ak))
		}
		townAlias[ak] = key
	}
}

// LookupCountry resolves a country id or alias, case-insensitively.
func LookupCountry(name string) (*Country, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if id, ok := countryAlias[n]; ok {
		n = id
	}
	c, ok := countries[n]
	return c, ok
}

// LookupTown finds a registered town (id or alias) in a country.
func LookupTown(country, town string) (Entry, bool) {
	c, ok := LookupCountry(country)
	if !ok {
		return Entry{}, false
	}
	t := match.Normalize(town)
	t = strings.ReplaceAll(t, " ", "")
	key := c.ID + "/" + t
	if k, ok := townAlias[key]; ok {
		key = k
	}
	e, ok := towns[key]
	return e, ok
}

// Resolve returns a constructor for country/town: the registered entry when
// there is one, else the country's AnyTown provider. The returned Info is
// what the CLI should display.
func Resolve(country, town string) (transit.Info, func(Deps) transit.Provider, error) {
	c, ok := LookupCountry(country)
	if !ok {
		return transit.Info{}, nil, fmt.Errorf("unknown country %q", country)
	}
	if e, ok := LookupTown(country, town); ok {
		return e.Info, e.New, nil
	}
	if c.AnyTown == nil {
		names := make([]string, 0)
		for _, e := range Towns(c.ID) {
			names = append(names, e.Info.ID)
		}
		return transit.Info{}, nil, fmt.Errorf("%s has no data for %q; covered: %s", c.Name, town, strings.Join(names, ", "))
	}
	t := strings.TrimSpace(town)
	// The provider's own Info carries the key requirements, zone and
	// notes; only the identity is the registry's.
	info := c.AnyTown(Deps{}, t).Info()
	info.ID = strings.ReplaceAll(match.Normalize(t), " ", "-")
	info.Name = t
	info.Country = c.ID
	return info, func(d Deps) transit.Provider { return c.AnyTown(d, t) }, nil
}

// Countries returns every country sorted by id.
func Countries() []*Country {
	out := make([]*Country, 0, len(countries))
	for _, c := range countries {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Towns returns a country's registered towns sorted by id; "" means all.
func Towns(country string) []Entry {
	out := make([]Entry, 0, len(towns))
	for _, e := range towns {
		if country == "" || e.Info.Country == country {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Info.Country != out[j].Info.Country {
			return out[i].Info.Country < out[j].Info.Country
		}
		return out[i].Info.ID < out[j].Info.ID
	})
	return out
}

// All returns every registered town; kept for tests.
func All() []Entry { return Towns("") }

// Lookup finds a town by id or alias in any country; the first country in
// id order wins. Kept for tests that predate countries.
func Lookup(town string) (Entry, bool) {
	for _, c := range Countries() {
		if e, ok := LookupTown(c.ID, town); ok {
			return e, true
		}
	}
	return Entry{}, false
}

// Reset clears everything; for tests only.
func Reset() {
	countries = map[string]*Country{}
	countryAlias = map[string]string{}
	towns = map[string]Entry{}
	townAlias = map[string]string{}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
