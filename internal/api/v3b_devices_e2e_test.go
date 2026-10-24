// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/binary"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
)

// The device endpoints against the real registry (task B) and scheduler
// (task C), wired as cmd wires them, with a fake probe, a fake nvidia-smi
// name and a fake self-test run (§14: no worker, no model).

type realDevEnv struct {
	*env
	reg *devices.Registry
	mu  sync.Mutex
	pw  devices.Power
	ran []string // devices the fake self-test ran on
}

func newRealDevEnv(t *testing.T) *realDevEnv {
	t.Helper()
	ctx := context.Background()
	rd := &realDevEnv{}
	probe := func(context.Context) (*devices.OVProbe, error) {
		return &devices.OVProbe{OpenVINO: "2026.4.1", GenAI: "2026.4.1.0", CanConvert: true,
			Devices: []devices.OVDevice{
				{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: igpuName, Vendor: "0x8086"},
				{Name: "NPU", Kind: model.DeviceKindNPU, FullName: npuName, Vendor: "0x8086", DriverVersion: "32.0.100.4841"},
			},
			Ignored: []devices.OVDevice{}}, nil
	}
	test := func(ctx context.Context, dev model.ASRDevice, env sched.DeviceTestEnv) (model.DeviceBenchmark, error) {
		rd.mu.Lock()
		rd.ran = append(rd.ran, dev.ID)
		rd.mu.Unlock()
		if err := env.BeforeCandidate(ctx); err != nil {
			return model.DeviceBenchmark{}, err
		}
		env.Report(sched.DeviceTestProgress{Phase: sched.DeviceTestPhaseTranscribing, Step: 1, Steps: 1,
			Candidate: devices.Candidate{Model: "large-v3-turbo", Precision: "int8"}, Progress: 0.5})
		wer := 0.06
		now := time.Now().UTC().Format(time.RFC3339)
		return model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone, StartedAt: now, FinishedAt: now,
			Audio:   model.BenchmarkAudio{Source: model.BenchmarkSourceLec, StartMS: 0, EndMS: 120000},
			Results: []model.BenchmarkResult{{Model: "large-v3-turbo", Precision: "int8", OK: true, TranscribeS: 12, RTF: 0.1, WER: &wer}},
			Pick:    &model.BenchmarkPick{Model: "large-v3-turbo", Precision: "int8", Rule: model.PickRuleBest}}, nil
	}
	ec := envConfig{sched: func(o *sched.Options) {
		fastScan()(o)
		rd.reg = devices.NewRegistry(o.Store, o.Config, o.DataDir, devices.RegistryOptions{
			Probe:    probe,
			CUDAName: func(context.Context) (string, error) { return nvidiaName, nil },
		})
		if err := rd.reg.Load(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := rd.reg.MigrateLegacy(ctx, o.Settings.ASR()); err != nil {
			t.Fatal(err)
		}
		o.Settings.SetDeviceSource(rd.reg)
		o.Devices = rd.reg
		o.Power = func() devices.Power { rd.mu.Lock(); defer rd.mu.Unlock(); return rd.pw }
		o.MemAvailMB = func() (int, error) { return 32000, nil }
		o.DeviceTest = test
	}, server: func(s *Server) {
		s.dev, s.dsc = rd.reg, s.sc
	}, resources: true, resDeps: func(d *resources.Deps) {
		// cmd's wiring of the device cards (rd.env is set before any sample)
		d.ASRDevices = func() []model.ASRDevice { return rd.reg.List() }
		d.DeviceStatuses = func() []sched.DeviceStatus { return rd.sc.DeviceStatuses() }
		d.Detection = func() devices.Detection { return rd.reg.LastDetection() }
		d.Power = func() devices.Power { return rd.sc.Power() }
		// the upper area's readings: fixed (the readers are tested in
		// package resources)
		d.SystemStats = func() resources.System {
			used, total := 14210, 32456
			return resources.System{MemUsedMB: &used, MemTotalMB: &total}
		}
	}}
	rd.env = newEnvWith(t, ec)
	rd.reg.OnChange(func() { rd.hub.Changed("devices", ""); rd.sc.Kick() })
	if _, _, err := rd.reg.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	return rd
}

func (rd *realDevEnv) start() {
	rd.t.Helper()
	if err := rd.sc.Start(context.Background()); err != nil {
		rd.t.Fatal(err)
	}
	rd.t.Cleanup(rd.sc.Stop)
}

func TestDevicesWithRealRegistry(t *testing.T) {
	rd := newRealDevEnv(t)
	var vs []ASRDeviceView
	rd.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
	// fw:cpu always has a row (3b Gate round 1, item 1): disabled, since
	// accelerators were found; listed last (priority 10).
	if len(vs) != 4 || vs[0].ID != model.DeviceCUDA0 || vs[1].ID != model.DeviceOVGPU || vs[2].ID != model.DeviceOVNPU ||
		vs[3].ID != model.DeviceFWCPU || vs[3].Enabled || vs[3].New || !vs[3].Present {
		t.Fatalf("devices %+v", vs)
	}
	for _, v := range vs[:3] {
		if !v.Present || !v.Enabled || v.Running == nil {
			t.Errorf("%s: %+v", v.ID, v)
		}
	}
	if vs[0].Label != nvidiaName || vs[0].New || !vs[1].New || !vs[2].New || vs[2].Model != "large-v3-turbo" || vs[2].Precision != "fp16" {
		t.Fatalf("rows %+v", vs)
	}
	// No repaired lecture yet: the self-test is not available.
	if b := vs[1].SelfTestBlocked; b == nil || b.Code != stages.CodeSelfTestNoAudio {
		t.Fatalf("blocked %+v", b)
	}
	if info := rd.callErrInfo("POST", "/api/asr/devices/ov:GPU/selftest", nil, http.StatusConflict); info.Code != stages.CodeSelfTestNoAudio {
		t.Fatalf("self-test without audio %+v", info)
	}

	// PATCH and its validation (the registry's codes).
	var v ASRDeviceView
	rd.call("PATCH", "/api/asr/devices/ov:GPU", map[string]any{"precision": "int8"}, http.StatusOK, &v)
	if v.Precision != "int8" {
		t.Fatalf("patched %+v", v)
	}
	if info := rd.callErrInfo("PATCH", "/api/asr/devices/ov:GPU", map[string]any{"precision": "float16"}, http.StatusBadRequest); info.Code != devices.CodePrecisionInvalid {
		t.Fatalf("bad precision %+v", info)
	}
	if info := rd.callErrInfo("PATCH", "/api/asr/devices/ov:GPU", map[string]any{"model": "large-v2"}, http.StatusBadRequest); info.Code != devices.CodeModelInvalid {
		t.Fatalf("bad OpenVINO model %+v", info)
	}
	if info := rd.callErrInfo("PATCH", "/api/asr/devices/ov:XPU", map[string]any{"enabled": false}, http.StatusNotFound); info.Code != devices.CodeNotFound || info.Params["device"] != "ov:XPU" {
		t.Fatalf("unknown %+v", info)
	}
	rd.call("PUT", "/api/asr/devices/order", DeviceOrderInput{IDs: []string{model.DeviceOVNPU, model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceFWCPU}}, http.StatusOK, &vs)
	if vs[0].ID != model.DeviceOVNPU || vs[2].ID != model.DeviceOVGPU {
		t.Fatalf("order %+v", vs)
	}
	if info := rd.callErrInfo("PUT", "/api/asr/devices/order", DeviceOrderInput{IDs: []string{model.DeviceOVNPU}}, http.StatusBadRequest); info.Code != devices.CodeOrderInvalid {
		t.Fatalf("bad order %+v", info)
	}

	// Pause one device (I8); the scheduler reports it.
	rd.call("POST", "/api/asr/devices/ov:NPU/pause", DevicePauseInput{Paused: true}, http.StatusOK, &v)
	if !v.Paused || !rd.sc.DevicePaused(model.DeviceOVNPU) {
		t.Fatalf("pause %+v", v)
	}
	rd.call("POST", "/api/asr/devices/ov:NPU/pause", DevicePauseInput{Paused: false}, http.StatusOK, &v)
	if v.Paused {
		t.Fatalf("resume %+v", v)
	}

	// Settings: device_mode is validated against the rows. Power management
	// (power-brief §3.2): an older client's single device id is stored as
	// manual [that id].
	var sv Settings
	rd.call("PUT", "/api/settings/asr", map[string]any{"device_mode": model.DeviceOVNPU}, http.StatusOK, &sv)
	if sv.ASR.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(sv.ASR.ManualDevices, []string{model.DeviceOVNPU}) ||
		len(sv.Devices) != 4 /* with fw:cpu */ || !sv.ASR.MultiDevice || !sv.ASR.BatteryAware {
		t.Fatalf("settings %+v", sv.ASR)
	}
	// The launch snapshot of manual mode is an auto snapshot of the list's
	// first usable device (power-brief §3.2, Registry.CurrentASRSnapshot;
	// lead ruling 9.2: A4.3 compares this effective configuration).
	npuRow, _ := rd.reg.Get(model.DeviceOVNPU)
	if got, want := rd.set.CurrentASR(), model.MarshalSnapshot(model.ASRDeviceSnapshot(npuRow, sv.ASR.Language, true)); got != want {
		t.Errorf("manual [ov:NPU] launch snapshot %s, want %s", got, want)
	}
	if info := rd.callErrInfo("PUT", "/api/settings/asr", map[string]any{"device_mode": "ov:XPU"}, http.StatusBadRequest); info.Code != settings.CodeDeviceModeInvalid {
		t.Fatalf("bad device mode %+v", info)
	}

	// Dashboard prompts: the two new Intel devices (cuda:0 was migrated).
	var d Dashboard
	rd.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	if len(d.DevicePrompts) != 2 {
		t.Fatalf("prompts %+v", d.DevicePrompts)
	}
	if res, _ := rd.do("POST", "/api/asr/devices/ov:NPU/prompt/dismiss", nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss %d", res.StatusCode)
	}
	var d2 Dashboard
	rd.call("GET", "/api/dashboard", nil, http.StatusOK, &d2)
	if len(d2.DevicePrompts) != 1 || d2.DevicePrompts[0].DeviceID != model.DeviceOVGPU {
		t.Fatalf("prompts after dismiss %+v", d2.DevicePrompts)
	}

	// System: the devices table, power and the OpenVINO environment.
	rd.mu.Lock()
	rd.pw = devices.Power{Known: true, OnBattery: true}
	rd.mu.Unlock()
	var sys System
	rd.call("GET", "/api/system", nil, http.StatusOK, &sys)
	if len(sys.Devices) != 4 /* with fw:cpu */ || sys.Power == nil || !sys.Power.OnBattery || sys.OpenVINO == nil || !sys.OpenVINO.Available || len(sys.OpenVINO.Devices) != 2 {
		t.Fatalf("system %+v %+v %+v", sys.Devices, sys.Power, sys.OpenVINO)
	}
	if c := checksByName(sys)["npu_driver"]; c.Status != statusOK {
		t.Fatalf("npu check %+v", c)
	}

	// The resource bar: battery mode on every card. Power management (task
	// P4): every row is listed, priority order, the disabled ov:GPU and
	// fw:cpu as standby rows; without a power status the enabled rows are
	// the active cards.
	rd.call("PATCH", "/api/asr/devices/ov:GPU", map[string]any{"enabled": false}, http.StatusOK, nil)
	var snap resources.Snapshot
	rd.call("GET", "/api/resources", nil, http.StatusOK, &snap)
	if len(snap.Devices) != 4 || snap.Devices[0].ID != model.DeviceOVNPU || snap.Devices[1].ID != model.DeviceCUDA0 ||
		snap.Devices[2].ID != model.DeviceOVGPU || snap.Devices[3].ID != model.DeviceFWCPU ||
		snap.Power == nil || !snap.Power.OnBattery || !snap.Power.BatteryAware || snap.Power.Source != resources.PowerSourceBattery {
		t.Fatalf("resources %+v %+v", snap.Devices, snap.Power)
	}
	for i, c := range snap.Devices {
		if c.Enabled != (i < 2) || c.Active != c.Enabled || !c.Untested {
			t.Errorf("%s: enabled %v active %v untested %v", c.ID, c.Enabled, c.Active, c.Untested)
		}
	}
	if snap.System == nil || snap.System.MemTotalMB == nil || *snap.System.MemTotalMB != 32456 || snap.Policy != nil {
		t.Fatalf("system %+v policy %+v", snap.System, snap.Policy)
	}
	for _, c := range snap.Devices {
		if !c.BatteryMode || !c.Present || c.SlotsMax != 1 || c.Tasks == nil || c.UtilHistory == nil {
			t.Errorf("card %+v", c)
		}
		if c.Kind == model.DeviceKindNPU && c.UtilPct != nil {
			t.Errorf("npu without a LUID has a utilisation: %+v", c)
		}
	}
}

// A self-test end to end through the API: start (202), run (the fake), the
// stored benchmark, "应用", and the workbench prompt disappears.
func TestSelfTestWithRealScheduler(t *testing.T) {
	rd := newRealDevEnv(t)
	c := rd.course("C")
	l := rd.lecture(c.ID, "Repaired")
	rd.allDone(l.ID)
	audio := filepath.Join(model.LectureDir(rd.cfg.DataDir, c.ID, l.ID), model.AudioFile)
	if err := os.MkdirAll(filepath.Dir(audio), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, tinyWAV(150), 0o644); err != nil {
		t.Fatal(err)
	}
	rd.start()
	var st SelfTestView
	rd.call("POST", "/api/asr/devices/ov:GPU/selftest", nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestWaiting && st.State != sched.DeviceTestRunning {
		t.Fatalf("started %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st = SelfTestView{}
		rd.call("GET", "/api/asr/devices/ov:GPU/selftest", nil, http.StatusOK, &st)
		if st.State == sched.DeviceTestDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("self-test did not finish: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Result == nil || st.Result.Pick == nil || st.FinishedAt == nil {
		t.Fatalf("done %+v", st)
	}
	var v ASRDeviceView
	rd.call("POST", "/api/asr/devices/ov:GPU/selftest/apply", SelfTestApplyInput{Model: "large-v3-turbo", Precision: "int8"}, http.StatusOK, &v)
	if v.Precision != "int8" || v.Benchmark == nil || v.Benchmark.AppliedAt == nil || v.New {
		t.Fatalf("applied %+v", v)
	}
	if info := rd.callErrInfo("POST", "/api/asr/devices/ov:GPU/selftest/apply", SelfTestApplyInput{Model: "large-v3", Precision: "fp16"}, http.StatusBadRequest); info.Code != devices.CodeApplyNotTested {
		t.Fatalf("apply untested %+v", info)
	}
	var d Dashboard
	rd.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	for _, p := range d.DevicePrompts {
		if p.DeviceID == model.DeviceOVGPU {
			t.Fatalf("tested device still prompted: %+v", d.DevicePrompts)
		}
	}
	rd.mu.Lock()
	ran := append([]string(nil), rd.ran...)
	rd.mu.Unlock()
	if len(ran) != 1 || ran[0] != model.DeviceOVGPU {
		t.Fatalf("self-test ran on %v", ran)
	}
	// Cancelling a finished test returns it as it is.
	st = SelfTestView{}
	rd.call("DELETE", "/api/asr/devices/ov:GPU/selftest", nil, http.StatusOK, &st)
	if st.State != sched.DeviceTestDone {
		t.Fatalf("cancel after done %+v", st)
	}
}

// tinyWAV is a PCM WAV header that reads as seconds long (a byte rate of
// 100: the fake self-test never decodes it, devices.FindClipSource only
// needs a lecture longer than the 2-minute clip).
func tinyWAV(seconds int) []byte {
	const rate = 100
	data := make([]byte, seconds*rate)
	b := make([]byte, 0, 44+len(data))
	le32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	le16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	b = append(b, "RIFF"...)
	le32(uint32(36 + len(data)))
	b = append(b, "WAVEfmt "...)
	le32(16)
	le16(1)        // PCM
	le16(1)        // mono
	le32(rate / 2) // samples per second
	le32(rate)     // byte rate
	le16(2)        // block align
	le16(16)       // bits per sample
	b = append(b, "data"...)
	le32(uint32(len(data)))
	return append(b, data...)
}
