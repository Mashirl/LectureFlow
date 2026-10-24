// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// End-to-end test of SPEC-v2 I6/I8 (phase 3b task C) through the real
// wiring in test mode: device detection without processes, binding at
// start, and the A4.3 decision after a device change (lead rulings 1, 2).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

func (e *e2e) asrSnapshot(lectureID string) model.ASRSnapshot {
	e.t.Helper()
	j, err := e.app.Store.GetCurrentJob(context.Background(), model.ScopeLecture, lectureID, model.StageASR)
	if err != nil {
		e.t.Fatal(err)
	}
	s, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok {
		e.t.Fatalf("asr snapshot %q", j.ConfigSnapshot)
	}
	return s
}

func TestV3bDevicesEndToEnd(t *testing.T) {
	ovFake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr_openvino.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Test mode with the fake OpenVINO result: the default probe reports
	// the Intel GPU and NPU without starting a process.
	e := newE2EWith(t, func(c *config.Config) { c.ASR.FakeResultOpenVINO = ovFake })
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for !e.app.Devices.Detected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.app.Devices.Detected() {
		t.Fatal("first device detection did not finish")
	}
	ids := map[string]bool{}
	for _, d := range e.app.Devices.List() {
		ids[d.ID] = true
	}
	if !ids[model.DeviceCUDA0] || !ids[model.DeviceOVGPU] || !ids[model.DeviceOVNPU] {
		t.Fatalf("devices after detection: %+v", e.app.Devices.List())
	}
	if sts := e.app.Sched.DeviceStatuses(); len(sts) != len(ids) || sts[0].ID != model.DeviceCUDA0 {
		t.Fatalf("device statuses: %+v", sts)
	}

	// Auto mode: the launch writes an auto snapshot, the start binds the
	// highest-priority device (cuda:0).
	_, lectureID := e.runPipeline()
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Backend != model.BackendFasterWhisper {
		t.Fatalf("asr ran with %+v", s)
	}

	// The user drags the Intel GPU to the top: nothing becomes stale, and
	// the rerun asks nothing — the device the scheduler picks in auto mode
	// is not a change of the settings (lead ruling 2, refined).
	if _, err := e.app.Devices.Reorder(ctx, []string{model.DeviceOVGPU, model.DeviceCUDA0, model.DeviceOVNPU, model.DeviceFWCPU}); err != nil { // every row: fw:cpu always has one
		t.Fatal(err)
	}
	if _, err := e.app.Sched.CheckStale(ctx, lectureID); err != nil {
		t.Fatal(err)
	}
	if l := e.lecture(lectureID); !allDone(l) {
		t.Fatalf("a device change made a stage leave done: %+v", l.Stages)
	}
	path := "/api/lectures/" + lectureID + "/stages/asr/rerun"
	if status, body := e.doRaw(http.MethodPost, path, nil); status != http.StatusOK {
		t.Fatalf("rerun after reordering: HTTP %d %s", status, body)
	}
	e.waitFor("rerun on the new first device", func() bool { return allDone(e.lecture(lectureID)) }, lectureID)
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceOVGPU || !s.Auto || s.Backend != model.BackendOpenVINO {
		t.Fatalf("auto rerun ran with %+v", s)
	}

	// The bound device's precision changes: the rerun asks (409).
	prec := model.PrecisionINT8
	if _, err := e.app.Devices.Update(ctx, model.DeviceOVGPU, model.ASRDevicePatch{Precision: &prec}); err != nil {
		t.Fatal(err)
	}
	asrJob := e.jobID(lectureID, model.StageASR)
	nm := e.needsChoice(path)
	if len(nm.Diff) != 1 || nm.Diff[0].Stage != model.StageASR || !nm.Diff[0].SnapshotUsable {
		t.Fatalf("diff after the precision change: %+v", nm.Diff)
	}
	if e.jobID(lectureID, model.StageASR) != asrJob {
		t.Fatal("a 409 must not change anything")
	}
	e.rerunWith(lectureID, path, model.ModelChoiceCurrent)
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceOVGPU || !s.Auto || s.Precision != model.PrecisionINT8 {
		t.Fatalf("current choice ran with %+v", s)
	}

	// SPEC-v2 I11: the device mode set to one device → the rerun asks. Since
	// power management the 3b device id is stored as manual [cuda:0]
	// (power-brief §3.2), and the rerun asks because the list's effective
	// device (cuda:0) is not the one the lecture ran on (ov:GPU; lead
	// decision A: effective configurations are compared).
	a := e.app.Settings.ASR()
	a.DeviceMode = model.DeviceCUDA0
	if err := e.app.Settings.SetASR(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := e.app.Settings.ASR(); got.DeviceMode != model.ASRDeviceModeManual || len(got.ManualDevices) != 1 || got.ManualDevices[0] != model.DeviceCUDA0 {
		t.Fatalf("stored device mode %q %v", got.DeviceMode, got.ManualDevices)
	}
	if nm := e.needsChoice(path); len(nm.Diff) != 1 || nm.Diff[0].Stage != model.StageASR {
		t.Fatalf("diff after the device mode change: %+v", nm.Diff)
	} else if cur := nm.Diff[0].Current.ASR; cur == nil || cur.DeviceID != model.DeviceCUDA0 || !cur.Auto {
		t.Fatalf("current of the diff: %+v", nm.Diff[0].Current)
	}
	// 沿用原模型: fixed to the Intel GPU it ran on.
	e.rerunWith(lectureID, path, model.ModelChoiceSnapshot)
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceOVGPU || s.Auto {
		t.Fatalf("pinned snapshot: %+v", s)
	}
	// 使用当前设置: cuda:0, the manual list's device — an auto snapshot since
	// power management (manual is a filter, power-brief §3.2; before: a
	// fixed cuda:0 snapshot).
	e.rerunWith(lectureID, path, model.ModelChoiceCurrent)
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Backend != model.BackendFasterWhisper {
		t.Fatalf("current choice ran with %+v", s)
	}
}

// needsChoice posts a rerun without a choice and expects 409 needs_model_choice.
func (e *e2e) needsChoice(path string) api.NeedsModelChoice {
	e.t.Helper()
	status, body := e.doRaw(http.MethodPost, path, nil)
	if status != http.StatusConflict {
		e.t.Fatalf("rerun without a choice: HTTP %d %s, want 409", status, body)
	}
	var nm api.NeedsModelChoice
	if err := json.Unmarshal(body, &nm); err != nil {
		e.t.Fatal(err)
	}
	if nm.Error.Code != model.ErrNeedsModelChoice {
		e.t.Fatalf("409 body: %s", body)
	}
	return nm
}

// rerunWith posts a rerun with a model choice and waits for the pipeline.
func (e *e2e) rerunWith(lectureID, path, choice string) {
	e.t.Helper()
	status, body := e.doRaw(http.MethodPost, path, api.ModelChoiceRequest{ModelChoice: choice})
	if status != http.StatusOK {
		e.t.Fatalf("rerun with %s: HTTP %d %s", choice, status, body)
	}
	e.waitFor("rerun with "+choice, func() bool { return allDone(e.lecture(lectureID)) }, lectureID)
}

// waitDetected waits for the first device detection of the app.
func (e *e2e) waitDetected() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !e.app.Devices.Detected() {
		if time.Now().After(deadline) {
			e.t.Fatal("first device detection did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 3b Gate round 1, item 1: a machine without NVIDIA and without .venv-ov,
// legacy settings device = cuda (the default), still transcribes — on the
// built-in fw:cpu row, created enabled by the first detection.
func TestV3bCPUOnlyMachineTranscribes(t *testing.T) {
	e := newE2EDevices(t, nil, func(o *devices.RegistryOptions) {
		o.CUDAName = func(context.Context) (string, error) { return "", errors.New("nvidia-smi: not found") }
		o.Probe = func(context.Context) (*devices.OVProbe, error) {
			return nil, model.Errorf(devices.CodeOVPythonMissing, map[string]any{"path": ".venv-ov"}, errors.New("python_openvino not found"))
		}
	})
	if a := e.app.Settings.ASR(); a.Device != "cuda" {
		t.Fatalf("legacy device = %q, want the default cuda", a.Device)
	}
	e.waitDetected()
	rows := e.app.Devices.List()
	if len(rows) != 1 || rows[0].ID != model.DeviceFWCPU || !rows[0].Enabled {
		t.Fatalf("rows on a CPU-only machine: %+v", rows)
	}
	_, lectureID := e.runPipeline()
	if s := e.asrSnapshot(lectureID); s.DeviceID != model.DeviceFWCPU || !s.Auto || s.Backend != model.BackendFasterWhisper {
		t.Fatalf("asr ran with %+v", s)
	}
}

// 3b Gate round 1, item 2: every device disabled → the launch writes an
// auto snapshot of the fallback; the job waits for a device and, once one
// is enabled, starts and is bound to it. Also item 1: with NVIDIA, fw:cpu
// exists and is disabled by default.
func TestV3bLaunchWithoutDeviceBindsLater(t *testing.T) {
	e := newE2E(t)
	ctx := context.Background()
	e.waitDetected()
	cpu, ok := e.app.Devices.Get(model.DeviceFWCPU)
	if !ok || cpu.Enabled {
		t.Fatalf("fw:cpu with NVIDIA present: %+v (ok %v)", cpu, ok)
	}
	off, on := false, true
	for _, d := range e.app.Devices.List() {
		if _, err := e.app.Devices.Update(ctx, d.ID, model.ASRDevicePatch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
	}

	var c api.Course
	e.postJSON("/api/courses", map[string]string{"name": "No device"}, &c)
	var l api.Lecture
	e.upload("/api/courses/"+c.ID+"/lectures", map[string]string{"title": "Week 1"}, "week1.wav", sampleWAV(t, e.cfg.FFmpeg, e.dir), &l)
	e.launch(l.ID)
	want := model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "small",
		Precision: model.PrecisionAuto, Language: e.app.Settings.ASR().Language, Auto: true}
	if s := e.asrSnapshot(l.ID); !model.EqualASRSnapshot(s, want) {
		t.Fatalf("launch snapshot without a device: %+v, want %+v", s, want)
	}
	e.waitFor("asr waits for a device", func() bool {
		j, err := e.app.Store.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageASR)
		return err == nil && j.Status == model.StatusQueued && e.app.Sched.WaitingFor(j) == model.WaitNoASRDevice
	}, l.ID)

	if _, err := e.app.Devices.Update(ctx, model.DeviceCUDA0, model.ASRDevicePatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("pipeline after enabling cuda:0", func() bool { return allDone(e.lecture(l.ID)) }, l.ID)
	if s := e.asrSnapshot(l.ID); s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Model != "large-v3" {
		t.Fatalf("asr ran with %+v", s)
	}
}
