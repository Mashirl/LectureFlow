// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// Power and memory readings (SPEC-v2 I8; lead rulings 5 and 8). FROZEN
// signatures; bodies: task B (Windows: kernel32 GetSystemPowerStatus and
// GlobalMemoryStatusEx through syscall, the way internal/api's
// diskfree_windows.go calls kernel32; split into power_windows.go /
// power_other.go as needed).

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Power is the power state the scheduler reads on every scan.
type Power struct {
	// Known is false when the system does not tell (ACLineStatus 255, pmset
	// failed, or neither Windows nor macOS): the scheduler then treats the
	// machine as on AC power (lead ruling 5).
	Known     bool `json:"known"`
	OnBattery bool `json:"on_battery"` // ACLineStatus 0 (only meaningful with Known)
	Saver     bool `json:"saver"`      // battery saver on (SystemStatusFlag 1); display only
	// BatteryPct is the remaining charge (BatteryLifePercent; nil = 255
	// unknown or no battery, BatteryFlag 128).
	BatteryPct *int `json:"battery_pct"`
	// HasBattery (power management, v2-step20 freeze): the machine has a
	// system battery (a laptop or a Windows tablet): BatteryFlag is known
	// and without bit 128; with BatteryFlag 255 (unknown) only when it runs
	// on battery right now (ACLineStatus 0). With Known, it selects the
	// power-aware policy of a machine with a battery (BatteryMachine); a
	// machine without one keeps the 3b behaviour (devices.Place).
	HasBattery bool `json:"has_battery"`
	// Charging: BatteryFlag bit 8 ("Charging"), only with a known flag and
	// a battery. Display only (the resource bar's "正在充电").
	Charging bool `json:"charging"`
}

// BatteryMachine reports whether the power-aware policy of a machine with a
// battery applies: the AC state is known and there is a battery. An unknown
// state is treated as a machine without a battery (3b behaviour, on AC
// power; power-plan §2).
func (p Power) BatteryMachine() bool { return p.Known && p.HasBattery }

// OnBatteryPower reports whether a machine with a battery runs on battery now
// (the policy's battery rules apply).
func (p Power) OnBatteryPower() bool { return p.BatteryMachine() && p.OnBattery }

// ReadPower reads the power state now (cheap; called on every scan).
// Windows: GetSystemPowerStatus; macOS: pmset, cached for a few seconds.
// Elsewhere, or the call fails: Power{} (unknown, treated as AC power).
func ReadPower() Power { return readPower() }

// SYSTEM_POWER_STATUS values PowerFromStatus understands.
const (
	acLineOffline       = 0
	acLineOnline        = 1
	batteryFlagNone     = 128 // "No system battery"
	batteryFlagUnknown  = 255 // "Unknown status—unable to read the battery flag information"
	batteryLifeUnknown  = 255
	systemStatusSaverOn = 1
	batteryFlagCharging = 8 // "Charging"
)

// PowerFromStatus maps the fields of SYSTEM_POWER_STATUS to a Power (pure,
// for the tests of task B): acLine 0 offline, 1 online, 255 unknown;
// batteryFlag 128 = no system battery; lifePercent 255 = unknown;
// systemStatus bit 0 = battery saver.
//
// A machine without a system battery is never on battery (whatever
// ACLineStatus says) and has no charge; a charge above 100 is treated as
// unknown. HasBattery: a known flag without bit 128, or (flag unknown) the
// machine runs on battery; Charging: a known flag with bit 8.
func PowerFromStatus(acLine, batteryFlag, lifePercent, systemStatus byte) Power {
	p := Power{Saver: systemStatus&systemStatusSaverOn != 0}
	switch acLine {
	case acLineOffline:
		p.Known, p.OnBattery = true, true
	case acLineOnline:
		p.Known = true
	}
	noBattery := batteryFlag != batteryFlagUnknown && batteryFlag&batteryFlagNone != 0
	if noBattery {
		p.OnBattery = false
		return p
	}
	if batteryFlag != batteryFlagUnknown {
		p.HasBattery = true
		p.Charging = batteryFlag&batteryFlagCharging != 0
	} else {
		p.HasBattery = p.OnBattery
	}
	if lifePercent != batteryLifeUnknown && lifePercent <= 100 {
		v := int(lifePercent)
		p.BatteryPct = &v
	}
	return p
}

// macOS (mac-opti): the power state comes from pmset(1), whose output is
// not localised. `pmset -g batt` names the source the machine draws from
// and lists the power sources, e.g. on a MacBook running on battery:
//
//	Now drawing from 'Battery Power'
//	 -InternalBattery-0 (id=24117347)	54%; discharging; 3:44 remaining present: true
//
// and on a Mac mini only "Now drawing from 'AC Power'". Only an
// InternalBattery line is a system battery: a UPS ('UPS Power', a line
// naming the UPS) does not make a desktop a machine with a battery, as on
// Windows. IOKit's AppleSmartBattery cannot be used for this: a Mac mini
// lists one too, with every value 0. `pmset -g` (the settings in use)
// carries "lowpowermode 1" when Low Power Mode is on (Saver).
var (
	pmsetSourceRe  = regexp.MustCompile(`Now drawing from '([^']+)'`)
	pmsetBatteryRe = regexp.MustCompile(`(?m)^\s*-InternalBattery-\d+.*$`)
	pmsetPctRe     = regexp.MustCompile(`\t(\d{1,3})%;\s*([^;]*)`)
	pmsetLowPower  = regexp.MustCompile(`(?m)^\s*lowpowermode\s+1\s*$`)
)

// PowerFromPmset maps the output of `pmset -g batt` and `pmset -g` to a Power
// (pure, for the tests). No source line: unknown (Power{}), treated as AC
// power. A machine without an internal battery is never on battery and has
// no charge (as PowerFromStatus); drawing from 'Battery Power' without a
// battery line counts as a battery (as an unknown BatteryFlag on battery).
func PowerFromPmset(batt, settings string) Power {
	m := pmsetSourceRe.FindStringSubmatch(batt)
	if m == nil {
		return Power{}
	}
	p := Power{Known: true, OnBattery: m[1] == "Battery Power", Saver: pmsetLowPower.MatchString(settings)}
	line := pmsetBatteryRe.FindString(batt)
	if line == "" || strings.Contains(line, "present: false") {
		p.HasBattery = p.OnBattery && line == ""
		if !p.HasBattery {
			p.OnBattery = false
		}
		return p
	}
	p.HasBattery = true
	if pm := pmsetPctRe.FindStringSubmatch(line); pm != nil {
		if v, err := strconv.Atoi(pm[1]); err == nil && v <= 100 {
			p.BatteryPct = &v
		}
		// "charging" and "finishing charge"; not "discharging", "charged",
		// "AC attached; not charging".
		state := strings.TrimSpace(pm[2])
		p.Charging = state == "charging" || state == "finishing charge"
	}
	return p
}

// AvailableMemoryMB is the available physical memory (ullAvailPhys) in MB.
// Not Windows: an error (unknown; the scheduler then admits by the
// estimate alone, task C).
func AvailableMemoryMB() (int, error) { return availableMemoryMB() }

// ---------- development power simulation (power management, v2-step20) ----------

// DevPowerEnv is the environment variable of the development power
// simulation (power-plan §6, the real verification "模拟拔电"): when it
// names a file, the power state is read from that file instead of the
// system, on every call, so rewriting the file plugs or unplugs the
// machine for the scheduler within one scan. The product never sets it;
// like the mock and ?dev=1 (en-XA) it exists for development only — the
// server has no other development switch, so the variable itself is the
// switch (NOTES "电源管理冻结"). The mock server simulates power by its
// scenarios (?mock=…) and does not read it.
const DevPowerEnv = "LECTUREFLOW_DEV_POWER"

// DevPower is the JSON of the DevPowerEnv file, e.g.
// {"on_battery": true, "battery_pct": 55}. The simulated machine always
// has a known AC state; HasBattery nil means true (a laptop); Charging is
// shown only.
type DevPower struct {
	OnBattery  bool  `json:"on_battery"`
	HasBattery *bool `json:"has_battery"`
	Charging   bool  `json:"charging"`
	BatteryPct *int  `json:"battery_pct"`
}

// Power is the simulated reading: Known, the fields of d; a machine without
// a battery is never on battery and has no charge (as PowerFromStatus).
func (d DevPower) Power() Power {
	p := Power{Known: true, HasBattery: d.HasBattery == nil || *d.HasBattery}
	if !p.HasBattery {
		return p
	}
	p.OnBattery, p.Charging = d.OnBattery, d.Charging
	if d.BatteryPct != nil && *d.BatteryPct >= 0 && *d.BatteryPct <= 100 {
		v := *d.BatteryPct
		p.BatteryPct = &v
	}
	return p
}

// NewPowerReader returns the power function cmd wires into the scheduler
// (sched.Options.Power): ReadPower, or — when getenv(DevPowerEnv) is not
// empty — a reader of that file (DevPower.Power) that falls back to
// ReadPower while the file is missing or invalid (logged once per distinct
// problem, never per call). The simulation applies in test mode too, so an
// end-to-end test can drive it; without it test mode keeps its fixed
// "unknown" reading (cmd, 3b C decision 11).
func NewPowerReader(getenv func(string) string) func() Power {
	path := ""
	if getenv != nil {
		path = strings.TrimSpace(getenv(DevPowerEnv))
	}
	if path == "" {
		return ReadPower
	}
	slog.Warn("development power simulation is on: the power state is read from a file", "env", DevPowerEnv, "file", path)
	var (
		mu   sync.Mutex
		last string // the last problem logged ("" = the file was fine)
	)
	report := func(problem string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if problem == last {
			return
		}
		last = problem
		if problem == "" {
			slog.Info("development power simulation: file read again", "file", path)
			return
		}
		slog.Warn("development power simulation: file unusable, using the real power state", "file", path, "problem", problem, "err", err)
	}
	return func() Power {
		raw, err := os.ReadFile(path)
		if err != nil {
			report("read", err)
			return ReadPower()
		}
		d, err := parseDevPower(raw)
		if err != nil {
			report("invalid", err)
			return ReadPower()
		}
		report("", nil)
		return d.Power()
	}
}

// parseDevPower decodes a DevPowerEnv file strictly (unknown fields are an
// error, so a typo does not silently simulate AC power).
func parseDevPower(raw []byte) (DevPower, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d DevPower
	if err := dec.Decode(&d); err != nil {
		return DevPower{}, err
	}
	if dec.More() {
		return DevPower{}, errors.New("more than one JSON value")
	}
	return d, nil
}
