package registry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/transit"
)

type p struct{ id, town string }

func (x p) Info() transit.Info { return transit.Info{ID: x.id, Provider: "prov", Country: "zed"} }
func (p) Departures(context.Context, []string, []transit.Route, time.Duration) (*transit.Departures, error) {
	return nil, nil
}

func TestCountriesAndTowns(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	RegisterCountry(Country{ID: "Zed", Name: "Zedland", Aliases: []string{"Z"}, Providers: []string{"prov"},
		AnyTown: func(_ Deps, town string) transit.Provider { return p{"any", town} }})
	RegisterCountry(Country{ID: "zed", Providers: []string{"other"}}) // second provider, merged
	RegisterCountry(Country{ID: "abc", Name: "Abc"})
	Register(Entry{Info: transit.Info{ID: "Cap", Country: "zed", Provider: "prov"}, Aliases: []string{"C"}, New: func(Deps) transit.Provider { return p{"cap", ""} }})
	Register(Entry{Info: transit.Info{ID: "town", Country: "abc", Provider: "x"}, New: func(Deps) transit.Provider { return p{"t", ""} }})

	c, ok := LookupCountry(" z ")
	if !ok || c.ID != "zed" || c.Name != "Zedland" || strings.Join(c.Providers, ",") != "other,prov" {
		t.Errorf("country: %v %+v", ok, c)
	}
	if e, ok := LookupTown("Z", "c"); !ok || e.Info.ID != "cap" {
		t.Errorf("town alias: %v %+v", ok, e.Info)
	}
	info, newP, err := Resolve("zed", "Elsewhere")
	if err != nil || info.ID != "elsewhere" || info.Provider != "prov" || newP(Deps{}).(p).town != "Elsewhere" {
		t.Errorf("any town: %+v %v", info, err)
	}
	if _, _, err := Resolve("abc", "nowhere"); err == nil || !strings.Contains(err.Error(), "covered: town") {
		t.Errorf("no AnyTown should list the covered towns, got %v", err)
	}
	if _, _, err := Resolve("nope", "x"); err == nil {
		t.Errorf("unknown country should fail")
	}
	if all := Towns(""); len(all) != 2 || all[0].Info.Country != "abc" {
		t.Errorf("Towns = %+v", all)
	}
	if e, ok := Lookup("cap"); !ok || e.Info.Country != "zed" {
		t.Errorf("global Lookup: %v %+v", ok, e.Info)
	}
	for _, bad := range []func(){
		func() {
			Register(Entry{Info: transit.Info{ID: "cap", Country: "zed"}, New: func(Deps) transit.Provider { return nil }})
		},
		func() {
			Register(Entry{Info: transit.Info{ID: "x", Country: "mars"}, New: func(Deps) transit.Provider { return nil }})
		},
		func() {
			RegisterCountry(Country{ID: "zed", AnyTown: func(Deps, string) transit.Provider { return nil }})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("should panic")
				}
			}()
			bad()
		}()
	}
}
