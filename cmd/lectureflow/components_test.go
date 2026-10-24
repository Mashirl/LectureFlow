// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// SPEC-v3 step 5 (v5-integration-b1): the component manager in the service.

func TestComponentCheck(t *testing.T) {
	model0 := filepath.Join("D", "components", "model__openvino__small__int8", "1.0.0", "model")
	cases := []struct {
		name string
		req  components.SelfTestRequest
		cuda bool
		want stagesCheck
	}{
		{"python", components.SelfTestRequest{ID: "python", Kind: components.KindPythonRuntime, Backend: components.BackendBase, Python: "p"}, true,
			stagesCheck{backend: config.ComponentBase, python: "p"}},
		{"venv with a model", components.SelfTestRequest{ID: "asr-openvino", Kind: components.KindVenv, Backend: "openvino", Python: "p", ModelDir: model0}, true,
			stagesCheck{backend: "openvino", python: "p", modelDir: model0, model: "small", precision: "int8"}},
		{"venv without a model", components.SelfTestRequest{ID: "asr-cuda", Kind: components.KindVenv, Backend: "faster-whisper", Python: "p"}, true,
			stagesCheck{backend: "faster-whisper", python: "p", cuda: true}},
		{"model", components.SelfTestRequest{ID: "model:faster-whisper:large-v2:int8_float16", Kind: components.KindModel, Backend: "faster-whisper",
			Python: "p", ModelDir: "staging/model"}, false,
			stagesCheck{backend: "faster-whisper", python: "p", modelDir: "staging/model", model: "large-v2", precision: "int8_float16"}},
		{"model without its environment", components.SelfTestRequest{ID: "model:mlx:small:fp16", Kind: components.KindModel, Backend: "mlx", ModelDir: "m"}, true,
			stagesCheck{backend: "mlx", modelDir: "m", model: "small", precision: "fp16"}},
	}
	for _, c := range cases {
		got := componentCheck(c.req, c.cuda)
		g := stagesCheck{backend: got.Backend, python: got.Python, modelDir: got.ModelDir, model: got.Model, precision: got.Precision, cuda: got.CUDA}
		if g != c.want || got.ID != c.req.ID {
			t.Errorf("%s: %+v, want %+v", c.name, g, c.want)
		}
	}
}

type stagesCheck struct {
	backend, python, modelDir, model, precision string
	cuda                                        bool
}

func TestManifestURLChannel(t *testing.T) {
	if got := manifestURL(" https://x.invalid/{channel}/components.json ", "beta"); got != "https://x.invalid/beta/components.json" {
		t.Errorf("url %q", got)
	}
	if got := manifestURL("", "stable"); got != "" {
		t.Errorf("no URL: %q", got)
	}
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg)
	app, err := buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.Components != nil || cfg.Components != nil {
		t.Fatal("no built-in manifest: no component manager")
	}
	if ch := updateChannel(ctx, app.Store); ch != "stable" {
		t.Errorf("default channel %q", ch)
	}
	for v, want := range map[string]string{"beta": "beta", "stable": "stable", "nightly": "stable"} {
		if err := app.Store.SetKV(ctx, model.KVAppUpdateChannel, v); err != nil {
			t.Fatal(err)
		}
		if ch := updateChannel(ctx, app.Store); ch != want {
			t.Errorf("kv %q: channel %q", v, ch)
		}
	}
}

// installFake writes components/state.json with one installed venv
// component whose interpreter is an empty file (never run).
func installFake(t *testing.T, dataDir, id, backend string) string {
	t.Helper()
	root := filepath.Join(dataDir, "components")
	rel := id + "/1.0.0"
	py := filepath.Join(root, id, "1.0.0", "venv", "Scripts", "python.exe")
	if runtime.GOOS != "windows" {
		py = filepath.Join(root, id, "1.0.0", "venv", "bin", "python")
	}
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	st := map[string]any{"schema": 1, "active": map[string]any{id: map[string]any{
		"id": id, "version": "1.0.0", "kind": components.KindVenv, "backend": backend, "platform": components.CurrentPlatform(),
		"dir": rel, "installed_at": "2026-10-24T00:00:00Z", "size_bytes": 1,
	}}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return py
}

// buildApp opens the component manager when the built-in manifest exists:
// the stages and the detection use its environments; installing or
// removing a backend environment runs the device detection again.
func TestBuildAppComponents(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg)
	fake, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr.json"))
	cfg.ASR.FakeResult = fake // test mode: no real probe, no power request
	manifest, _ := filepath.Abs(filepath.Join("..", "..", "packaging", "components", "components.json"))
	cfg.ComponentsManifest = manifest
	py := installFake(t, cfg.DataDir, components.IDASROpenVINO, components.BackendOpenVINO)

	var probes atomic.Int32
	var gotOpts components.Options
	app, err := buildApp(ctx, cfg, AppOptions{
		Devices: func(o *devices.RegistryOptions) {
			o.CUDAName = func(context.Context) (string, error) { return "", nil }
			o.Probe = func(context.Context) (*devices.OVProbe, error) {
				probes.Add(1)
				return &devices.OVProbe{Devices: []devices.OVDevice{}, Ignored: []devices.OVDevice{}}, nil
			}
		},
		Components: func(o *components.Options) { gotOpts = *o },
		Version:    "3.0.0-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.Components == nil || cfg.Components != config.ComponentLocator(app.Components) {
		t.Fatalf("component manager not wired: %v %v", app.Components, cfg.Components)
	}
	if gotOpts.DataDir != cfg.DataDir || gotOpts.BuiltinManifest != manifest || gotOpts.AppVersion != "3.0.0-test" ||
		gotOpts.UV != cfg.ComponentsUV() || gotOpts.ManifestURL != "" || gotOpts.SelfTest == nil {
		t.Fatalf("options %+v", gotOpts)
	}
	if p, _, comp := cfg.WorkerPython(model.BackendOpenVINO); p != py || !comp {
		t.Fatalf("openvino interpreter %q (component %v), want %q", p, comp, py)
	}
	if p, _, comp := cfg.WorkerPython(model.BackendFasterWhisper); comp || p != cfg.Python {
		t.Fatalf("faster-whisper is not installed: %q", p)
	}

	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("first detection", func() bool { return app.Devices.LastDetection().Python == py })
	if n := probes.Load(); n != 1 {
		t.Fatalf("probes after start: %d", n)
	}

	// Removing asr-openvino: the detection runs again with python_openvino.
	if err := app.Components.Uninstall(components.IDASROpenVINO); err != nil {
		t.Fatal(err)
	}
	waitFor("detection after the uninstall", func() bool {
		return probes.Load() == 2 && app.Devices.LastDetection().Python == cfg.PythonOpenVINO
	})
	if p, _, comp := cfg.WorkerPython(model.BackendOpenVINO); comp || p != cfg.PythonOpenVINO {
		t.Fatalf("after the uninstall: %q %v", p, comp)
	}
	// An event that changes no backend environment detects nothing.
	time.Sleep(100 * time.Millisecond)
	if n := probes.Load(); n != 2 {
		t.Fatalf("probes: %d", n)
	}

	// The graceful stop closes the manager: no more tasks.
	app.Close()
	if _, err := app.Components.Install([]string{components.IDPython}, components.Sources{}); err == nil {
		t.Error("the closed manager accepted a task")
	}
}
