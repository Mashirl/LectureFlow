// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"reflect"
	"runtime"
	"testing"
)

func pct(v int) *int { return &v }

// SYSTEM_POWER_STATUS → Power (lead ruling 5: unknown counts as AC power,
// which the scheduler reads from Known = false).
func TestPowerFromStatus(t *testing.T) {
	for _, c := range []struct {
		name                      string
		ac, flag, life, sysStatus byte
		want                      Power
	}{
		{"laptop on AC, charging", 1, 8, 80, 0, Power{Known: true, BatteryPct: pct(80), HasBattery: true, Charging: true}},
		{"laptop on AC, high and charging", 1, 1 | 8, 97, 0, Power{Known: true, BatteryPct: pct(97), HasBattery: true, Charging: true}},
		{"laptop on battery", 0, 1, 64, 0, Power{Known: true, OnBattery: true, BatteryPct: pct(64), HasBattery: true}},
		{"on battery, saver on", 0, 2, 15, 1, Power{Known: true, OnBattery: true, Saver: true, BatteryPct: pct(15), HasBattery: true}},
		{"low and critical flags", 0, 2 | 4, 3, 0, Power{Known: true, OnBattery: true, BatteryPct: pct(3), HasBattery: true}},
		{"AC status unknown", 255, 1, 50, 0, Power{BatteryPct: pct(50), HasBattery: true}},
		{"charge unknown", 0, 1, 255, 0, Power{Known: true, OnBattery: true, HasBattery: true}},
		{"charge out of range", 1, 0, 101, 0, Power{Known: true, HasBattery: true}},
		{"desktop without battery", 1, 128, 255, 0, Power{Known: true}},
		{"no battery but offline (bogus)", 0, 128, 255, 0, Power{Known: true}},
		{"battery flag unknown", 1, 255, 255, 0, Power{Known: true}},
		{"battery flag unknown, charge known", 0, 255, 40, 0, Power{Known: true, OnBattery: true, BatteryPct: pct(40), HasBattery: true}},
		{"full", 1, 1, 100, 0, Power{Known: true, BatteryPct: pct(100), HasBattery: true}},
		{"empty", 0, 4, 0, 0, Power{Known: true, OnBattery: true, BatteryPct: pct(0), HasBattery: true}},
		{"other status bits", 1, 0, 90, 0x80, Power{Known: true, BatteryPct: pct(90), HasBattery: true}},
		{"all unknown", 255, 255, 255, 0, Power{}},
	} {
		if got := PowerFromStatus(c.ac, c.flag, c.life, c.sysStatus); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The policy's two predicates (power management): a machine with a battery
// needs a known AC state; unknown is a machine without one (3b behaviour).
func TestPowerBatteryMachine(t *testing.T) {
	for _, c := range []struct {
		name           string
		p              Power
		machine, onBat bool
	}{
		{"desktop", PowerFromStatus(1, 128, 255, 0), false, false},
		{"laptop on AC", PowerFromStatus(1, 8, 80, 0), true, false},
		{"laptop on battery", PowerFromStatus(0, 1, 64, 0), true, true},
		{"AC state unknown", PowerFromStatus(255, 1, 50, 0), false, false},
		{"not Windows / failed call", Power{}, false, false},
		{"flag unknown on battery", PowerFromStatus(0, 255, 40, 0), true, true},
	} {
		if got := c.p.BatteryMachine(); got != c.machine {
			t.Errorf("%s: BatteryMachine %v", c.name, got)
		}
		if got := c.p.OnBatteryPower(); got != c.onBat {
			t.Errorf("%s: OnBatteryPower %v", c.name, got)
		}
	}
}

// The real readings: on Windows the calls succeed (the values depend on the
// machine); elsewhere they are unknown.
func TestReadPowerAndMemory(t *testing.T) {
	p := ReadPower()
	mb, err := AvailableMemoryMB()
	if runtime.GOOS != "windows" {
		if p.Known || err == nil {
			t.Fatalf("not Windows: %+v %d %v", p, mb, err)
		}
		return
	}
	if err != nil || mb <= 0 {
		t.Fatalf("available memory %d %v", mb, err)
	}
	if p.BatteryPct != nil && (*p.BatteryPct < 0 || *p.BatteryPct > 100) {
		t.Fatalf("power %+v", p)
	}
}
