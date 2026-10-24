// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// golden compares got with testdata/{name}, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/tariff -update to create it)", err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		gl, wl := strings.Split(got, "\n"), strings.Split(w, "\n")
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("%s differs from golden file at line %d:\n got: %s\nwant: %s", name, i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("%s differs from golden file: %d lines, want %d", name, len(gl), len(wl))
	}
}

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

var (
	shanghai  = mustZone("Asia/Shanghai")
	london    = mustZone("Europe/London")
	havana    = mustZone("America/Havana")
	utcMinus2 = time.FixedZone("UTC-2", -2*3600)
)

// at parses "2006-01-02 15:04[:05[.999999999]]" in loc.
func at(loc *time.Location, s string) time.Time {
	layout := "2006-01-02 15:04"
	if len(s) > len(layout) {
		layout = "2006-01-02 15:04:05"
	}
	t, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func holidays(t *testing.T, raw string) HolidaySet {
	t.Helper()
	h, err := ParseHolidays([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// refPeak is an independent, brute-force reading of P12.3 for one instant:
// the tariff's wall clock (loc = cfg.Timezone) as "HH:MM" strings, which
// compare like times.
func refPeak(x time.Time, loc *time.Location, cfg Tariff, hol HolidaySet) bool {
	w := x.In(loc)
	if cfg.ExcludeHolidays != "" && hol.IsHoliday(w.Format("2006-01-02")) {
		return false
	}
	iso := (int(w.Weekday())+6)%7 + 1
	hm := w.Format("15:04")
	for _, win := range cfg.PeakWindows {
		for _, d := range win.Days {
			if d == iso && hm >= win.From && hm < win.To {
				return true
			}
		}
	}
	return false
}
