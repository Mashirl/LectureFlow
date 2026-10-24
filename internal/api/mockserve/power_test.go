// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"slices"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Power management in the mock (power.go): the four resource-bar scenarios
// of the screenshots (power-req part 3) and the manual device list.

func cardOf(res resources.Snapshot, id string) *resources.Device {
	for i := range res.Devices {
		if res.Devices[i].ID == id {
			return &res.Devices[i]
		}
	}
	return nil
}

func asrStageOf(t *testing.T, base, id, sc string) api.StageView {
	t.Helper()
	var l api.Lecture
	doJSON(t, "GET", base+"/api/lectures/"+id+"?mock="+sc, nil, http.StatusOK, &l)
	for _, st := range l.Stages {
		if st.Stage == model.StageASR {
			return st
		}
	}
	t.Fatalf("%s: no asr stage", id)
	return api.StageView{}
}

func TestMockPowerScenarios(t *testing.T) {
	srv := testServer(t)
	get := func(sc string) resources.Snapshot {
		t.Helper()
		var res resources.Snapshot
		doJSON(t, "GET", srv.URL+"/api/resources?mock="+sc, nil, http.StatusOK, &res)
		if res.Power == nil || res.Policy == nil || res.System == nil || res.Policy.Active == nil || res.Policy.Notices == nil {
			t.Fatalf("%s: power %+v policy %+v system %+v", sc, res.Power, res.Policy, res.System)
		}
		for _, c := range res.Devices {
			if c.Active != slices.Contains(res.Policy.Active, c.ID) {
				t.Errorf("%s: %s active %v, policy %v", sc, c.ID, c.Active, res.Policy.Active)
			}
			if c.Active && c.Kind == model.DeviceKindCPU {
				t.Errorf("%s: a CPU device is active on a machine with a battery", sc)
			}
		}
		return res
	}
	queued := lecID(c2120, 7)
	running := lecID(c3631, 7)

	// AC + NVIDIA card
	res := get("power_ac_dgpu")
	if p := res.Power; !p.HasBattery || p.OnBattery || !p.Charging || p.Source != resources.PowerSourceAC || p.BatteryPct == nil {
		t.Errorf("ac_dgpu power %+v", p)
	}
	if p := res.Policy; p.Reason != "ac_auto" || len(p.Active) != 1 || p.Active[0] != model.DeviceCUDA0 || p.Next != "" || p.WaitAC != 0 {
		t.Errorf("ac_dgpu policy %+v", p)
	}
	if c := cardOf(res, model.DeviceCUDA0); c == nil || len(c.Tasks) != 1 || c.Tasks[0].LectureID != running {
		t.Errorf("ac_dgpu cuda %+v", c)
	}
	if res.System.CPUTempC == nil || !res.GPU.Available {
		t.Errorf("ac_dgpu system %+v gpu %+v", res.System, res.GPU)
	}

	// AC + Intel only: no NVIDIA row, the Intel GPU runs, no CPU temperature
	res = get("power_ac_intel")
	if cardOf(res, model.DeviceCUDA0) != nil || res.GPU.Available || res.System.CPUTempC != nil {
		t.Errorf("ac_intel rows %+v gpu %+v system %+v", res.Devices, res.GPU, res.System)
	}
	if p := res.Policy; p.Reason != "ac_auto" || len(p.Active) != 1 || p.Active[0] != model.DeviceOVGPU {
		t.Errorf("ac_intel policy %+v", p)
	}
	if c := cardOf(res, model.DeviceOVGPU); c == nil || len(c.Tasks) != 1 || c.MemUsedMB == nil || c.UtilPct == nil {
		t.Errorf("ac_intel gpu %+v", c)
	}
	if st := asrStageOf(t, srv.URL, queued, "power_ac_intel"); st.WaitingFor == nil || *st.WaitingFor != model.WaitPowerQueue(model.DeviceOVGPU) {
		t.Errorf("ac_intel queued %+v", st.WaitingFor)
	}

	// battery + NPU: the lecture switched from the Intel GPU
	res = get("power_battery_npu")
	if p := res.Power; !p.OnBattery || p.Charging || p.Source != resources.PowerSourceBattery {
		t.Errorf("battery_npu power %+v", p)
	}
	if p := res.Policy; p.Reason != "battery" || len(p.Active) != 1 || p.Active[0] != model.DeviceOVNPU || len(p.Notices) != 1 || p.Notices[0].Info.Code != sched.CodePowerSwitched {
		t.Errorf("battery_npu policy %+v", p)
	}
	if c := cardOf(res, model.DeviceOVNPU); c == nil || len(c.Tasks) != 1 || c.UtilPct == nil {
		t.Errorf("battery_npu npu %+v", c)
	}
	if c := cardOf(res, model.DeviceCUDA0); c == nil || !c.BatteryHold || c.HoldsSuspended || len(c.Tasks) != 0 {
		t.Errorf("battery_npu cuda %+v", c)
	}
	if st := asrStageOf(t, srv.URL, running, "power_battery_npu"); st.NoteInfo == nil || st.NoteInfo.Code != sched.CodePowerSwitched {
		t.Errorf("battery_npu note %+v", st.NoteInfo)
	}
	// the hover card's parts of a lecture resumed on the NPU
	st := asrStageOf(t, srv.URL, lecID(c3631, 3), "power_battery_npu")
	if st.Model == nil || st.Model.ASR == nil || len(st.Model.ASR.Parts) != 2 || st.Model.ASR.Parts[0].DeviceID != model.DeviceOVGPU || st.Model.ASR.Parts[1].ToMS != 3300000 || st.Model.ASR.DeviceID != model.DeviceOVNPU {
		t.Errorf("parts %+v", st.Model)
	}
	// on battery only the battery device may be tested
	wantCode(t, "POST", srv.URL+"/api/asr/devices/cuda:0/selftest?mock=power_battery_npu", nil, http.StatusConflict, sched.CodeSelfTestPowerBattery)

	// battery with a suspended NVIDIA job
	res = get("power_battery_suspended")
	// one notice: the suspension (no switch notice at the same moment)
	if p := res.Policy; p.WaitAC != 1 || p.WaitNoDevice != 0 || len(p.Notices) != 1 || p.Notices[0].Info.Code != sched.CodePowerSuspended || p.Active[0] != model.DeviceOVNPU {
		t.Errorf("suspended policy %+v", p)
	}
	if st := asrStageOf(t, srv.URL, running, "power_battery_suspended"); st.NoteInfo != nil {
		t.Errorf("suspended: the lecture on the NPU has a note %+v", st.NoteInfo)
	}
	if c := cardOf(res, model.DeviceCUDA0); c == nil || !c.HoldsSuspended || c.Active {
		t.Errorf("suspended cuda %+v", c)
	}
	st = asrStageOf(t, srv.URL, queued, "power_battery_suspended")
	if st.WaitingFor == nil || *st.WaitingFor != model.WaitPowerWaitAC || st.NoteInfo == nil || st.NoteInfo.Code != sched.CodePowerSuspended {
		t.Errorf("suspended queued %+v %+v", st.WaitingFor, st.NoteInfo)
	}

	// the default world is a laptop on AC power without notices
	res = get("1")
	if len(res.Policy.Notices) != 0 || res.Policy.Reason != "ac_auto" {
		t.Errorf("default policy %+v", res.Policy)
	}
}

// PUT /api/settings/asr with the manual list (power-brief §3.2).
func TestMockManualDevices(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/settings/asr"
	manual := func(ids ...string) map[string]any {
		return map[string]any{"device_mode": model.ASRDeviceModeManual, "manual_devices": ids}
	}
	wantCode(t, "PUT", u, manual(), 400, settings.CodeManualDevicesEmpty)
	wantCode(t, "PUT", u, manual(" "), 400, settings.CodeManualDevicesEmpty)
	if e := wantCode(t, "PUT", u, manual(model.DeviceCUDA0, model.DeviceFWCPU), 400, settings.CodeManualDevicesCPU); e.Params["device"] != labelFWCPU {
		t.Errorf("cpu error %+v", e)
	}
	wantCode(t, "PUT", u, manual("ov:XPU"), 400, settings.CodeManualDevicesUnknown)
	wantCode(t, "PUT", u, map[string]any{"device_mode": "ov:XPU"}, 400, settings.CodeDeviceModeInvalid)

	var s api.Settings
	doJSON(t, "PUT", u, manual(model.DeviceOVGPU, " cuda:0 ", model.DeviceOVGPU), http.StatusOK, &s)
	if a := s.ASR; a.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(a.ManualDevices, []string{model.DeviceOVGPU, model.DeviceCUDA0}) {
		t.Fatalf("manual %+v", a)
	}
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources", nil, http.StatusOK, &res)
	if p := res.Policy; p.Mode != model.ASRDeviceModeManual || p.Reason != "manual" || p.ManualIgnored || len(p.Active) != 1 || p.Active[0] != model.DeviceCUDA0 {
		t.Errorf("manual policy %+v", p)
	}
	// a merge keeps the list; auto clears it
	doJSON(t, "PUT", u, map[string]any{"language": "en"}, http.StatusOK, &s)
	if len(s.ASR.ManualDevices) != 2 {
		t.Errorf("merged %+v", s.ASR)
	}
	s = api.Settings{} // manual_devices is omitted when empty
	doJSON(t, "PUT", u, map[string]any{"device_mode": model.ASRDeviceModeAuto}, http.StatusOK, &s)
	if s.ASR.DeviceMode != model.ASRDeviceModeAuto || s.ASR.ManualDevices != nil {
		t.Errorf("auto %+v", s.ASR)
	}

	// on battery the manual list does not apply
	doJSON(t, "PUT", u+"?mock=battery", manual(model.DeviceCUDA0), http.StatusOK, &s)
	res = resources.Snapshot{}
	doJSON(t, "GET", srv.URL+"/api/resources?mock=battery", nil, http.StatusOK, &res)
	if p := res.Policy; p.Mode != model.ASRDeviceModeAuto || !p.ManualIgnored || p.Reason != "battery" || p.Active[0] != model.DeviceOVNPU {
		t.Errorf("battery manual policy %+v", p)
	}
}
