// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// The three sources of the memory estimate (I8, lead ruling 8): observed
// peak × 1.1, the self-test's peak, twice the model size.
func TestMemEstimateSources(t *testing.T) {
	st := regStore(t)
	data := t.TempDir()
	d := model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16"}

	// 3. nothing known: 2 × the 3a size of the model.
	if got := MemEstimateMB(tctx(), st, data, d); got != 2*1551 {
		t.Fatalf("model size: %d", got)
	}
	small := d
	small.Model, small.Precision = "small", "int8"
	if got := MemEstimateMB(tctx(), st, data, small); got != 2*243 {
		t.Fatalf("small int8: %d", got)
	}
	// A combination 3a did not convert: the size on disk (2.5 MB → 3 MB).
	other := d
	other.Model = "distil-x"
	dir := ModelDir(data, other.Model, other.Precision)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 2<<20), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.bin"), make([]byte, 512<<10), 0o644)
	if got := MemEstimateMB(tctx(), st, data, other); got != 2*3 {
		t.Fatalf("disk size: %d", got)
	}
	// Nothing at all: 2 × 1551.
	other.Model = "nothing-here"
	if got := MemEstimateMB(tctx(), st, data, other); got != 2*DefaultMemModelMB {
		t.Fatalf("default: %d", got)
	}

	// 2. the last self-test's peak for the same model and precision (failed
	// results and other combinations do not count).
	b := model.DeviceBenchmark{Version: 1, Status: model.BenchmarkDone, Results: []model.BenchmarkResult{
		{Model: "large-v3-turbo", Precision: "fp16", OK: false, PeakMemMB: 9999},
		{Model: "large-v3-turbo", Precision: "int8", OK: true, PeakMemMB: 4000},
		{Model: "large-v3-turbo", Precision: "fp16", OK: true, PeakMemMB: 5004},
	}}
	raw, _ := json.Marshal(b)
	s := string(raw)
	d.LastBenchmark = &s
	if got := MemEstimateMB(tctx(), st, data, d); got != 5004 {
		t.Fatalf("benchmark: %d", got)
	}

	// 1. the observed peak wins: ceil(1.1 × 5003) = 5504.
	if err := RecordObservedMem(tctx(), st, d.ID, d.Model, d.Precision, 5003); err != nil {
		t.Fatal(err)
	}
	if got := MemEstimateMB(tctx(), st, data, d); got != 5504 {
		t.Fatalf("observed: %d", got)
	}
	// Another device's observation does not count.
	if got := MemEstimateMB(tctx(), st, data, model.ASRDevice{ID: model.DeviceOVGPU, Model: d.Model, Precision: d.Precision}); got != 2*1551 {
		t.Fatalf("other device: %d", got)
	}
	// A broken kv value is ignored.
	st.SetKV(tctx(), model.MemObservedKey(model.DeviceOVGPU, "small", "fp16"), "lots")
	if got := MemEstimateMB(tctx(), st, data, model.ASRDevice{ID: model.DeviceOVGPU, Model: "small", Precision: "fp16"}); got != 2*469 {
		t.Fatalf("broken kv: %d", got)
	}
}

func TestRecordObservedMemKeepsTheLargest(t *testing.T) {
	st := regStore(t)
	key := model.MemObservedKey(model.DeviceOVGPU, "large-v3-turbo", "fp16")
	get := func() string {
		v, _, err := st.GetKV(tctx(), key)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		mb   int
		want string
	}{
		{0, ""}, {-5, ""}, // not reported: nothing written
		{2600, "2600"},
		{2400, "2600"}, // smaller: kept
		{2683, "2683"},
		{2683, "2683"},
	} {
		if err := RecordObservedMem(tctx(), st, model.DeviceOVGPU, "large-v3-turbo", "fp16", c.mb); err != nil {
			t.Fatal(err)
		}
		if got := get(); got != c.want {
			t.Fatalf("after %d: %q, want %q", c.mb, got, c.want)
		}
	}
	// A broken stored value is replaced.
	st.SetKV(tctx(), key, "x")
	RecordObservedMem(tctx(), st, model.DeviceOVGPU, "large-v3-turbo", "fp16", 100)
	if get() != "100" {
		t.Fatalf("broken value: %q", get())
	}
}

// faster-whisper rows (power management: the worker memory budget counts
// the CUDA worker's host memory too): observed × 1.1, the self-test's
// peak, else FWHostMemMB (I12: 3346 MB for large-v3 float16 on CUDA) —
// never the OpenVINO model sizes.
func TestMemEstimateFasterWhisper(t *testing.T) {
	st := regStore(t)
	data := t.TempDir()
	d := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: "auto"}
	if got := MemEstimateMB(tctx(), st, data, d); got != 3400 || FWHostMemLargeMB != 3400 {
		t.Fatalf("large-v3: %d", got)
	}
	for _, m := range []string{"large-v2", "large-v3-turbo", "distil-large-v3"} {
		d.Model = m
		if got := MemEstimateMB(tctx(), st, data, d); got != FWHostMemLargeMB {
			t.Fatalf("%s: %d", m, got)
		}
	}
	cpu := model.ASRDevice{ID: model.DeviceFWCPU, Model: "small", Precision: "auto"}
	if got := MemEstimateMB(tctx(), st, data, cpu); got != 2000 || FWHostMemOtherMB != 2000 {
		t.Fatalf("fw:cpu small: %d", got)
	}
	d.Model = "large-v3"
	if err := RecordObservedMem(tctx(), st, d.ID, d.Model, d.Precision, 3346); err != nil {
		t.Fatal(err)
	}
	if got := MemEstimateMB(tctx(), st, data, d); got != 3681 { // ceil(1.1 × 3346)
		t.Fatalf("observed: %d", got)
	}
}
