// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
)

// SPEC-v2 I6-I10 in the mock (devices.go): the device endpoints follow the
// frozen types and the codes of the real handlers (internal/api/types_intel.go,
// .claude/v3b-brief.md section 5).

func fastSelfTest(t *testing.T) {
	t.Helper()
	old := selftestScale
	selftestScale = 0.004 // a whole test in about 0.1 s
	t.Cleanup(func() { selftestScale = old })
}

func deviceByID(vs []api.ASRDeviceView, id string) *api.ASRDeviceView {
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i]
		}
	}
	return nil
}

// wantCode checks an error body's code and that the code is in one of the
// two catalogs with the params it sends.
func wantCode(t *testing.T, method, url string, body any, status int, code string) model.ErrorInfo {
	t.Helper()
	got, b := rawCall(t, method, url, body)
	var e api.ErrorBody
	if got != status || json.Unmarshal(b, &e) != nil || e.Error.Code != code {
		t.Fatalf("%s %s: %d %s, want %d with code %s", method, url, got, b, status, code)
	}
	cat := map[string]struct {
		Params []string `json:"params"`
	}{}
	for _, f := range []string{"error_codes.json", "error_codes_backend.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "api", f))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			var d struct {
				Params []string `json:"params"`
			}
			if json.Unmarshal(v, &d) == nil {
				cat[k] = d
			}
		}
	}
	def, ok := cat[code]
	if !ok {
		t.Errorf("code %s is in no catalog", code)
	}
	for k := range e.Error.Params {
		found := false
		for _, p := range def.Params {
			found = found || p == k
		}
		if !found {
			t.Errorf("%s: param %q is not a param of the code", code, k)
		}
	}
	return e.Error
}

func TestMockDevicesViews(t *testing.T) {
	srv := testServer(t)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices", nil, http.StatusOK, &vs)
	ids := []string{}
	for _, v := range vs {
		ids = append(ids, v.ID)
		if v.Running == nil || v.Options.Models == nil || v.Options.Precisions == nil {
			t.Errorf("%s: nil list %+v", v.ID, v)
		}
		if v.Kind != model.DeviceKind(v.ID) || v.Backend != model.DeviceBackend(v.ID) {
			t.Errorf("%s: kind %q backend %q", v.ID, v.Kind, v.Backend)
		}
		if (v.Backend == model.BackendOpenVINO) != (v.ModelReady != nil && v.CompileCached != nil) {
			t.Errorf("%s: model_ready / compile_cached only for OpenVINO: %v %v", v.ID, v.ModelReady, v.CompileCached)
		}
		if v.SelfTest.DeviceID != v.ID || v.SelfTest.State != sched.DeviceTestIdle || v.SelfTestBlocked != nil {
			t.Errorf("%s: selftest %+v blocked %+v", v.ID, v.SelfTest, v.SelfTestBlocked)
		}
	}
	// gate round 1: fw:cpu always has a row (priority 10, off while faster devices exist)
	if want := []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceFWCPU, model.DeviceOVCPU}; len(ids) != 5 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] || ids[3] != want[3] || ids[4] != want[4] {
		t.Fatalf("order %v", ids)
	}
	cuda, gpu, npu, fwcpu, cpu := &vs[0], &vs[1], &vs[2], &vs[3], &vs[4]
	if cuda.New || !npu.New || gpu.New || cpu.New || fwcpu.New {
		t.Errorf("new: cuda %v gpu %v npu %v cpu %v fw:cpu %v", cuda.New, gpu.New, npu.New, cpu.New, fwcpu.New)
	}
	if fwcpu.Enabled || !fwcpu.Present || fwcpu.Priority != 10 || fwcpu.Backend != model.BackendFasterWhisper || fwcpu.ModelReady != nil {
		t.Errorf("fw:cpu %+v", fwcpu)
	}
	if cuda.SlotsUsed != 1 || len(cuda.Running) != 1 || cuda.Running[0].LectureID != lecID(c3631, 7) {
		t.Errorf("cuda runs %+v (slots %d)", cuda.Running, cuda.SlotsUsed)
	}
	if !*npu.ModelReady || *npu.CompileCached || !*gpu.CompileCached || cpu.Enabled {
		t.Errorf("npu ready %v cached %v, gpu cached %v, cpu enabled %v", *npu.ModelReady, *npu.CompileCached, *gpu.CompileCached, cpu.Enabled)
	}
	if gpu.Benchmark == nil || gpu.Benchmark.Pick == nil || gpu.Benchmark.Pick.Model != "large-v3-turbo" || gpu.Benchmark.Pick.Precision != model.PrecisionFP16 || gpu.Benchmark.AppliedAt == nil {
		t.Errorf("gpu benchmark %+v", gpu.Benchmark)
	}
	if r := npu.Options.Recommended; r == nil || r.Model != "large-v3-turbo" || r.Precision != model.PrecisionFP16 {
		t.Errorf("npu recommended %+v", r)
	}

	// the same list in the settings, the diagnostics; cards and power in the resources
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if len(s.Devices) != 5 {
		t.Errorf("settings devices %d", len(s.Devices))
	}
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system", nil, http.StatusOK, &sys)
	if len(sys.Devices) != 5 || sys.Power == nil || !sys.Power.BatteryAware || sys.OpenVINO == nil || !sys.OpenVINO.Available || len(sys.OpenVINO.Devices) != 3 || len(sys.OpenVINO.Ignored) != 1 {
		t.Errorf("system devices %d power %+v openvino %+v", len(sys.Devices), sys.Power, sys.OpenVINO)
	}
	names := map[string]string{}
	for _, c := range sys.Checks {
		names[c.Name] = c.Status
	}
	for _, n := range []string{"openvino_venv", "openvino", "openvino_devices", "npu_driver"} {
		if names[n] != "ok" {
			t.Errorf("check %s = %q", n, names[n])
		}
	}
	// power management (power-brief §5): every device row, the turned-off
	// ones too (formerly only the 3 enabled ones had a card); a laptop on
	// AC power, so the NVIDIA card is the one active device
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources", nil, http.StatusOK, &res)
	if len(res.Devices) != 5 || res.Power == nil || res.Power.OnBattery || !res.Power.HasBattery || !res.Power.Charging || res.Power.Source != resources.PowerSourceAC {
		t.Fatalf("resource cards %+v power %+v", res.Devices, res.Power)
	}
	if c := res.Devices[0]; c.ID != model.DeviceCUDA0 || c.UtilPct == nil || len(c.Tasks) != 1 || c.SlotsUsed != 1 || c.SlotsMax != 1 || c.BatteryMode || !c.Active || !c.Enabled {
		t.Errorf("cuda card %+v", c)
	}
	if c := res.Devices[2]; c.ID != model.DeviceOVNPU || c.UtilPct != nil || c.Tasks == nil || c.UtilHistory == nil || c.Active || !c.Untested || c.MemTotalMB == nil {
		t.Errorf("npu card (usage not readable) %+v", c)
	}
	for _, c := range res.Devices[3:] {
		if c.Enabled || c.Active || c.Kind != model.DeviceKindCPU {
			t.Errorf("turned-off CPU row %+v", c)
		}
	}
	if p := res.Policy; p == nil || !p.BatteryMachine || p.Mode != model.ASRDeviceModeAuto || p.Reason != "ac_auto" || len(p.Active) != 1 || p.Active[0] != model.DeviceCUDA0 || p.NoneReason != "" || p.Notices == nil {
		t.Errorf("policy %+v", res.Policy)
	}
	if y := res.System; y == nil || y.CPUPct == nil || y.MemUsedMB == nil || y.MemTotalMB == nil || y.CPUTempC == nil {
		t.Errorf("system %+v", res.System)
	}
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard", nil, http.StatusOK, &d)
	if len(d.DevicePrompts) != 1 || d.DevicePrompts[0].DeviceID != model.DeviceOVNPU || d.DevicePrompts[0].Label != labelNPU || d.DevicePrompts[0].Kind != model.DeviceKindNPU {
		t.Fatalf("prompts %+v", d.DevicePrompts)
	}
	code, _ := rawCall(t, "POST", srv.URL+"/api/asr/devices/ov:NPU/prompt/dismiss", nil)
	d = api.Dashboard{}
	doJSON(t, "GET", srv.URL+"/api/dashboard", nil, http.StatusOK, &d)
	if code != http.StatusNoContent || len(d.DevicePrompts) != 0 {
		t.Errorf("dismiss: %d, prompts %+v", code, d.DevicePrompts)
	}
}

func TestMockDevicePatchOrderPause(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/asr/devices/"
	str := func(s string) *string { return &s }
	n := func(v int) *int { return &v }
	wantCode(t, "PATCH", u+"ov:NPU", api.ASRDevicePatchInput{Model: str("large-v2")}, 400, devices.CodeModelInvalid)
	wantCode(t, "PATCH", u+"ov:NPU", api.ASRDevicePatchInput{Precision: str("float16")}, 400, devices.CodePrecisionInvalid)
	wantCode(t, "PATCH", u+"cuda:0", api.ASRDevicePatchInput{Model: str("  ")}, 400, devices.CodeModelMissing)
	wantCode(t, "PATCH", u+"cuda:0", api.ASRDevicePatchInput{MaxParallel: n(9)}, 400, devices.CodeMaxParallelInvalid)
	wantCode(t, "PATCH", u+"cuda:9", api.ASRDevicePatchInput{}, 404, devices.CodeNotFound)

	var v api.ASRDeviceView
	doJSON(t, "PATCH", u+"ov:NPU", api.ASRDevicePatchInput{Precision: str(model.PrecisionINT8), MaxParallel: n(2)}, http.StatusOK, &v)
	if v.Precision != model.PrecisionINT8 || v.MaxParallel != 2 || v.Model != "large-v3-turbo" || !*v.ModelReady || *v.CompileCached {
		t.Errorf("patched %+v", v)
	}
	// faster-whisper keeps accepting any model name
	doJSON(t, "PATCH", u+"cuda:0", api.ASRDevicePatchInput{Model: str(" distil-large-v3 "), Enabled: func() *bool { b := false; return &b }()}, http.StatusOK, &v)
	if v.Model != "distil-large-v3" || v.Enabled {
		t.Errorf("cuda %+v", v)
	}

	wantCode(t, "PUT", u+"order", api.DeviceOrderInput{IDs: []string{"ov:NPU", "cuda:0"}}, 400, devices.CodeOrderInvalid)
	wantCode(t, "PUT", u+"order", api.DeviceOrderInput{IDs: []string{"ov:NPU", "ov:NPU", "cuda:0", "ov:CPU", "fw:cpu"}}, 400, devices.CodeOrderInvalid)
	var vs []api.ASRDeviceView
	doJSON(t, "PUT", u+"order", api.DeviceOrderInput{IDs: []string{"ov:NPU", "cuda:0", "ov:CPU", "ov:GPU", "fw:cpu"}}, http.StatusOK, &vs)
	if vs[0].ID != "ov:NPU" || vs[1].ID != "cuda:0" || vs[2].ID != "ov:CPU" || vs[3].ID != "ov:GPU" || vs[4].ID != "fw:cpu" || !(vs[0].Priority > vs[1].Priority && vs[1].Priority > vs[2].Priority && vs[2].Priority > vs[3].Priority && vs[3].Priority > vs[4].Priority) {
		t.Errorf("order %+v", vs)
	}

	doJSON(t, "POST", u+"ov:GPU/pause", api.DevicePauseInput{Paused: true}, http.StatusOK, &v)
	if !v.Paused {
		t.Errorf("not paused %+v", v)
	}
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources", nil, http.StatusOK, &res)
	for _, c := range res.Devices {
		if c.ID == "ov:GPU" && !c.Paused {
			t.Errorf("card not paused %+v", c)
		}
		if c.ID == "cuda:0" && (c.Enabled || c.Active) {
			t.Errorf("a turned-off device is active: %+v", c) // power management: listed, not active
		}
	}
}

func TestMockSelfTest(t *testing.T) {
	fastSelfTest(t)
	srv := testServer(t)
	// power management (devices.SelfTest): the default data is a laptop and
	// a test starts only while no other device is active, so the lecture
	// transcribing on the NVIDIA card is cancelled first (the wait itself:
	// TestMockSelfTestWaitsForActiveDevice)
	var l api.Lecture
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecID(c3631, 7)+"/stages/asr/cancel", nil, http.StatusOK, &l)
	u := srv.URL + "/api/asr/devices/ov:NPU/selftest"
	var st api.SelfTestView
	doJSON(t, "POST", u, nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestRunning || st.Phase != sched.DeviceTestPhaseClip || st.StartedAt == nil || st.Steps != 2 {
		t.Fatalf("started %+v", st)
	}
	wantCode(t, "POST", u, nil, http.StatusConflict, sched.CodeSelfTestRunning)
	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for st.State == sched.DeviceTestRunning && time.Now().Before(deadline) {
		seen[st.Phase] = true
		if st.Progress < 0 || st.Progress > 1 {
			t.Fatalf("progress %v", st.Progress)
		}
		time.Sleep(2 * time.Millisecond)
		doJSON(t, "GET", u, nil, http.StatusOK, &st)
	}
	// the NPU has the model converted (fp16) but not compiled; int8 is converted too
	if st.State != sched.DeviceTestDone || !seen[sched.DeviceTestPhaseCompiling] || !seen[sched.DeviceTestPhaseTranscribing] || seen[sched.DeviceTestPhasePreparing] {
		t.Fatalf("ended %+v, phases seen %v", st, seen)
	}
	b := st.Result
	if b == nil || b.Status != model.BenchmarkDone || len(b.Results) != 2 || b.Pick == nil || b.Pick.Rule != model.PickRuleBest || b.Audio.ReferenceWords == 0 || b.Results[0].WER == nil || !b.Results[0].FirstCompile {
		t.Fatalf("result %+v", b)
	}
	var v api.ASRDeviceView
	doJSON(t, "POST", u+"/apply", api.SelfTestApplyInput{Model: "large-v3-turbo", Precision: model.PrecisionINT8}, http.StatusOK, &v)
	if v.Precision != model.PrecisionINT8 || v.Benchmark == nil || v.Benchmark.AppliedAt == nil || v.New || !*v.CompileCached {
		t.Errorf("applied %+v", v)
	}
	wantCode(t, "POST", u+"/apply", api.SelfTestApplyInput{Model: "small", Precision: model.PrecisionFP16}, 400, devices.CodeApplyNotTested)

	// cancel: the test ends at once, the slot is free again (power
	// management: a laptop never tests a CPU device (devices.SelfTest), so
	// the Intel GPU, whose test of two days ago must stay)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices", nil, http.StatusOK, &vs)
	prev := deviceByID(vs, model.DeviceOVGPU)
	if prev == nil || prev.Benchmark == nil {
		t.Fatalf("ov:GPU before %+v", prev)
	}
	g := srv.URL + "/api/asr/devices/ov:GPU/selftest"
	doJSON(t, "POST", g, nil, http.StatusAccepted, &st)
	doJSON(t, "DELETE", g, nil, http.StatusOK, &st)
	if st.State != sched.DeviceTestCancelled || st.Result == nil || st.Result.Status != model.BenchmarkCancelled || st.Result.Results == nil {
		t.Errorf("cancelled %+v", st)
	}
	doJSON(t, "DELETE", g, nil, http.StatusOK, &st) // idempotent
	if st.State != sched.DeviceTestCancelled {
		t.Errorf("second cancel %+v", st)
	}
	// gate round 1: cancelled before any candidate finished, so nothing is
	// stored (the device keeps its last benchmark)
	doJSON(t, "GET", srv.URL+"/api/asr/devices", nil, http.StatusOK, &vs)
	if d := deviceByID(vs, model.DeviceOVGPU); d == nil || d.Benchmark == nil || d.Benchmark.StartedAt != prev.Benchmark.StartedAt || d.SelfTest.Result == nil || len(d.SelfTest.Result.Results) != 0 {
		t.Errorf("empty cancel stored: %+v", d)
	}
}

func TestMockSelfTestNoAudio(t *testing.T) {
	// SPEC-v3 V4.3: blocked only in a build without the bundled sample.
	*noSampleFlag = true
	t.Cleanup(func() { *noSampleFlag = false })
	srv := testServer(t)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=empty", nil, http.StatusOK, &vs)
	for _, v := range vs {
		if v.SelfTestBlocked == nil || v.SelfTestBlocked.Code != stages.CodeSelfTestNoAudio {
			t.Errorf("%s: blocked %+v", v.ID, v.SelfTestBlocked)
		}
	}
	wantCode(t, "POST", srv.URL+"/api/asr/devices/ov:NPU/selftest?mock=empty", nil, http.StatusConflict, stages.CodeSelfTestNoAudio)
}

// SPEC-v3 V4.3: without a repaired lecture the self-test uses the bundled
// sample (whole, source "bundled").
func TestMockSelfTestBundledSample(t *testing.T) {
	srv := testServer(t)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=empty", nil, http.StatusOK, &vs)
	for _, v := range vs {
		if v.SelfTestBlocked != nil && v.SelfTestBlocked.Code == stages.CodeSelfTestNoAudio {
			t.Errorf("%s: blocked %+v", v.ID, v.SelfTestBlocked)
		}
	}
	var st api.SelfTestView
	doJSON(t, "POST", srv.URL+"/api/asr/devices/ov:NPU/selftest?mock=empty", nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestRunning {
		t.Errorf("started %+v", st)
	}
}

// PUT /api/settings/asr merges into the current settings (lead ruling 15).
func TestMockASRMerge(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/settings/asr"
	var s api.Settings
	doJSON(t, "PUT", u, map[string]any{"language": "zh", "multi_device": false}, http.StatusOK, &s)
	a := s.ASR
	if a.Language != "zh" || a.MultiDevice || !a.BatteryAware || a.MemReserveMB != 2048 || a.DeviceMode != model.ASRDeviceModeAuto || a.Model != "large-v2" || a.MaxParallel != 1 {
		t.Errorf("merged %+v", a)
	}
	wantCode(t, "PUT", u, map[string]any{"device_mode": "ov:XPU"}, 400, settings.CodeDeviceModeInvalid)
	wantCode(t, "PUT", u, map[string]any{"mem_reserve_mb": 70000}, 400, settings.CodeMemReserveInvalid)
	// power management: a 3b single device id is read as manual [id]
	doJSON(t, "PUT", u, map[string]any{"device_mode": "ov:NPU", "mem_reserve_mb": 4096}, http.StatusOK, &s)
	if s.ASR.DeviceMode != model.ASRDeviceModeManual || len(s.ASR.ManualDevices) != 1 || s.ASR.ManualDevices[0] != "ov:NPU" || s.ASR.MemReserveMB != 4096 || s.ASR.Language != "zh" {
		t.Errorf("device mode %+v", s.ASR)
	}
	// the asr model a rerun would use now names the chosen device
	var nm api.NeedsModelChoice
	code, b := rawCall(t, "POST", srv.URL+"/api/lectures/"+lecID(c3631, 1)+"/stages/asr/rerun", nil)
	if code != http.StatusConflict || json.Unmarshal(b, &nm) != nil || len(nm.Diff) == 0 || nm.Diff[0].Stage != model.StageASR ||
		nm.Diff[0].Current.Label != asrLabelOf(labelNPU, "large-v3-turbo", model.PrecisionFP16, "zh") {
		t.Fatalf("rerun: %d %s", code, b)
	}
}

func TestMockBatteryAndNoOpenVINO(t *testing.T) {
	srv := testServer(t)
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock=battery", nil, http.StatusOK, &res)
	if res.Power == nil || !res.Power.OnBattery || res.Power.BatteryPct == nil {
		t.Fatalf("battery power %+v", res.Power)
	}
	hold := map[string]bool{}
	for _, c := range res.Devices {
		if !c.BatteryMode {
			t.Errorf("%s: no battery mode", c.ID)
		}
		hold[c.ID] = c.BatteryHold
	}
	if !hold[model.DeviceCUDA0] || !hold[model.DeviceOVGPU] || hold[model.DeviceOVNPU] {
		t.Errorf("battery holds %v (auto: only the NPU)", hold)
	}
	// the default world is not on battery
	res = resources.Snapshot{}
	doJSON(t, "GET", srv.URL+"/api/resources", nil, http.StatusOK, &res)
	if res.Power == nil || res.Power.OnBattery {
		t.Errorf("default power %+v", res.Power)
	}

	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system?mock=noov", nil, http.StatusOK, &sys)
	if len(sys.Devices) != 2 || sys.Devices[0].ID != model.DeviceCUDA0 || sys.Devices[1].ID != model.DeviceFWCPU || sys.OpenVINO == nil || sys.OpenVINO.Available ||
		sys.OpenVINO.ErrorInfo == nil || sys.OpenVINO.ErrorInfo.Code != devices.CodeOVPythonMissing || sys.OpenVINO.Devices == nil {
		t.Fatalf("noov system devices %+v openvino %+v", sys.Devices, sys.OpenVINO)
	}
	codes := catalogCodes(t)
	for _, c := range sys.Checks {
		switch c.Name {
		case "openvino_venv", "openvino", "openvino_devices", "npu_driver":
			if c.Status != "warn" {
				t.Errorf("%s: %s (OpenVINO checks are at most warn)", c.Name, c.Status)
			}
			wantCatalogCode(t, c.Name, c.DetailInfo, codes)
			if c.HintInfo != nil {
				wantCatalogCode(t, c.Name+" hint", c.HintInfo, codes)
			}
		}
	}
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=noov", nil, http.StatusOK, &vs)
	if len(vs) != 2 {
		t.Errorf("noov devices %+v", vs)
	}
	// on battery, the CPU devices start nothing (power management: a
	// machine with a battery never transcribes on the CPU; battery_hold =
	// enabled, present and not the battery device, so the turned-off fw:cpu
	// has none until it is turned on)
	vs = nil
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=battery", nil, http.StatusOK, &vs)
	if d := deviceByID(vs, model.DeviceFWCPU); d == nil || d.BatteryHold {
		t.Errorf("battery fw:cpu (turned off) %+v", d)
	}
	on := true
	var v api.ASRDeviceView
	doJSON(t, "PATCH", srv.URL+"/api/asr/devices/fw:cpu?mock=battery", api.ASRDevicePatchInput{Enabled: &on}, http.StatusOK, &v)
	if !v.BatteryHold {
		t.Errorf("battery fw:cpu (turned on) %+v", v)
	}
}

// Scenario cpuonly: no NVIDIA card, no OpenVINO environment, fw:cpu on.
func TestMockCPUOnly(t *testing.T) {
	srv := testServer(t)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=cpuonly", nil, http.StatusOK, &vs)
	if len(vs) != 1 || vs[0].ID != model.DeviceFWCPU || !vs[0].Enabled || !vs[0].Present {
		t.Fatalf("cpuonly devices %+v", vs)
	}
	var res resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock=cpuonly", nil, http.StatusOK, &res)
	if len(res.Devices) != 1 || res.Devices[0].ID != model.DeviceFWCPU || res.GPU.Available || res.Devices[0].UtilPct != nil {
		t.Errorf("cpuonly cards %+v gpu %+v", res.Devices, res.GPU)
	}
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system?mock=cpuonly", nil, http.StatusOK, &sys)
	codes := catalogCodes(t)
	for _, c := range sys.Checks {
		switch c.Name {
		case "gpu", "cuda", "openvino_venv", "openvino", "openvino_devices", "npu_driver":
			if c.Status != "warn" {
				t.Errorf("%s: %s (cuda:0 does not exist: at most warn)", c.Name, c.Status)
			}
			wantCatalogCode(t, c.Name, c.DetailInfo, codes)
		}
	}
	// power management: a 3b single device id is read as manual [id], and a
	// manual list never holds a CPU device (formerly fw:cpu was accepted)
	wantCode(t, "PUT", srv.URL+"/api/settings/asr?mock=cpuonly", map[string]any{"device_mode": model.DeviceFWCPU}, 400, settings.CodeManualDevicesCPU)
	wantCode(t, "PUT", srv.URL+"/api/settings/asr?mock=cpuonly", map[string]any{"device_mode": model.DeviceCUDA0}, 400, settings.CodeDeviceModeInvalid)
	// a desktop: the 3b pool with the CPU fallback (Q1)
	if p := res.Policy; p == nil || p.BatteryMachine || p.Reason != "desktop" || len(p.Active) != 1 || p.Active[0] != model.DeviceFWCPU || res.Power.HasBattery {
		t.Errorf("cpuonly policy %+v power %+v", res.Policy, res.Power)
	}
}
