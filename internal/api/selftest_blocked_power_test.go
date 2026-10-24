// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/stages"
)

// Power management closing task (web-builder, 2026-10-24): the device
// view's selftest_blocked gives the power policy's refusal before the
// click, with the code and params the scheduler refuses the test with
// (sched testRefusal); POST .../selftest answers the same, in the same
// order. A desktop keeps the phase-3b reasons.

const ovCPUName = "Intel(R) Core(TM) Ultra 9 386H" // the fixture's ov:CPU label

var (
	readingAC      = devices.PowerFromStatus(1, 8, 80, 0)    // a machine with a battery, on AC
	readingBattery = devices.PowerFromStatus(0, 0, 63, 0)    // a machine with a battery, on battery
	readingDesktop = devices.PowerFromStatus(1, 128, 255, 0) // no system battery
)

func blockedPowerCPU(name string) *model.ErrorInfo {
	return &model.ErrorInfo{Code: sched.CodeSelfTestPowerCPU, Params: map[string]any{"device": name}}
}

func blockedPowerBattery(name string) *model.ErrorInfo {
	return &model.ErrorInfo{Code: sched.CodeSelfTestPowerBattery, Params: map[string]any{"device": name}}
}

// sameBlocked compares a selftest_blocked with the expected one: both nil,
// or the same code, exactly the same params and no detail.
func sameBlocked(got, want *model.ErrorInfo) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Code == want.Code && paramsEqual(got.Params, want.Params) && got.Detail == ""
}

func TestSelfTestBlockedByPower(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	if !readingAC.BatteryMachine() || readingAC.OnBatteryPower() || !readingBattery.OnBatteryPower() || readingDesktop.BatteryMachine() {
		t.Fatalf("readings %+v %+v %+v", readingAC, readingBattery, readingDesktop)
	}
	setPower := func(p devices.Power) {
		de.ds.mu.Lock()
		de.ds.power = p
		de.ds.mu.Unlock()
	}
	setPresent := func(id string, ok bool) {
		de.reg.mu.Lock()
		de.reg.present[id] = ok
		de.reg.mu.Unlock()
	}
	noAudio := &model.ErrorInfo{Code: stages.CodeSelfTestNoAudio}
	type want map[string]*model.ErrorInfo // device id → selftest_blocked (nil: none)
	check := func(name string, w want) {
		t.Helper()
		var vs []ASRDeviceView
		de.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
		if len(vs) != len(w) {
			t.Fatalf("%s: %d devices, want %d", name, len(vs), len(w))
		}
		for _, v := range vs {
			exp, ok := w[v.ID]
			if !ok {
				t.Fatalf("%s: unexpected device %s", name, v.ID)
			}
			if !sameBlocked(v.SelfTestBlocked, exp) {
				t.Errorf("%s: %s selftest_blocked %+v, want %+v", name, v.ID, v.SelfTestBlocked, exp)
			}
		}
	}

	// A desktop, and an unknown power state (treated as one): no power
	// reason; the phase-3b reasons stay as they were.
	de.setClip(true)
	for _, p := range []devices.Power{readingDesktop, {}} {
		setPower(p)
		check("desktop", want{model.DeviceCUDA0: nil, model.DeviceOVGPU: nil, model.DeviceOVNPU: nil, model.DeviceOVCPU: nil})
	}
	setPower(readingDesktop)
	de.setClip(false)
	check("desktop without audio", want{model.DeviceCUDA0: noAudio, model.DeviceOVGPU: noAudio, model.DeviceOVNPU: noAudio, model.DeviceOVCPU: noAudio})
	de.setClip(true)
	setPresent(model.DeviceOVNPU, false)
	check("desktop, NPU not seen", want{model.DeviceCUDA0: nil, model.DeviceOVGPU: nil, model.DeviceOVCPU: nil,
		model.DeviceOVNPU: {Code: stages.CodeASROVDeviceUnavailable, Params: map[string]any{"device": npuName}}})
	setPresent(model.DeviceOVNPU, true)

	// A machine with a battery on AC: the CPU device (disabled here) is
	// never testable; the accelerators are.
	setPower(readingAC)
	check("on AC", want{model.DeviceCUDA0: nil, model.DeviceOVGPU: nil, model.DeviceOVNPU: nil, model.DeviceOVCPU: blockedPowerCPU(ovCPUName)})
	// The non-power reasons stay for the other devices.
	setPresent(model.DeviceOVNPU, false)
	check("on AC, NPU not seen", want{model.DeviceCUDA0: nil, model.DeviceOVGPU: nil, model.DeviceOVCPU: blockedPowerCPU(ovCPUName),
		model.DeviceOVNPU: {Code: stages.CodeASROVDeviceUnavailable, Params: map[string]any{"device": npuName}}})
	setPresent(model.DeviceOVNPU, true)

	// On battery only the battery device (the NPU) is testable: CUDA and the
	// Intel GPU get selftest.power_battery, the CPU selftest.power_cpu.
	setPower(readingBattery)
	check("on battery", want{model.DeviceCUDA0: blockedPowerBattery(nvidiaName), model.DeviceOVGPU: blockedPowerBattery(igpuName),
		model.DeviceOVNPU: nil, model.DeviceOVCPU: blockedPowerCPU(ovCPUName)})
	// The power reason comes before selftest.no_audio.
	de.setClip(false)
	check("on battery without audio", want{model.DeviceCUDA0: blockedPowerBattery(nvidiaName), model.DeviceOVGPU: blockedPowerBattery(igpuName),
		model.DeviceOVNPU: noAudio, model.DeviceOVCPU: blockedPowerCPU(ovCPUName)})
	de.setClip(true)
	// The battery device follows the registry: without the NPU it is the
	// Intel GPU, and the NPU gets the power reason.
	setPresent(model.DeviceOVNPU, false)
	check("on battery, NPU not seen", want{model.DeviceCUDA0: blockedPowerBattery(nvidiaName), model.DeviceOVGPU: nil,
		model.DeviceOVNPU: blockedPowerBattery(npuName), model.DeviceOVCPU: blockedPowerCPU(ovCPUName)})
	setPresent(model.DeviceOVNPU, true)
	// A row without a label: the device param is its id (sched deviceLabel).
	de.reg.mu.Lock()
	de.reg.rows[0].Label = ""
	de.reg.mu.Unlock()
	check("on battery, cuda:0 without label", want{model.DeviceCUDA0: blockedPowerBattery(model.DeviceCUDA0), model.DeviceOVGPU: blockedPowerBattery(igpuName),
		model.DeviceOVNPU: nil, model.DeviceOVCPU: blockedPowerCPU(ovCPUName)})
	de.reg.mu.Lock()
	de.reg.rows[0].Label = nvidiaName
	de.reg.mu.Unlock()

	// The settings page carries the same views.
	var sv Settings
	de.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	for _, v := range sv.Devices {
		if v.ID == model.DeviceCUDA0 && !sameBlocked(v.SelfTestBlocked, blockedPowerBattery(nvidiaName)) {
			t.Errorf("settings cuda:0 selftest_blocked %+v", v.SelfTestBlocked)
		}
	}

	// POST answers what the view shows, before the scheduler is asked.
	postErr := func(id string) model.ErrorInfo {
		t.Helper()
		return de.callErrInfo("POST", de.devicePath(id, "selftest"), nil, http.StatusConflict)
	}
	for _, c := range []struct {
		id   string
		want *model.ErrorInfo
	}{
		{model.DeviceCUDA0, blockedPowerBattery(nvidiaName)},
		{model.DeviceOVGPU, blockedPowerBattery(igpuName)},
		{model.DeviceOVCPU, blockedPowerCPU(ovCPUName)},
	} {
		if got := postErr(c.id); !sameBlocked(&got, c.want) {
			t.Errorf("POST %s on battery: %+v, want %+v", c.id, got, c.want)
		}
	}
	de.setClip(false)
	if got := postErr(model.DeviceCUDA0); !sameBlocked(&got, blockedPowerBattery(nvidiaName)) {
		t.Errorf("POST cuda:0 on battery without audio: %+v", got)
	}
	if got := postErr(model.DeviceOVNPU); got.Code != stages.CodeSelfTestNoAudio || len(got.Params) != 0 {
		t.Errorf("POST ov:NPU on battery without audio: %+v", got)
	}
	de.setClip(true)
	setPower(readingAC)
	if got := postErr(model.DeviceOVCPU); !sameBlocked(&got, blockedPowerCPU(ovCPUName)) {
		t.Errorf("POST ov:CPU on AC: %+v", got)
	}
	de.ds.mu.Lock()
	starts := de.ds.starts
	de.ds.mu.Unlock()
	if starts != 0 {
		t.Fatalf("the scheduler was asked %d times for refused tests", starts)
	}
	// The battery device on battery, and CUDA on AC, are queued.
	setPower(readingBattery)
	var st SelfTestView
	de.call("POST", de.devicePath(model.DeviceOVNPU, "selftest"), nil, http.StatusAccepted, &st)
	setPower(readingAC)
	de.call("POST", de.devicePath(model.DeviceCUDA0, "selftest"), nil, http.StatusAccepted, &st)
	de.ds.mu.Lock()
	starts = de.ds.starts
	de.ds.mu.Unlock()
	if starts != 2 || st.State != sched.DeviceTestWaiting {
		t.Fatalf("allowed tests: starts %d, state %+v", starts, st)
	}
}

// Against the real scheduler and registry: every power reason of the view
// is exactly the scheduler's refusal (code and params), and a device the
// view does not block for power is not refused for power by the scheduler.
func TestSelfTestBlockedMatchesScheduler(t *testing.T) {
	rd := newRealDevEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		p     devices.Power
		power map[string]string // device id → the power code of selftest_blocked (absent: none)
	}{
		{"desktop", readingDesktop, map[string]string{}},
		{"unknown power state", devices.Power{}, map[string]string{}},
		{"battery machine on AC", readingAC, map[string]string{model.DeviceFWCPU: sched.CodeSelfTestPowerCPU}},
		{"on battery", readingBattery, map[string]string{model.DeviceCUDA0: sched.CodeSelfTestPowerBattery,
			model.DeviceOVGPU: sched.CodeSelfTestPowerBattery, model.DeviceFWCPU: sched.CodeSelfTestPowerCPU}},
	} {
		rd.mu.Lock()
		rd.pw = c.p
		rd.mu.Unlock()
		var vs []ASRDeviceView
		rd.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
		if len(vs) != 4 {
			t.Fatalf("%s: devices %+v", c.name, vs)
		}
		for _, v := range vs {
			b := v.SelfTestBlocked
			code := c.power[v.ID]
			if code == "" {
				// No repaired lecture in this fixture: the 3b reason.
				if b == nil || b.Code != stages.CodeSelfTestNoAudio {
					t.Errorf("%s: %s selftest_blocked %+v, want %s", c.name, v.ID, b, stages.CodeSelfTestNoAudio)
				}
			} else if !sameBlocked(b, &model.ErrorInfo{Code: code, Params: map[string]any{"device": v.Label}}) {
				t.Errorf("%s: %s selftest_blocked %+v, want %s {device: %s}", c.name, v.ID, b, code, v.Label)
			}
			_, err := rd.sc.StartDeviceTest(ctx, v.ID)
			if code != "" {
				var ce *model.CodedError
				if !errors.As(err, &ce) || ce.Info.Code != b.Code || !paramsEqual(ce.Info.Params, b.Params) {
					t.Errorf("%s: %s: the scheduler refuses with %v, the view says %+v", c.name, v.ID, err, b)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %s: refused by the scheduler (%v), the view shows no power reason", c.name, v.ID, err)
				continue
			}
			if _, err := rd.sc.CancelDeviceTest(v.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}
