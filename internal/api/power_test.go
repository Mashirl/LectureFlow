// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Power management (v2-step20, task P4): the settings of the device mode,
// the power fields of GET /api/system, the self-test refused on battery and
// the asr stage's power notes.

// PUT /api/settings/asr with the manual list: merged first (lead ruling 15),
// then checked (power-brief §3.2, 400 with the settings codes), stored
// normalized; GET /api/settings sends it back (omitted in auto mode).
func TestPutASRManualDevices(t *testing.T) {
	rd := newRealDevEnv(t)
	var sv Settings
	rd.call("PUT", "/api/settings/asr", map[string]any{"device_mode": "manual", "manual_devices": []string{"ov:GPU", " cuda:0", "ov:GPU"}}, http.StatusOK, &sv)
	if sv.ASR.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(sv.ASR.ManualDevices, []string{model.DeviceOVGPU, model.DeviceCUDA0}) {
		t.Fatalf("manual %+v", sv.ASR)
	}
	// A body without the list keeps it.
	rd.call("PUT", "/api/settings/asr", map[string]any{"multi_device": false}, http.StatusOK, &sv)
	if sv.ASR.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(sv.ASR.ManualDevices, []string{model.DeviceOVGPU, model.DeviceCUDA0}) || sv.ASR.MultiDevice {
		t.Fatalf("after a one-field PUT %+v", sv.ASR)
	}
	// The configuration view shows the list.
	var sys System
	rd.call("GET", "/api/system", nil, http.StatusOK, &sys)
	b, _ := json.Marshal(sys.Config)
	if !strings.Contains(string(b), `"device_mode":"manual"`) || !strings.Contains(string(b), `"manual_devices":["ov:GPU","cuda:0"]`) {
		t.Fatalf("config %s", b)
	}
	cases := []struct {
		body   map[string]any
		code   string
		params map[string]any
	}{
		{map[string]any{"manual_devices": []string{}}, settings.CodeManualDevicesEmpty, nil},
		{map[string]any{"manual_devices": []string{"ov:NPU", "fw:cpu"}}, settings.CodeManualDevicesCPU, map[string]any{"device": "CPU (faster-whisper)"}},
		{map[string]any{"manual_devices": []string{"ov:XPU"}}, settings.CodeManualDevicesUnknown, map[string]any{"device": "ov:XPU"}},
		{map[string]any{"device_mode": "fw:cpu"}, settings.CodeManualDevicesCPU, map[string]any{"device": "CPU (faster-whisper)"}},
		{map[string]any{"device_mode": "ov:XPU"}, settings.CodeDeviceModeInvalid, map[string]any{"value": "ov:XPU"}},
	}
	for _, c := range cases {
		info := rd.callErrInfo("PUT", "/api/settings/asr", c.body, http.StatusBadRequest)
		if info.Code != c.code || (len(c.params) > 0 && !paramsEqual(info.Params, c.params)) {
			t.Errorf("%v: %+v, want %s %v", c.body, info, c.code, c.params)
		}
	}
	rd.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if !slices.Equal(sv.ASR.ManualDevices, []string{model.DeviceOVGPU, model.DeviceCUDA0}) {
		t.Fatalf("a refused PUT changed the list: %+v", sv.ASR)
	}
	// An older client's single device id: manual [id].
	rd.call("PUT", "/api/settings/asr", map[string]any{"device_mode": model.DeviceCUDA0}, http.StatusOK, &sv)
	if sv.ASR.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(sv.ASR.ManualDevices, []string{model.DeviceCUDA0}) {
		t.Fatalf("3b id %+v", sv.ASR)
	}
	// auto: the list is gone, also from the JSON.
	res, raw := rd.do("PUT", "/api/settings/asr", map[string]any{"device_mode": "auto"})
	if res.StatusCode != http.StatusOK || strings.Contains(string(raw), "manual_devices") || !strings.Contains(string(raw), `"device_mode":"auto"`) {
		t.Fatalf("auto: %d %s", res.StatusCode, raw)
	}
	rd.call("GET", "/api/system", nil, http.StatusOK, &sys)
	if b, _ := json.Marshal(sys.Config); strings.Contains(string(b), "manual_devices") {
		t.Fatalf("config in auto mode %s", b)
	}
}

func paramsEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// GET /api/system: the power state with has_battery, charging and source.
func TestSystemPowerFields(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	de.srv.diag.waitStartup()
	for _, c := range []struct {
		p      devices.Power
		source string
	}{
		{devices.PowerFromStatus(0, 0, 63, 0), resources.PowerSourceBattery},
		{devices.PowerFromStatus(1, 8, 80, 0), resources.PowerSourceAC},
		{devices.PowerFromStatus(1, 128, 255, 0), resources.PowerSourceAC}, // a desktop
		{devices.Power{}, ""},
	} {
		de.ds.mu.Lock()
		de.ds.power = c.p
		de.ds.mu.Unlock()
		var sys System
		de.call("GET", "/api/system", nil, http.StatusOK, &sys)
		p := sys.Power
		if p == nil || p.Source != c.source || p.HasBattery != c.p.HasBattery || p.Charging != c.p.Charging || p.OnBattery != c.p.OnBattery ||
			p.Known != c.p.Known || (p.BatteryPct == nil) != (c.p.BatteryPct == nil) || p.BatteryAware != de.set.ASR().BatteryAware {
			t.Errorf("%+v: power %+v", c.p, p)
		}
	}
}

// POST /api/asr/devices/{id}/selftest on battery for a device that is not
// the battery device: 409 selftest.power_battery {device} (the coded error
// of the scheduler, or its bare sentinel).
func TestSelfTestRefusedOnBattery(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	de.setClip(true)
	path := de.devicePath(model.DeviceCUDA0, "selftest")
	for _, err := range []error{
		&model.CodedError{Info: model.ErrorInfo{Code: sched.CodeSelfTestPowerBattery, Params: map[string]any{"device": nvidiaName}},
			Err: sched.ErrSelfTestPowerBattery},
		sched.ErrSelfTestPowerBattery,
	} {
		de.ds.mu.Lock()
		de.ds.startErr = err
		de.ds.mu.Unlock()
		info := de.callErrInfo("POST", path, nil, http.StatusConflict)
		if info.Code != sched.CodeSelfTestPowerBattery || info.Params["device"] != nvidiaName {
			t.Errorf("%v: %+v", err, info)
		}
	}
	// selftest.running keeps its own code (both wrap state.ErrConflict).
	de.ds.mu.Lock()
	de.ds.startErr = sched.ErrSelfTestRunning
	de.ds.mu.Unlock()
	if info := de.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != sched.CodeSelfTestRunning {
		t.Errorf("running: %+v", info)
	}
}

// The asr stage's note while queued or running: job.stats.power_event as
// notes.power.* {lecture, device} (power-brief §5), with the device's label;
// not shown once the job is done.
func TestASRPowerEventNote(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	ctx := context.Background()
	c := de.course("C")
	l := de.lecture(c.ID, "W03 L1 Block ciphers")
	de.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	j := de.moveJob(model.ScopeLecture, l.ID, "asr", "queued")
	note := func() StageView {
		t.Helper()
		var lv Lecture
		de.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		return stageOf(lv.Stages, "asr")
	}
	set := func(ev *model.PowerEvent) {
		t.Helper()
		cur, err := de.st.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		cur.Stats.PowerEvent = ev
		if err := de.st.SetJobStats(ctx, j.ID, cur.Stats); err != nil {
			t.Fatal(err)
		}
	}
	if st := note(); st.NoteInfo != nil {
		t.Fatalf("no event: %+v", st.NoteInfo)
	}
	cases := []struct {
		ev     model.PowerEvent
		params map[string]any
		zh     string
	}{
		{model.PowerEvent{Code: sched.CodePowerSuspended, At: "2026-10-24T07:12:05Z", FromDevice: model.DeviceCUDA0},
			map[string]any{"lecture": l.Title}, "已挂起：电源感知 - 已拔掉电源，《W03 L1 Block ciphers》等待电源适配器"},
		{model.PowerEvent{Code: sched.CodePowerSwitched, At: "2026-10-24T07:12:05Z", FromDevice: model.DeviceOVGPU, ToDevice: model.DeviceOVNPU},
			map[string]any{"lecture": l.Title, "device": npuName}, "已切换：电源感知 - 已拔掉电源，《W03 L1 Block ciphers》转到 " + npuName + " 继续"},
		{model.PowerEvent{Code: sched.CodePowerResumed, At: "2026-10-24T08:00:00Z", FromDevice: model.DeviceCUDA0},
			map[string]any{"lecture": l.Title, "device": nvidiaName}, "已恢复：电源感知 - 已接通电源，《W03 L1 Block ciphers》在 " + nvidiaName + " 上继续"},
	}
	for _, c := range cases {
		ev := c.ev
		set(&ev)
		st := note()
		if st.NoteInfo == nil || st.NoteInfo.Code != c.ev.Code || !paramsEqual(st.NoteInfo.Params, c.params) || st.Note == nil || *st.Note != c.zh {
			t.Errorf("%s: note %v %+v", c.ev.Code, st.Note, st.NoteInfo)
		}
	}
	// Running: still shown, first, joined with the model note.
	de.moveJob(model.ScopeLecture, l.ID, "asr", "running")
	cur, _ := de.st.GetJob(ctx, j.ID)
	cur.Stats.Status = model.ASRStatusCompilingModel
	if err := de.st.SetJobStats(ctx, j.ID, cur.Stats); err != nil {
		t.Fatal(err)
	}
	st := note()
	if st.NoteInfo == nil || st.NoteInfo.Code != sched.CodePowerResumed || st.NoteInfo.Params["extra"] == nil ||
		st.Note == nil || !strings.HasPrefix(*st.Note, "已恢复：电源感知") || !strings.Contains(*st.Note, "正在编译模型") {
		t.Fatalf("running: %v %+v", st.Note, st.NoteInfo)
	}
	// Done: the event stays in the stats but is not a note any more.
	de.moveJob(model.ScopeLecture, l.ID, "asr", "done")
	if st := note(); st.NoteInfo != nil && strings.HasPrefix(st.NoteInfo.Code, "power.") {
		t.Fatalf("done: %+v", st.NoteInfo)
	}
	// An unknown code is ignored.
	if _, _, ok := de.srv.powerNote(model.PowerEvent{Code: "power.other"}, "x"); ok {
		t.Fatal("unknown code produced a note")
	}
}
