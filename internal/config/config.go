// Package config reads eta's user configuration: the config file with the
// default city and aliases, and API keys from the environment or the keys
// file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bancsdan/eta/internal/transit"
)

type Config struct {
	DefaultCountry string
	DefaultTown    string
	Aliases        map[string]string
}

func Dir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join("~", ".config", "eta")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "eta")
}

func ConfigFile() string { return filepath.Join(Dir(), "config") }

func KeysFile() string { return filepath.Join(Dir(), "keys") }

// "default_country" and "default_town" are reserved; every other name is an
// alias whose value is re-parsed as command-line arguments.
func Load(path string) (*Config, error) {
	kv, err := parseKV(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Aliases: map[string]string{}}
	for k, v := range kv {
		switch k {
		case "default_country":
			c.DefaultCountry = strings.ToLower(v)
		case "default_town":
			c.DefaultTown = v
		default:
			c.Aliases[k] = v
		}
	}
	return c, nil
}

func Key(env string) string {
	if k := strings.TrimSpace(os.Getenv(env)); k != "" {
		return k
	}
	kv, err := parseKV(KeysFile())
	if err != nil {
		return ""
	}
	return kv[transit.KeySpec{Env: env}.ConfigName()]
}

func SetKey(name, value string) error { return setKV(KeysFile(), name, value) }

// SetAlias writes name = args into the config file; "default_city" is
// reserved and refused.
func SetAlias(name, args string) error {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "default_country", "default_town":
		return fmt.Errorf("%q is a setting, not an alias name", name)
	}
	return setKV(ConfigFile(), name, args)
}

// setKV replaces or appends "name = value" in path, keeping every other line
// (comments included). The file is created with owner-only permissions.
func setKV(path, name, value string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	value = strings.TrimSpace(value)
	if name == "" || value == "" || strings.ContainsAny(name, " \t=") {
		return errors.New("name and value must not be empty, and the name may not contain spaces or '='")
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %v", path, err)
	}
	replaced := false
	for i, line := range lines {
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.ToLower(strings.TrimSpace(k)) == name {
			lines[i] = name + " = " + value
			replaced = true
		}
	}
	if !replaced {
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		lines = append(lines, name+" = "+value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func MissingKeyError(info transit.Info, k transit.KeySpec) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s needs an API key", info.Name)
	if k.Label != "" {
		fmt.Fprintf(&sb, " (%s)", k.Label)
	}
	sb.WriteString(".")
	if k.SignupURL != "" {
		fmt.Fprintf(&sb, " Get a free key at %s, then", k.SignupURL)
	} else {
		sb.WriteString(" Then")
	}
	fmt.Fprintf(&sb, "\n  export %s=<key>\nor add a line to %s:\n  %s = <key>", k.Env, KeysFile(), k.ConfigName())
	return errors.New(sb.String())
}

type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func parseKV(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("config %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := strings.Cut(line, "=")
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		if !ok || name == "" || val == "" || strings.ContainsAny(name, " \t") {
			return nil, &UsageError{Msg: fmt.Sprintf("%s:%d: expected \"name = value\", got %q", path, n, line)}
		}
		out[strings.ToLower(name)] = val
	}
	return out, sc.Err()
}
