// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// compLocator is a component manager with some components installed.
type compLocator struct {
	py     map[string]string // backend → interpreter
	models map[string]string // backend/name/precision → dir
}

func (c compLocator) Python(backend string) (string, bool) {
	p, ok := c.py[backend]
	return p, ok
}

func (c compLocator) ModelDir(backend, name, precision string) (string, bool) {
	d, ok := c.models[backend+"/"+name+"/"+precision]
	return d, ok
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), make([]byte, 2<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// SPEC-v3 step 5: an installed model component is used before the data
// directory's own locations; without it the earlier locations stay.
func TestComponentModelPaths(t *testing.T) {
	data := t.TempDir()
	cfg := config.Default()
	for _, c := range []*config.Config{nil, cfg} { // nil config and no locator: the old locations
		if got := OVModelPath(c, data, "small", "int8"); got != ModelDir(data, "small", "int8") {
			t.Errorf("ov without components: %s", got)
		}
		if got := MLXModelPath(c, data, "small", "fp16"); got != MLXModelDir(data, "small") {
			t.Errorf("mlx without components: %s", got)
		}
		if got := FWModel(c, "large-v2", "float16"); got != "large-v2" {
			t.Errorf("faster-whisper without components: %s", got)
		}
	}
	comp := t.TempDir()
	ovDir := filepath.Join(comp, "model__openvino__small__int8", "1.0.0", "model")
	mlxDir := filepath.Join(comp, "model__mlx__small__fp16", "1.0.0", "model")
	fwDir := filepath.Join(comp, "model__faster-whisper__large-v2__float16", "1.0.0", "model")
	cfg.Components = compLocator{models: map[string]string{
		"openvino/small/int8": ovDir, "mlx/small/fp16": mlxDir, "faster-whisper/large-v2/float16": fwDir,
	}}
	if got := OVModelPath(cfg, data, "small", "int8"); got != ovDir {
		t.Errorf("ov component: %s", got)
	}
	if got := OVModelPath(cfg, data, "small", "fp16"); got != ModelDir(data, "small", "fp16") {
		t.Errorf("ov precision not installed: %s", got)
	}
	if got := MLXModelPath(cfg, data, "small", "fp16"); got != mlxDir {
		t.Errorf("mlx component: %s", got)
	}
	if got := MLXModelPath(cfg, data, "medium", "fp16"); got != MLXModelDir(data, "medium") {
		t.Errorf("mlx model not installed: %s", got)
	}
	if got := FWModel(cfg, "large-v2", "float16"); got != fwDir {
		t.Errorf("faster-whisper component: %s", got)
	}
	if got := FWModel(cfg, "large-v3", "float16"); got != "large-v3" {
		t.Errorf("faster-whisper model not installed: %s", got)
	}

	// readiness follows the chosen directory
	if OVModelReady(cfg, data, "small", "int8") {
		t.Error("empty component directory is not ready")
	}
	writeFiles(t, ovDir, ModelFiles...)
	if !OVModelReady(cfg, data, "small", "int8") || ModelReady(data, "small", "int8") {
		t.Error("the component's model is ready, the data directory's is not")
	}
	if MLXModelFilesReady(mlxDir) {
		t.Error("mlx without files")
	}
	writeFiles(t, mlxDir, "config.json", "weights.safetensors")
	if !MLXModelFilesReady(mlxDir) {
		t.Error("mlx with config.json + weights.safetensors")
	}
	if FWModelFilesReady(fwDir) {
		t.Error("faster-whisper without model.bin")
	}
	writeFiles(t, fwDir, "model.bin")
	if !FWModelFilesReady(fwDir) {
		t.Error("faster-whisper with model.bin")
	}

	// the unified-memory estimate counts the component's size (2 × 4 MB on disk)
	mlx := model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX, Model: "small", Precision: "fp16"}
	if mb := MemEstimateMBFor(tctx(), nil, cfg, data, mlx); mb != 8 {
		t.Errorf("mlx estimate from the component: %d MB, want 8", mb)
	}
	if mb := MemEstimateMB(tctx(), nil, data, mlx); mb == 8 {
		t.Errorf("without components the component's size must not count: %d", mb)
	}
}

// The OpenVINO probe and the MLX detection use the component's interpreter
// when installed (Detection.Python), the configured one otherwise. The
// probe below never starts a process: the interpreter does not exist.
func TestDetectUsesComponentPython(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	o := w.opts()
	o.Probe = nil // the worker probe (not test mode: no fake_result)
	var mlxPython string
	cfg := config.Default()
	cfg.PythonOpenVINO = filepath.Join(t.TempDir(), "no-venv-ov", "python.exe")
	reg := NewRegistry(st, cfg, t.TempDir(), o)
	if err := reg.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	d, _, err := reg.Detect(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if d.Python != cfg.PythonOpenVINO || d.OVErr == nil || d.OVErr.Code != CodeOVPythonMissing || d.OVErr.Params["path"] != cfg.PythonOpenVINO {
		t.Fatalf("without components: python %q err %+v", d.Python, d.OVErr)
	}

	comp := filepath.Join(t.TempDir(), "components", "asr-openvino", "1.0.0", "venv", "Scripts", "python.exe")
	cfg.Components = compLocator{py: map[string]string{model.BackendOpenVINO: comp, model.BackendMLX: "/c/mlx/python"}}
	if d, _, err = reg.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if d.Python != comp || d.OVErr == nil || d.OVErr.Params["path"] != comp {
		t.Fatalf("with the asr-openvino component: python %q err %+v", d.Python, d.OVErr)
	}
	// the default MLX detection gets the asr-mlx interpreter (the platform
	// part only runs on darwin; here the hook records the argument)
	reg.o.MLX = nil
	prev := detectMLXFn
	t.Cleanup(func() { detectMLXFn = prev })
	detectMLXFn = func(_ context.Context, python string) (string, bool) { mlxPython = python; return "", false }
	cfg.ASR.FakeResult = ""
	if _, _, err = reg.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if mlxPython != "/c/mlx/python" {
		t.Fatalf("mlx detection ran with %q", mlxPython)
	}
}
