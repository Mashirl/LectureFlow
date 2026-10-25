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
	"strconv"
	"strings"
	"sync"
)

// Power is the power state the scheduler reads on every scan.
type Power struct {
	// Known is false when the system does not tell (ACLineStatus 255, or
	// not Windows): the scheduler then treats the machine as on AC power
	// (lead ruling 5).
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
// Not Windows, or the call fails: Power{} (unknown, treated as AC power).
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

// PowerFromPmset maps the output of macOS `pmset -g batt` to a Power (pure,
// for testing on any platform).
//
// A desktop Mac (Mac mini, Mac Studio, Mac Pro, iMac) prints only the power
// source ("Now drawing from 'AC Power'"); a laptop includes an " -InternalBattery-..."
// line with the remaining percentage and charging / discharging status.
func PowerFromPmset(out string) Power {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return Power{}
	}
	var p Power
	switch {
	case strings.Contains(trimmed, "'Battery Power'"):
		p.Known, p.OnBattery = true, true
	case strings.Contains(trimmed, "'UPS Power'"):
		p.Known, p.OnBattery = true, true
	case strings.Contains(trimmed, "'AC Power'"):
		p.Known = true
	}
	hasBattery := strings.Contains(trimmed, "InternalBattery")
	if !p.Known && hasBattery {
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "discharging") {
			p.Known, p.OnBattery = true, true
		} else if strings.Contains(lower, "charging") || strings.Contains(lower, "charged") || strings.Contains(lower, "ac attached") {
			p.Known = true
		}
	}
	if !p.Known {
		return Power{}
	}
	if !hasBattery {
		p.OnBattery = false
		return p
	}
	p.HasBattery = true
	for _, line := range strings.Split(trimmed, "\n") {
		if !strings.Contains(line, "InternalBattery") {
			continue
		}
		lineLower := strings.ToLower(line)
		if strings.Contains(lineLower, "present: false") {
			p.HasBattery, p.OnBattery = false, false
			return p
		}
		if pctIdx := strings.Index(line, "%"); pctIdx > 0 {
			start := pctIdx - 1
			for start >= 0 && line[start] >= '0' && line[start] <= '9' {
				start--
			}
			if start < pctIdx-1 {
				if val, err := strconv.Atoi(line[start+1 : pctIdx]); err == nil && val >= 0 && val <= 100 {
					p.BatteryPct = &val
				}
			}
		}
		if !p.OnBattery {
			p.Charging = (strings.Contains(lineLower, "charging") || strings.Contains(lineLower, "finishing charge")) &&
				!strings.Contains(lineLower, "not charging") &&
				!strings.Contains(lineLower, "discharging")
		}
		break
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
