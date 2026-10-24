// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Power management, front-end gate round 1 (web-builder): the mock's policy
// comes from internal/devices (Place, View, SelfTest, BatteryDevice), the
// manual list is checked in settings.SetASR's order, and two scenarios —
// everything suspended, plugged in while the NPU runs.

// The manual list is checked in settings.SetASR's order: a CPU device is
// refused as such even when it has no row (here the hidden OpenVINO CPU of
// scenario noov), before the check that the device exists.
func TestMockManualDevicesCheckOrder(t *testing.T) {
	srv := testServer(t)
	manual := func(ids ...string) map[string]any {
		return map[string]any{"device_mode": model.ASRDeviceModeManual, "manual_devices": ids}
	}
	u := srv.URL + "/api/settings/asr?mock=noov"
	if e := wantCode(t, "PUT", u, manual(model.DeviceOVCPU), 400, settings.CodeManualDevicesCPU); e.Params["device"] != model.DeviceOVCPU {
		t.Errorf("cpu without a row %+v", e)
	}
	// in the order sent: the unknown device before the CPU one, and back
	if e := wantCode(t, "PUT", u, manual("ov:XPU", model.DeviceFWCPU), 400, settings.CodeManualDevicesUnknown); e.Params["device"] != "ov:XPU" {
		t.Errorf("unknown first %+v", e)
	}
	if e := wantCode(t, "PUT", u, manual(model.DeviceFWCPU, "ov:XPU"), 400, settings.CodeManualDevicesCPU); e.Params["device"] != labelFWCPU {
		t.Errorf("cpu first %+v", e)
	}
}

// Everything suspended (power_all_suspended): on battery with the Intel
// devices turned off nothing qualifies (never the NVIDIA card, never the
// CPU): no active device, the reason for the middle area, the suspended
// lecture and the one waiting for a power-saving device.
func TestMockPowerAllSuspended(t *testing.T) {
	srv := testServer(t)
	const sc = "power_all_suspended"
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock="+sc, nil, http.StatusOK, &res)
	p := res.Policy
	if p == nil || !p.BatteryMachine || p.Mode != model.ASRDeviceModeAuto || p.Reason != "battery" || len(p.Active) != 0 || p.Next != "" ||
		p.NoneReason != model.WaitPowerNoSaverDevice || p.WaitAC != 1 || p.WaitNoDevice != 1 {
		t.Fatalf("policy %+v", p)
	}
	if len(p.Notices) != 1 || p.Notices[0].Info.Code != sched.CodePowerSuspended || p.Notices[0].LectureID != lecID(c3631, 7) {
		t.Errorf("notices %+v", p.Notices)
	}
	if pw := res.Power; pw == nil || !pw.HasBattery || !pw.OnBattery || pw.Source != resources.PowerSourceBattery {
		t.Errorf("power %+v", pw)
	}
	if len(res.Devices) != 5 {
		t.Fatalf("rows %+v", res.Devices)
	}
	for _, c := range res.Devices {
		if c.Active || len(c.Tasks) != 0 || c.SlotsUsed != 0 {
			t.Errorf("%s: active %v, tasks %+v", c.ID, c.Active, c.Tasks)
		}
	}
	for _, id := range []string{model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU, model.DeviceFWCPU} {
		if c := cardOf(res, id); c == nil || c.Enabled || c.BatteryHold {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if c := cardOf(res, model.DeviceCUDA0); c == nil || !c.Enabled || !c.HoldsSuspended || !c.BatteryHold {
		t.Errorf("cuda %+v", c)
	}
	held := asrStageOf(t, srv.URL, lecID(c3631, 7), sc)
	if held.Status != model.StatusQueued || held.WaitingFor == nil || *held.WaitingFor != model.WaitPowerWaitAC || held.Progress != 0 ||
		held.NoteInfo == nil || held.NoteInfo.Code != sched.CodePowerSuspended {
		t.Errorf("suspended lecture %+v", held)
	}
	if st := asrStageOf(t, srv.URL, lecID(c2120, 7), sc); st.Status != model.StatusQueued || st.WaitingFor == nil || *st.WaitingFor != model.WaitPowerNoSaverDevice {
		t.Errorf("queued lecture %+v", st)
	}
	// on battery without a battery device no device may be tested
	wantCode(t, "POST", srv.URL+"/api/asr/devices/cuda:0/selftest?mock="+sc, nil, http.StatusConflict, sched.CodeSelfTestPowerBattery)
}

// Plugged in while the NPU runs a lecture (power_ac_switch): devices.View
// keeps the NPU active and names the NVIDIA card as the next device; the
// queued lecture waits for the switch.
func TestMockPowerSwitchPending(t *testing.T) {
	srv := testServer(t)
	const sc = "power_ac_switch"
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock="+sc, nil, http.StatusOK, &res)
	p := res.Policy
	if p == nil || p.Reason != "ac_auto" || !slices.Equal(p.Active, []string{model.DeviceOVNPU}) || p.Next != model.DeviceCUDA0 || p.NoneReason != "" || len(p.Notices) != 0 {
		t.Fatalf("policy %+v", p)
	}
	if res.Power == nil || res.Power.Source != resources.PowerSourceAC || !res.Power.Charging {
		t.Errorf("power %+v", res.Power)
	}
	if c := cardOf(res, model.DeviceOVNPU); c == nil || !c.Active || len(c.Tasks) != 1 || c.Tasks[0].LectureID != lecID(c3631, 7) || c.BatteryHold {
		t.Errorf("npu %+v", c)
	}
	if c := cardOf(res, model.DeviceCUDA0); c == nil || c.Active || len(c.Tasks) != 0 || c.BatteryHold {
		t.Errorf("cuda %+v", c)
	}
	if st := asrStageOf(t, srv.URL, lecID(c2120, 7), sc); st.WaitingFor == nil || *st.WaitingFor != model.WaitPowerSwitchPending(model.DeviceOVNPU, model.DeviceCUDA0) {
		t.Errorf("queued %+v", st.WaitingFor)
	}
}

// devices.SelfTest on a laptop: a test waits while another device is
// active, with the power-aware reason power_queue:{active device}; a test
// of the active device itself waits for its own transcription (3b, no
// power reason).
func TestMockSelfTestWaitsForActiveDevice(t *testing.T) {
	h := newHandler(filepath.Join("..", "..", "..", "web"), filepath.Join("..", "..", "..", "testdata", "api"), 10*time.Millisecond)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	m := h.(*handler).m
	testOf := func(s *store, id string) *mockTest {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, d := range s.dev.devs {
			if d.row.ID == id {
				return d.test
			}
		}
		return nil
	}
	// the default data: a lecture runs on the NVIDIA card
	var st api.SelfTestView
	doJSON(t, "POST", srv.URL+"/api/asr/devices/ov:GPU/selftest", nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestWaiting || st.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Fatalf("ov:GPU %+v", st)
	}
	if tt := testOf(m.main, model.DeviceOVGPU); tt == nil || tt.waitFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Errorf("ov:GPU waits for %+v", tt)
	}
	var vs []api.ASRDeviceView // the device list carries it too
	doJSON(t, "GET", srv.URL+"/api/asr/devices", nil, http.StatusOK, &vs)
	if d := deviceByID(vs, model.DeviceOVGPU); d == nil || d.SelfTest.State != sched.DeviceTestWaiting || d.SelfTest.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Errorf("ov:GPU in the list %+v", d)
	}
	// on battery the lecture runs on the NPU: its own test waits for it,
	// without a power-aware reason
	st = api.SelfTestView{} // waiting_for is omitted when empty
	doJSON(t, "POST", srv.URL+"/api/asr/devices/ov:NPU/selftest?mock=battery", nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestWaiting || st.WaitingFor != "" {
		t.Fatalf("battery ov:NPU %+v", st)
	}
	if tt := testOf(m.battery.s, model.DeviceOVNPU); tt == nil || tt.waitFor != "" {
		t.Errorf("battery ov:NPU waits for %+v", tt)
	}
}

// While a self-test holds the active device of a laptop, the lecture that
// queues for that device waits for the test (device_selftest:{id}, shown
// as "排队：电源感知 - 等待 … 完成测试"), and for the device again after it.
func TestMockSelfTestHoldsQueuedLecture(t *testing.T) {
	old := selftestScale
	selftestScale = 0.02 // a test of about 0.6 s
	t.Cleanup(func() { selftestScale = old })
	srv := testServer(t)
	const sc = "power_ac_intel"
	queued := lecID(c2120, 7)
	u := srv.URL + "/api/asr/devices/ov:GPU/selftest?mock=" + sc
	wait := func() string {
		t.Helper()
		var st api.SelfTestView // the device read moves the simulated test on
		doJSON(t, "GET", u, nil, http.StatusOK, &st)
		if w := asrStageOf(t, srv.URL, queued, sc).WaitingFor; w != nil {
			return *w
		}
		return ""
	}
	if w := wait(); w != model.WaitPowerQueue(model.DeviceOVGPU) {
		t.Fatalf("before the test: %q", w)
	}
	var st api.SelfTestView
	doJSON(t, "POST", u, nil, http.StatusAccepted, &st)
	seen := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen[wait()] = true
		doJSON(t, "GET", u, nil, http.StatusOK, &st)
		if st.State == sched.DeviceTestDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.State != sched.DeviceTestDone || !seen[model.WaitDeviceSelfTest(model.DeviceOVGPU)] {
		t.Fatalf("test %s, waits seen %v", st.State, seen)
	}
	for w := range seen {
		if w != model.WaitPowerQueue(model.DeviceOVGPU) && w != model.WaitDeviceSelfTest(model.DeviceOVGPU) {
			t.Errorf("unexpected wait %q", w)
		}
	}
	if w := wait(); w != model.WaitPowerQueue(model.DeviceOVGPU) {
		t.Errorf("after the test: %q", w)
	}
}

// A laptop never tests a CPU device, plugged in or on battery
// (selftest.power_cpu, sched testRefusal); other devices refused on battery
// keep selftest.power_battery; a desktop tests its CPU device.
func TestMockSelfTestPowerCPU(t *testing.T) {
	srv := testServer(t)
	if e := wantCode(t, "POST", srv.URL+"/api/asr/devices/fw:cpu/selftest", nil, http.StatusConflict, sched.CodeSelfTestPowerCPU); e.Params["device"] != labelFWCPU {
		t.Errorf("AC fw:cpu %+v", e)
	}
	if e := wantCode(t, "POST", srv.URL+"/api/asr/devices/ov:CPU/selftest?mock=battery", nil, http.StatusConflict, sched.CodeSelfTestPowerCPU); e.Params["device"] != labelOVCPU {
		t.Errorf("battery ov:CPU %+v", e)
	}
	wantCode(t, "POST", srv.URL+"/api/asr/devices/ov:GPU/selftest?mock=battery", nil, http.StatusConflict, sched.CodeSelfTestPowerBattery)
	var st api.SelfTestView
	doJSON(t, "POST", srv.URL+"/api/asr/devices/fw:cpu/selftest?mock=cpuonly", nil, http.StatusAccepted, &st)
	if st.DeviceID != model.DeviceFWCPU || st.WaitingFor != "" {
		t.Errorf("desktop fw:cpu %+v", st)
	}
}
