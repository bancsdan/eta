package xutil

import (
	"testing"
	"time"
)

func TestHelpers(t *testing.T) {
	s := "x"
	if Str(nil) != "" || Str(&s) != "x" {
		t.Error("Str")
	}
	if got := Dedupe([]string{" a ", "", "b", "a"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Dedupe = %v", got)
	}
	if !ParseRFC3339("").IsZero() || !ParseRFC3339("nope").IsZero() || ParseRFC3339("2026-09-13T18:00:00+02:00").IsZero() {
		t.Error("ParseRFC3339")
	}
	bad, good := "bad", "2026-09-13T18:00:00Z"
	if got, ok := FirstTime(nil, &bad, &good); !ok || got.Hour() != 18 {
		t.Errorf("FirstTime = %v %v", got, ok)
	}
	if _, ok := FirstTime(nil, &bad); ok {
		t.Error("FirstTime should fail")
	}
	if !EpochTime(0).IsZero() || EpochTime(1_700_000_000).Year() != 2023 || EpochTime(1_700_000_000_000).Year() != 2023 {
		t.Error("EpochTime")
	}
	if WindowMinutes(0) != 1 || WindowMinutes(90*time.Minute) != 90 || WindowMinutes(30*time.Second) != 1 {
		t.Error("WindowMinutes")
	}
}
