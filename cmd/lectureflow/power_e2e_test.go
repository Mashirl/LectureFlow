// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// Power management end to end (power-brief §3.1, §7 P1): the development
// power simulation (LECTUREFLOW_DEV_POWER) drives the whole application in
// test mode. Unplugged, a lecture is transcribed on the NPU; plugged in,
// the next one on CUDA. (The fake ASR finishes at once, so unplugging in
// the middle of a run is covered by the scheduler's tests.)
func TestPowerDevSimulationEndToEnd(t *testing.T) {
	ovFake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr_openvino.json"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "power.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"on_battery": true, "battery_pct": 48}`)
	t.Setenv(devices.DevPowerEnv, file)
	e := newE2EWith(t, func(c *config.Config) { c.ASR.FakeResultOpenVINO = ovFake })
	e.waitDetected()

	_, onBattery := e.runPipeline()
	if s := e.asrSnapshot(onBattery); s.DeviceID != model.DeviceOVNPU || !s.Auto || s.Backend != model.BackendOpenVINO {
		t.Fatalf("on battery the asr ran with %+v", s)
	}
	ps := e.app.Sched.PowerStatus()
	if !ps.Power.OnBatteryPower() || ps.Power.BatteryPct == nil || *ps.Power.BatteryPct != 48 ||
		!ps.View.BatteryMachine || ps.View.Reason != devices.PolicyReasonBattery || !slices.Equal(ps.View.Active, []string{model.DeviceOVNPU}) {
		t.Fatalf("power status on battery: %+v", ps)
	}

	write(`{"on_battery": false, "charging": true, "battery_pct": 49}`)
	_, onAC := e.runPipeline()
	if s := e.asrSnapshot(onAC); s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Backend != model.BackendFasterWhisper {
		t.Fatalf("on AC the asr ran with %+v", s)
	}
	ps = e.app.Sched.PowerStatus()
	if ps.Power.OnBattery || !ps.Power.Charging || ps.View.Reason != devices.PolicyReasonACAuto || !slices.Equal(ps.View.Active, []string{model.DeviceCUDA0}) {
		t.Fatalf("power status on AC: %+v", ps)
	}
	// The lecture transcribed on battery keeps its NPU snapshot.
	if s := e.asrSnapshot(onBattery); s.DeviceID != model.DeviceOVNPU {
		t.Fatalf("first lecture now %+v", s)
	}
}
