// Package registry maps city ids to provider constructors. City packages
// call Register from init(); cmd/eta blank-imports internal/providers/all.
package registry

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bancsdan/eta/internal/cache"
	"github.com/bancsdan/eta/internal/transit"
)

type Deps struct {
	// Key returns "" when unset so the provider fails with transit.ErrNoKey on
	// its first network call rather than at construction.
	Key   func(env string) string
	HTTP  *http.Client
	Cache *cache.Cache // already namespaced to the city
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

type Entry struct {
	Info    transit.Info
	Aliases []string
	New     func(Deps) transit.Provider
}

var (
	byID    = map[string]Entry{}
	byAlias = map[string]string{}
)

// Duplicate ids and aliases panic so the clash shows up in tests.
func Register(e Entry) {
	id := strings.ToLower(e.Info.ID)
	if id == "" || e.New == nil {
		panic("registry: entry needs an id and a constructor")
	}
	if _, dup := byID[id]; dup {
		panic(fmt.Sprintf("registry: duplicate city %q", id))
	}
	if _, dup := byAlias[id]; dup {
		panic(fmt.Sprintf("registry: city %q clashes with an alias", id))
	}
	e.Info.ID = id
	byID[id] = e
	for _, a := range e.Aliases {
		a = strings.ToLower(a)
		if _, dup := byAlias[a]; dup {
			panic(fmt.Sprintf("registry: duplicate alias %q", a))
		}
		if _, dup := byID[a]; dup {
			panic(fmt.Sprintf("registry: alias %q clashes with a city", a))
		}
		byAlias[a] = id
	}
}

func Lookup(id string) (Entry, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if canon, ok := byAlias[id]; ok {
		id = canon
	}
	e, ok := byID[id]
	return e, ok
}

func All() []Entry {
	out := make([]Entry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.ID < out[j].Info.ID })
	return out
}

func Reset() {
	byID = map[string]Entry{}
	byAlias = map[string]string{}
}
