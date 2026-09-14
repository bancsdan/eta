package registry

import (
	"context"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/transit"
)

type p struct{ id string }

func (x p) Info() transit.Info { return transit.Info{ID: x.id} }
func (p) Departures(context.Context, []string, []transit.Route, time.Duration) (*transit.Departures, error) {
	return nil, nil
}

func TestRegisterLookupAll(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register(Entry{Info: transit.Info{ID: "Zed"}, Aliases: []string{"Z"}, New: func(Deps) transit.Provider { return p{"zed"} }})
	Register(Entry{Info: transit.Info{ID: "abc"}, New: func(Deps) transit.Provider { return p{"abc"} }})
	if e, ok := Lookup(" z "); !ok || e.Info.ID != "zed" {
		t.Errorf("alias lookup: %v %+v", ok, e)
	}
	if _, ok := Lookup("nope"); ok {
		t.Errorf("unknown city found")
	}
	if all := All(); len(all) != 2 || all[0].Info.ID != "abc" {
		t.Errorf("All = %+v", all)
	}
	for _, dup := range []Entry{
		{Info: transit.Info{ID: "abc"}, New: func(Deps) transit.Provider { return nil }},
		{Info: transit.Info{ID: "z"}, New: func(Deps) transit.Provider { return nil }},
		{Info: transit.Info{ID: "new"}, Aliases: []string{"abc"}, New: func(Deps) transit.Provider { return nil }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) should panic", dup.Info.ID)
				}
			}()
			Register(dup)
		}()
	}
}
