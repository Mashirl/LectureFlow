// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The development power simulation (power-brief §3.1): the variable unset
// is the real reading; set, the file is read on every call, so rewriting it
// plugs / unplugs at once; a missing or invalid file falls back to the
// real reading.
func TestNewPowerReader(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == DevPowerEnv {
				return v
			}
			return ""
		}
	}
	if got, want := NewPowerReader(env(""))(), ReadPower(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unset: %+v, want the real reading %+v", got, want)
	}
	if got, want := NewPowerReader(env("   "))(), ReadPower(); !reflect.DeepEqual(got, want) {
		t.Fatalf("blank: %+v", got)
	}
	if got, want := NewPowerReader(nil)(), ReadPower(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil getenv: %+v", got)
	}

	file := filepath.Join(t.TempDir(), "power.json")
	read := NewPowerReader(env(file))
	// Missing file: the real reading.
	if got, want := read(), ReadPower(); !reflect.DeepEqual(got, want) {
		t.Fatalf("missing file: %+v", got)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"on_battery": true, "battery_pct": 55}`)
	if got := read(); !reflect.DeepEqual(got, Power{Known: true, OnBattery: true, HasBattery: true, BatteryPct: pct(55)}) {
		t.Fatalf("unplugged: %+v", got)
	}
	if !read().OnBatteryPower() {
		t.Fatal("not on battery power")
	}
	// Rewriting the file plugs the machine in on the next call.
	write(`{"on_battery": false, "charging": true, "battery_pct": 56}`)
	if got := read(); !reflect.DeepEqual(got, Power{Known: true, HasBattery: true, Charging: true, BatteryPct: pct(56)}) {
		t.Fatalf("plugged in: %+v", got)
	}
	// A desktop.
	write(`{"has_battery": false, "on_battery": true}`)
	if got := read(); !reflect.DeepEqual(got, Power{Known: true}) || got.BatteryMachine() {
		t.Fatalf("no battery: %+v", got)
	}
	// Invalid JSON, an unknown field (a typo), a second value: the real reading.
	for _, bad := range []string{`{"on_battery": tru`, `{"onbattery": true}`, `{} {}`} {
		write(bad)
		if got, want := read(), ReadPower(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v", bad, got)
		}
	}
	write(`{"on_battery": true}`)
	if got := read(); !got.OnBatteryPower() || got.BatteryPct != nil {
		t.Fatalf("back to a valid file: %+v", got)
	}
}
