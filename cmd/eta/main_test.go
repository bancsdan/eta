package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bancsdan/eta/internal/app"
	"github.com/bancsdan/eta/internal/config"
	"github.com/bancsdan/eta/internal/registry"
	"github.com/bancsdan/eta/internal/transit"
)

type stub struct{}

func (stub) Info() transit.Info {
	return transit.Info{ID: "stubcity", Name: "Stub", Provider: "stub", TZ: "UTC"}
}
func (stub) Departures(context.Context, []string, []transit.Route, time.Duration) (*transit.Departures, error) {
	return &transit.Departures{}, nil
}

func init() {
	registry.Register(registry.Entry{
		Info:    stub{}.Info(),
		Aliases: []string{"sc"},
		New:     func(registry.Deps) transit.Provider { return stub{} },
	})
}

func TestParseArgs(t *testing.T) {
	cfg := &config.Config{Aliases: map[string]string{"home": "stubcity 155 viranyos -c 3", "default": "home", "noCity": "4 moricz"}}
	cases := []struct {
		args    []string
		cfg     *config.Config
		want    app.Options
		wantErr string
	}{
		{[]string{"stubcity", "155", "viranyos"}, cfg, app.Options{City: "stubcity", Route: "155", Query: "viranyos", Count: 1}, ""},
		{[]string{"sc", "155", "Virányos", "út", "-c", "3", "-t"}, cfg, app.Options{City: "sc", Route: "155", Query: "Virányos út", Count: 3, ShowClock: true}, ""},
		{[]string{"-j", "stubcity", "155", "x", "-c=2"}, cfg, app.Options{City: "stubcity", Route: "155", Query: "x", Count: 2, JSON: true}, ""},
		{[]string{"stubcity", "155", "-l"}, cfg, app.Options{City: "stubcity", Route: "155", Count: 1, List: true}, ""},
		{[]string{"home"}, cfg, app.Options{City: "stubcity", Route: "155", Query: "viranyos", Count: 3}, ""},
		{[]string{"home", "-c", "1"}, cfg, app.Options{City: "stubcity", Route: "155", Query: "viranyos", Count: 1}, ""},
		{[]string{}, cfg, app.Options{City: "stubcity", Route: "155", Query: "viranyos", Count: 3}, ""},
		{[]string{"4", "moricz"}, &config.Config{DefaultCity: "stubcity"}, app.Options{City: "stubcity", Route: "4", Query: "moricz", Count: 1}, ""},
		{[]string{"noCity"}, cfg, app.Options{}, "unknown city \"4\""},
		{[]string{"4", "moricz"}, &config.Config{}, app.Options{}, "unknown city \"4\""},
		{[]string{"stubcity", "155"}, cfg, app.Options{City: "stubcity", Query: "155", Count: 1}, ""},
		{[]string{"stubcity", "-w", "--every", "10", "alex"}, cfg, app.Options{City: "stubcity", Query: "alex", Count: 1}, ""},
		{[]string{"stubcity"}, cfg, app.Options{}, "usage:"},
		{[]string{"stubcity", "x", "--every", "1"}, cfg, app.Options{}, "minimum 5"},
		{[]string{"stubcity", "155", "x", "-c", "0"}, cfg, app.Options{}, "not a positive number"},
		{[]string{"stubcity", "155", "x", "--bogus"}, cfg, app.Options{}, "unknown flag"},
	}
	for _, c := range cases {
		got, err := parseArgs(c.args, c.cfg)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if got.Options != c.want {
			t.Errorf("%v: got %+v, want %+v", c.args, got.Options, c.want)
		}
	}
}

func TestParseWatchAndSetup(t *testing.T) {
	cfg := &config.Config{}
	got, err := parseArgs([]string{"stubcity", "alex", "-w", "--every=45"}, cfg)
	if err != nil || !got.watch || got.every != 45*time.Second {
		t.Errorf("watch: %+v %v", got, err)
	}
	got, err = parseArgs([]string{"stubcity", "--setup"}, cfg)
	if err != nil || !got.setup || got.City != "stubcity" {
		t.Errorf("setup: %+v %v", got, err)
	}
	if _, err = parseArgs([]string{"--setup"}, cfg); err == nil {
		t.Errorf("setup without city should fail")
	}
}

func TestRunSetupWritesKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ETA_STUB_API_KEY", "")
	info := transit.Info{Name: "Stub", Keys: []transit.KeySpec{{Env: "ETA_STUB_API_KEY", Req: transit.KeyRequired, SignupURL: "https://x"}}}
	var out strings.Builder
	if err := runSetup(info, strings.NewReader("  s3cret \n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := config.Key("ETA_STUB_API_KEY"); got != "s3cret" {
		t.Errorf("key = %q; output:\n%s", got, out.String())
	}
	if err := config.SetKey("other", "o"); err != nil {
		t.Fatal(err)
	}
	if err := runSetup(info, strings.NewReader("\n"), &out); err != nil {
		t.Fatal(err)
	}
	if config.Key("ETA_STUB_API_KEY") != "s3cret" || config.Key("ETA_OTHER_API_KEY") != "o" {
		t.Errorf("keys not preserved")
	}
	if err := runSetup(info, strings.NewReader("new\n"), &out); err != nil {
		t.Fatal(err)
	}
	if config.Key("ETA_STUB_API_KEY") != "new" || config.Key("ETA_OTHER_API_KEY") != "o" {
		t.Errorf("replace failed")
	}
}

func TestCitiesTable(t *testing.T) {
	var sb strings.Builder
	if err := runCities(nil, &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "stubcity (sc)") || !strings.Contains(out, "ready") || !strings.Contains(out, "CITY") {
		t.Errorf("output:\n%s", out)
	}
}

func TestConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("# comment\ndefault_city = Berlin\nhome = M4 alex -c 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultCity != "berlin" || cfg.Aliases["home"] != "M4 alex -c 2" || len(cfg.Aliases) != 1 {
		t.Errorf("cfg = %+v", cfg)
	}
	if err := os.WriteFile(path, []byte("bad line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "config:1") {
		t.Errorf("err = %v", err)
	}
	if cfg, err := config.Load(filepath.Join(dir, "missing")); err != nil || cfg.DefaultCity != "" {
		t.Errorf("missing file: %v %+v", err, cfg)
	}
}

func TestKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ETA_TFL_API_KEY", "")
	if err := os.MkdirAll(filepath.Join(dir, "eta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.KeysFile(), []byte("tfl = abc\ncta_train = t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := config.Key("ETA_TFL_API_KEY"); got != "abc" {
		t.Errorf("tfl = %q", got)
	}
	if got := config.Key("ETA_CTA_TRAIN_API_KEY"); got != "t1" {
		t.Errorf("cta_train = %q", got)
	}
	t.Setenv("ETA_TFL_API_KEY", "env")
	if got := config.Key("ETA_TFL_API_KEY"); got != "env" {
		t.Errorf("env should win, got %q", got)
	}
	if got := config.Key("ETA_NOPE_API_KEY"); got != "" {
		t.Errorf("unset = %q", got)
	}
}
