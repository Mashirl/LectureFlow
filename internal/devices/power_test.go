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

// pmset output → Power (macOS, mac-opti). The Mac mini lines are a real
// reading (Apple M6, macOS 27); the MacBook lines follow pmset's format
// (power source, charge, state, estimate, present).
func TestPowerFromPmset(t *testing.T) {
	const lowPowerOn = "Currently in use:\n standby              0\n lowpowermode         1\n womp                 1\n"
	const lowPowerOff = "Currently in use:\n lowpowermode         0\n"
	const (
		onAC      = "Now drawing from 'AC Power'\n"
		onBattery = "Now drawing from 'Battery Power'\n"
		onUPS     = "Now drawing from 'UPS Power'\n"
	)
	battery := func(rest string) string { return " -InternalBattery-0 (id=24117347)\t" + rest + "\n" }
	for _, c := range []struct {
		name, batt, settings string
		want                 Power
	}{
		{"Mac mini (real)", onAC, lowPowerOff, Power{Known: true}},
		{"Mac mini, Low Power Mode", onAC, lowPowerOn, Power{Known: true, Saver: true}},
		{"MacBook on battery", onBattery + battery("54%; discharging; 3:44 remaining present: true"),
			lowPowerOff, Power{Known: true, OnBattery: true, HasBattery: true, BatteryPct: pct(54)}},
		{"MacBook on battery, Low Power Mode", onBattery + battery("18%; discharging; (no estimate) present: true"),
			lowPowerOn, Power{Known: true, OnBattery: true, Saver: true, HasBattery: true, BatteryPct: pct(18)}},
		{"MacBook charging", onAC + battery("75%; charging; 0:45 remaining present: true"),
			lowPowerOff, Power{Known: true, HasBattery: true, Charging: true, BatteryPct: pct(75)}},
		{"MacBook finishing charge", onAC + battery("99%; finishing charge; 0:05 remaining present: true"),
			"", Power{Known: true, HasBattery: true, Charging: true, BatteryPct: pct(99)}},
		{"MacBook charged", onAC + battery("100%; charged; 0:00 remaining present: true"),
			"", Power{Known: true, HasBattery: true, BatteryPct: pct(100)}},
		{"MacBook held at 80% (optimised charging)", onAC + battery("80%; AC attached; not charging present: true"),
			"", Power{Known: true, HasBattery: true, BatteryPct: pct(80)}},
		{"battery removed", onAC + battery("0%; AC attached; not charging present: false"), "", Power{Known: true}},
		{"Mac mini on a UPS", onUPS + " -Back-UPS ES 700 (id=1234)\t96%; discharging; 0:21 remaining present: true\n", "", Power{Known: true}},
		{"Mac mini, UPS on standby", onAC + " -Back-UPS ES 700 (id=1234)\t100%; charged; 0:00 remaining present: true\n", "", Power{Known: true}},
		{"on battery without a battery line", onBattery, "", Power{Known: true, OnBattery: true, HasBattery: true}},
		{"charge out of range", onAC + battery("101%; charging; present: true"), "", Power{Known: true, HasBattery: true, Charging: true}},
		// A MacBook Air (Mac16,12, Apple M4, macOS 27.0.1), real readings
		// (2026-10-27; the battery id replaced): plugged in and full,
		// unplugged, unplugged with Low Power Mode.
		{"MacBook Air plugged in, full (real)", onAC + " -InternalBattery-0 (id=1000001)\t100%; charged; 0:00 remaining present: true\n",
			" lowpowermode         0\n", Power{Known: true, HasBattery: true, BatteryPct: pct(100)}},
		{"MacBook Air unplugged (real)", onBattery + " -InternalBattery-0 (id=1000001)\t100%; discharging; (no estimate) present: true\n",
			" lowpowermode         0\n", Power{Known: true, OnBattery: true, HasBattery: true, BatteryPct: pct(100)}},
		{"MacBook Air unplugged, Low Power Mode (real)", onBattery + " -InternalBattery-0 (id=1000001)\t100%; discharging; (no estimate) present: true\n",
			" lowpowermode         1\n", Power{Known: true, OnBattery: true, Saver: true, HasBattery: true, BatteryPct: pct(100)}},
		{"empty output (pmset failed)", "", lowPowerOn, Power{}},
		{"unrelated output", "pmset: unknown option\n", "", Power{}},
	} {
		if got := PowerFromPmset(c.batt, c.settings); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if p := PowerFromPmset(onBattery+battery("54%; discharging; 3:44 remaining present: true"), ""); !p.BatteryMachine() || !p.OnBatteryPower() {
		t.Errorf("a MacBook on battery is a battery machine on battery: %+v", p)
	}
	if p := PowerFromPmset(onUPS, ""); p.BatteryMachine() {
		t.Errorf("a desktop on a UPS is not a battery machine: %+v", p)
	}
}

// The real readings: on Windows the calls succeed (the values depend on the
// machine); on macOS pmset answers (the AC state is known) and the
// available unified memory is read through Mach; elsewhere both are
// unknown.
func TestReadPowerAndMemory(t *testing.T) {
	p := ReadPower()
	mb, err := AvailableMemoryMB()
	if runtime.GOOS == "darwin" {
		if !p.Known || err != nil || mb <= 0 {
			t.Fatalf("macOS: %+v %d %v", p, mb, err)
		}
		if again := ReadPower(); !reflect.DeepEqual(again, p) {
			t.Fatalf("macOS: a second reading within the cache interval differs: %+v %+v", p, again)
		}
		return
	}
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
