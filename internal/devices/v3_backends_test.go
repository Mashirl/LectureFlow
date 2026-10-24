// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// SPEC-v3 V5 (v5-integration-a): mlx:gpu is detected through
// RegistryOptions.MLX (darwin default: Apple Silicon + python_mlx) and gets
// the MLX defaults; the CPU fallback stays off next to it.
func TestDetectMLX(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda, w.probe, w.probeErr = "", nil, errors.New("no openvino")
	o := w.opts()
	o.MLX = func(context.Context) (string, bool) { return "Apple M3 Pro", true }
	reg := NewRegistry(st, config.Default(), t.TempDir(), o)
	if err := reg.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	d, newIDs, err := reg.Detect(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if d.MLX != "Apple M3 Pro" || len(newIDs) != 1 || newIDs[0] != model.DeviceMLXGPU {
		t.Fatalf("detection %+v new %v", d, newIDs)
	}
	row, ok := reg.Get(model.DeviceMLXGPU)
	if !ok || row.Backend != model.BackendMLX || row.Label != "Apple M3 Pro" || !row.Enabled || row.Model != "large-v3-turbo" ||
		row.Precision != model.PrecisionFP16 || row.Priority != 250 || row.MaxParallel != 1 {
		t.Fatalf("mlx row %+v", row)
	}
	if cpu, ok := reg.Get(model.DeviceFWCPU); !ok || cpu.Enabled {
		t.Errorf("fw:cpu must be off next to the Apple GPU: %+v", cpu)
	}
}

func TestMLXHelpers(t *testing.T) {
	if c := Candidates(model.DeviceMLXGPU); len(c) != 2 || c[0] != (Candidate{Model: "large-v3-turbo", Precision: model.PrecisionFP16}) || !c[1].Fallback {
		t.Errorf("candidates %+v", c)
	}
	for _, p := range PrecisionChoices(model.BackendMLX) {
		if !model.ValidPrecision(model.BackendMLX, p) {
			t.Errorf("precision %s", p)
		}
	}
	if f := Fallback(model.BackendMLX); !model.ValidPrecision(model.BackendMLX, f.Precision) {
		t.Errorf("fallback %+v", f)
	}
	if f := Fallback(model.BackendColab); !model.ValidPrecision(model.BackendColab, f.Precision) {
		t.Errorf("colab fallback %+v", f)
	}
	if got := MLXModelDir("D", "small"); got != filepath.Join("D", "models", "mlx", "small") {
		t.Errorf("model dir %s", got)
	}
	d := model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX, Model: "small", Precision: "fp16"}
	big := "large-v9"
	if err := applyPatch(&d, model.ASRDevicePatch{Model: &big}); err == nil {
		t.Error("unknown MLX model accepted")
	}
	int8 := model.PrecisionINT8
	if err := applyPatch(&d, model.ASRDevicePatch{Precision: &int8}); err != nil || d.Precision != int8 {
		t.Errorf("int8: %v", err)
	}
	// a Mac on battery keeps using mlx:gpu (SPEC-v3 V5)
	in := PolicyInput{Devices: []PolicyDevice{
		{ID: model.DeviceMLXGPU, Enabled: true, Present: true, Slots: 1},
		{ID: model.DeviceFWCPU, Enabled: true, Present: true, Slots: 1},
	}}
	if id, ok := BatteryDevice(in); !ok || id != model.DeviceMLXGPU {
		t.Errorf("battery device %q %v", id, ok)
	}
	if !isAccelerator(model.DeviceMLXGPU) || isAccelerator(model.DeviceColab) {
		t.Error("accelerator kinds")
	}
}
