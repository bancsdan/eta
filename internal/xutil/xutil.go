// Package xutil holds the small helpers every provider ends up needing:
// nil-safe pointer reads, tolerant time parsing and slice utilities.
package xutil

import (
	"strings"
	"time"
)

func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func Dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func ParseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func FirstTime(cands ...*string) (time.Time, bool) {
	for _, c := range cands {
		if t := ParseRFC3339(Str(c)); !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}

func EpochTime(v int64) time.Time {
	switch {
	case v == 0:
		return time.Time{}
	case v > 1e12:
		return time.UnixMilli(v)
	default:
		return time.Unix(v, 0)
	}
}

func WindowMinutes(window time.Duration) int {
	return max(1, int(window/time.Minute))
}
