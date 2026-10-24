// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"testing"
)

// Phase 3b freeze (SPEC-v2 I6, lead rulings 1 and 2): device ids, the I6
// snapshot, its normalization, comparison and pinning.

func TestDeviceIDHelpers(t *testing.T) {
	cases := []struct {
		id, backend, kind, ov, fw string
		prio                      int
	}{
		{DeviceCUDA0, BackendFasterWhisper, DeviceKindCUDA, "", "cuda", 300},
		{"cuda:1", BackendFasterWhisper, DeviceKindCUDA, "", "cuda", 300},
		{DeviceOVGPU, BackendOpenVINO, DeviceKindIGPU, "GPU", "", 200},
		{DeviceOVNPU, BackendOpenVINO, DeviceKindNPU, "NPU", "", 100},
		{DeviceOVCPU, BackendOpenVINO, DeviceKindCPU, "CPU", "", 0},
		{DeviceFWCPU, BackendFasterWhisper, DeviceKindCPU, "", "cpu", 10},
		{"rocm:0", "", "", "", "", 0},
		{"", "", "", "", "", 0},
	}
	for _, c := range cases {
		if got := DeviceBackend(c.id); got != c.backend {
			t.Errorf("DeviceBackend(%q) = %q, want %q", c.id, got, c.backend)
		}
		if got := DeviceKind(c.id); got != c.kind {
			t.Errorf("DeviceKind(%q) = %q, want %q", c.id, got, c.kind)
		}
		if got := OVDeviceName(c.id); got != c.ov {
			t.Errorf("OVDeviceName(%q) = %q, want %q", c.id, got, c.ov)
		}
		if got := FWDevice(c.id); got != c.fw {
			t.Errorf("FWDevice(%q) = %q, want %q", c.id, got, c.fw)
		}
		if got := DefaultDevicePriority(c.id); got != c.prio {
			t.Errorf("DefaultDevicePriority(%q) = %d, want %d", c.id, got, c.prio)
		}
	}
	for kind, id := range map[string]string{DeviceKindIGPU: DeviceOVGPU, DeviceKindNPU: DeviceOVNPU, DeviceKindCPU: DeviceOVCPU, DeviceKindCUDA: "", "x": ""} {
		if got := OVDeviceID(kind); got != id {
			t.Errorf("OVDeviceID(%q) = %q, want %q", kind, got, id)
		}
	}
	// The default order of I8: CUDA > Intel GPU > NPU > CPU.
	if !(DefaultDevicePriority(DeviceCUDA0) > DefaultDevicePriority(DeviceOVGPU) &&
		DefaultDevicePriority(DeviceOVGPU) > DefaultDevicePriority(DeviceOVNPU) &&
		DefaultDevicePriority(DeviceOVNPU) > DefaultDevicePriority(DeviceFWCPU) &&
		DefaultDevicePriority(DeviceFWCPU) > DefaultDevicePriority(DeviceOVCPU)) {
		t.Error("default priorities are not CUDA > iGPU > NPU > fw:cpu > ov:CPU")
	}
}

func TestValidPrecision(t *testing.T) {
	for _, p := range []string{"auto", "float16", "int8_float16"} {
		if !ValidPrecision(BackendFasterWhisper, p) || ValidPrecision(BackendOpenVINO, p) {
			t.Errorf("%q: faster-whisper only", p)
		}
	}
	for _, p := range []string{"fp16", "int8", "int4"} {
		if !ValidPrecision(BackendOpenVINO, p) || ValidPrecision(BackendFasterWhisper, p) {
			t.Errorf("%q: openvino only", p)
		}
	}
	for _, p := range []string{"", "FP16", "int8_float32"} {
		if ValidPrecision(BackendOpenVINO, p) || ValidPrecision(BackendFasterWhisper, p) {
			t.Errorf("%q accepted", p)
		}
	}
	if ValidPrecision("", "auto") {
		t.Error("unknown backend accepted")
	}
}

// A v1.1-style snapshot keeps its bytes: the new fields are omitted when
// empty, the old ones keep their order (settings' TestCurrentSnapshots
// compares this string).
func TestASRSnapshotLegacyBytes(t *testing.T) {
	legacy := ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"}
	if got, want := MarshalSnapshot(legacy), `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}`; got != want {
		t.Fatalf("legacy snapshot\n got %s\nwant %s", got, want)
	}
	dev := ASRDevice{ID: DeviceOVNPU, Backend: BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16"}
	if got, want := MarshalSnapshot(ASRDeviceSnapshot(dev, "en", true)),
		`{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en","auto":true}`; got != want {
		t.Fatalf("I6 snapshot\n got %s\nwant %s", got, want)
	}
	if got, want := MarshalSnapshot(ASRDeviceSnapshot(dev, "en", false)),
		`{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en"}`; got != want {
		t.Fatalf("fixed I6 snapshot\n got %s\nwant %s", got, want)
	}
	// Round trip.
	s, ok := ParseASRSnapshot(`{"device_id":"ov:GPU","backend":"openvino","model":"m","precision":"int8","language":"de","auto":true}`)
	if !ok || !EqualASRSnapshot(s, ASRSnapshot{DeviceID: DeviceOVGPU, Backend: BackendOpenVINO, Model: "m", Precision: "int8", Language: "de", Auto: true}) {
		t.Fatalf("parse: %+v %v", s, ok)
	}
}

func TestNormalizeASRSnapshot(t *testing.T) {
	cases := []struct {
		in, want ASRSnapshot
	}{
		{ // v1.1 cuda
			ASRSnapshot{Model: "large-v3", ComputeType: "auto", Device: "cuda", Language: "en"},
			ASRSnapshot{DeviceID: DeviceCUDA0, Backend: BackendFasterWhisper, Model: "large-v3", Precision: "auto", ComputeType: "auto", Device: "cuda", Language: "en"},
		},
		{ // v1.1 cpu
			ASRSnapshot{Model: "small", ComputeType: "int8_float16", Device: "cpu", Language: "en"},
			ASRSnapshot{DeviceID: DeviceFWCPU, Backend: BackendFasterWhisper, Model: "small", Precision: "int8_float16", ComputeType: "int8_float16", Device: "cpu", Language: "en"},
		},
		{ // v1.1 without device: the v1 default (cuda)
			ASRSnapshot{Model: "large-v3", ComputeType: "float16", Language: "en"},
			ASRSnapshot{DeviceID: DeviceCUDA0, Backend: BackendFasterWhisper, Model: "large-v3", Precision: "float16", ComputeType: "float16", Language: "en"},
		},
		{ // I6 snapshot: unchanged
			ASRSnapshot{DeviceID: DeviceOVGPU, Backend: BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16", Language: "en", Auto: true},
			ASRSnapshot{DeviceID: DeviceOVGPU, Backend: BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16", Language: "en", Auto: true},
		},
		{ // device id without backend
			ASRSnapshot{DeviceID: DeviceOVNPU, Model: "m", Precision: "int8", Language: "en"},
			ASRSnapshot{DeviceID: DeviceOVNPU, Backend: BackendOpenVINO, Model: "m", Precision: "int8", Language: "en"},
		},
	}
	for i, c := range cases {
		if got := NormalizeASRSnapshot(c.in); !EqualASRSnapshot(got, c.want) {
			t.Errorf("%d: got %+v\nwant %+v", i, got, c.want)
		}
		if got := NormalizeASRSnapshot(NormalizeASRSnapshot(c.in)); !EqualASRSnapshot(got, c.want) {
			t.Errorf("%d: not idempotent: %+v", i, got)
		}
	}
}

func TestSameSnapshotASRDevices(t *testing.T) {
	cuda := ASRDevice{ID: DeviceCUDA0, Backend: BackendFasterWhisper, Model: "large-v3", Precision: "auto"}
	gpu := ASRDevice{ID: DeviceOVGPU, Backend: BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16"}
	snap := func(d ASRDevice, lang string, auto bool) string {
		return MarshalSnapshot(ASRDeviceSnapshot(d, lang, auto))
	}
	legacy := `{"model":"large-v3","compute_type":"auto","device":"cuda","language":"en"}`
	legacyCPU := `{"model":"large-v3","compute_type":"auto","device":"cpu","language":"en"}`
	other := func(f func(*ASRDevice)) ASRDevice { d := gpu; f(&d); return d }
	cases := []struct {
		a, b string
		same bool
	}{
		// a v1.1 snapshot equals the cuda:0 row its settings migrated to
		{legacy, snap(cuda, "en", false), true},
		{legacy, snap(cuda, "en", true), true},
		{legacy, legacy, true},
		{legacy, legacyCPU, false},
		{legacyCPU, snap(ASRDevice{ID: DeviceFWCPU, Backend: BackendFasterWhisper, Model: "large-v3", Precision: "auto"}, "en", false), true},
		// the auto flag is not compared (lead ruling 2)
		{snap(gpu, "en", true), snap(gpu, "en", false), true},
		// device, backend, model, precision, language are
		{snap(gpu, "en", true), snap(cuda, "en", true), false},
		{snap(gpu, "en", false), snap(other(func(d *ASRDevice) { d.ID = DeviceOVNPU }), "en", false), false},
		{snap(gpu, "en", false), snap(other(func(d *ASRDevice) { d.Model = "small" }), "en", false), false},
		{snap(gpu, "en", false), snap(other(func(d *ASRDevice) { d.Precision = "int8" }), "en", false), false},
		{snap(gpu, "en", false), snap(gpu, "de", false), false},
		// a v1.1 snapshot with another compute type or model differs
		{legacy, `{"model":"large-v3","compute_type":"float16","device":"cuda","language":"en"}`, false},
		{legacy, `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}`, false},
		// empty and unparsable (unchanged rules)
		{"{}", "", true},
		{"{}", legacy, false},
		{"not json", "not json", false},
	}
	for _, c := range cases {
		if got := SameSnapshot(StageASR, c.a, c.b); got != c.same {
			t.Errorf("SameSnapshot(asr,\n  %s,\n  %s) = %v, want %v", c.a, c.b, got, c.same)
		}
		if got := SameSnapshot(StageASR, c.b, c.a); got != c.same {
			t.Errorf("SameSnapshot not symmetric for %s / %s", c.a, c.b)
		}
	}
}

func TestPinASRSnapshot(t *testing.T) {
	auto := `{"device_id":"ov:GPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en","auto":true}`
	if got, want := PinASRSnapshot(auto), `{"device_id":"ov:GPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en"}`; got != want {
		t.Fatalf("pin auto\n got %s\nwant %s", got, want)
	}
	if !SameSnapshot(StageASR, auto, PinASRSnapshot(auto)) {
		t.Fatal("a pinned snapshot is the same configuration")
	}
	// Everything else comes back byte for byte.
	for _, raw := range []string{
		`{"model":"large-v3","compute_type":"auto","device":"cuda","language":"en"}`,
		`{"device_id":"ov:NPU","backend":"openvino","model":"m","precision":"int8","language":"en"}`,
		`{"language": "en", "model": "x", "auto": false}`,
		"{}", "", "null", "garbage", `{"auto":true}`,
	} {
		if got := PinASRSnapshot(raw); got != raw {
			t.Errorf("PinASRSnapshot(%q) = %q, want unchanged", raw, got)
		}
	}
	// Backend Gate round 1, fix 3: the parts of the run that happened are
	// never kept, of an auto or a fixed snapshot.
	parts := `,"parts":[{"from_ms":0,"to_ms":1000,"device_id":"ov:GPU","model":"m","precision":"int8"},` +
		`{"from_ms":1000,"to_ms":2000,"device_id":"ov:NPU","model":"m","precision":"int8"}]`
	for _, c := range []struct{ raw, want string }{
		{`{"device_id":"ov:NPU","backend":"openvino","model":"m","precision":"int8","language":"en","auto":true` + parts + `}`,
			`{"device_id":"ov:NPU","backend":"openvino","model":"m","precision":"int8","language":"en"}`},
		{`{"device_id":"ov:NPU","backend":"openvino","model":"m","precision":"int8","language":"en"` + parts + `}`,
			`{"device_id":"ov:NPU","backend":"openvino","model":"m","precision":"int8","language":"en"}`},
	} {
		if got := PinASRSnapshot(c.raw); got != c.want {
			t.Errorf("PinASRSnapshot with parts\n got %s\nwant %s", got, c.want)
		}
		if !SameSnapshot(StageASR, c.raw, PinASRSnapshot(c.raw)) {
			t.Errorf("pinning changed the configuration of %s", c.raw)
		}
	}
}

// The JSON shape of the frozen device and benchmark structs.
func TestASRDeviceJSON(t *testing.T) {
	at := "2026-10-24T08:00:00Z"
	d := ASRDevice{ID: DeviceOVNPU, Backend: BackendOpenVINO, Label: "Intel(R) AI Boost", Enabled: true,
		Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100, DetectedAt: &at}
	b, _ := json.Marshal(d)
	want := `{"id":"ov:NPU","backend":"openvino","label":"Intel(R) AI Boost","enabled":true,"model":"large-v3-turbo","precision":"fp16","max_parallel":1,"priority":100,"detected_at":"2026-10-24T08:00:00Z","last_benchmark":null}`
	if string(b) != want {
		t.Fatalf("ASRDevice JSON\n got %s\nwant %s", b, want)
	}
	wer := 0.057
	bm := DeviceBenchmark{Version: BenchmarkVersion, Status: BenchmarkDone, StartedAt: at, FinishedAt: at,
		Audio: BenchmarkAudio{Source: BenchmarkSourceLec, LectureID: "L1", StartMS: 1000, EndMS: 121000, ReferenceWords: 300},
		Results: []BenchmarkResult{{Model: "large-v3-turbo", Precision: "fp16", OK: true, LoadS: 2.2, TranscribeS: 8.5,
			RTF: 0.071, WER: &wer, PeakMemMB: 5004, Words: true}},
		Pick: &BenchmarkPick{Model: "large-v3-turbo", Precision: "fp16", Rule: PickRuleBest}}
	raw, _ := json.Marshal(bm)
	var back DeviceBenchmark
	if err := json.Unmarshal(raw, &back); err != nil || back.Pick == nil || back.Results[0].WER == nil || *back.Results[0].WER != wer {
		t.Fatalf("benchmark round trip: %s %v", raw, err)
	}
	if got := MemObservedKey(DeviceOVNPU, "large-v3-turbo", "fp16"); got != "mem_observed:ov:NPU:large-v3-turbo:fp16" {
		t.Fatalf("MemObservedKey = %q", got)
	}
	if WaitDevicePaused(DeviceOVGPU) != "device_paused:ov:GPU" || WaitDeviceSelfTest(DeviceOVNPU) != "device_selftest:ov:NPU" {
		t.Fatal("waiting_for helpers")
	}
}
