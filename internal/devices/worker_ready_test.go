// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// taskLocator is an instLocator with unfinished install tasks
// (ComponentTasks): busy holds "python/{backend}" and
// "model/{backend}/{name}/{precision}" (faster-whisper: any precision).
type taskLocator struct {
	instLocator
	busy map[string]bool
}

func (l taskLocator) PythonInstalling(backend string) bool { return l.busy["python/"+backend] }

func (l taskLocator) ModelInstalling(backend, name, precision string) bool {
	if l.busy["model/"+backend+"/"+name+"/"+precision] {
		return true
	}
	if backend == model.BackendFasterWhisper {
		for k := range l.busy {
			if strings.HasPrefix(k, "model/"+backend+"/"+name+"/") {
				return true
			}
		}
	}
	return false
}

// writeOVModel puts a converted OpenVINO model (every ModelFiles entry) in dir.
func writeOVModel(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range ModelFiles {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fresh-cuda: WorkerState of the three component states (not installed,
// being installed, installed) for the environment and for the model, and
// the cases that never decide (no manager, test mode, remote backends).
func TestWorkerState(t *testing.T) {
	dataDir := t.TempDir()
	fw, ov := model.BackendFasterWhisper, model.BackendOpenVINO
	existing := filepath.Join(t.TempDir(), "python.exe")
	if err := os.WriteFile(existing, []byte("interpreter"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "nowhere", "python.exe")
	ovDir := filepath.Join(t.TempDir(), "ov-model")
	writeOVModel(t, ovDir)

	withLoc := func(python string, loc config.ComponentLocator) *config.Config {
		c := &config.Config{Python: python, PythonOpenVINO: python, Components: loc}
		return c
	}
	cudaOnly := instLocator{py: map[string]string{fw: "C:/c/asr-cuda/venv/python.exe"},
		models: map[string]string{fw + "/large-v3/float16": "C:/c/model"}}
	ovOnly := instLocator{py: map[string]string{ov: "C:/c/asr-openvino/venv/python.exe"},
		models: map[string]string{ov + "/large-v3-turbo/fp16": ovDir}}
	fake := withLoc("", instLocator{})
	fake.ASR.FakeResult = "testdata/fake_asr.json"

	for _, tc := range []struct {
		name, backend, mdl, precision string
		cfg                           *config.Config
		want                          string
	}{
		// never decided here: the earlier behaviour
		{"nil config", fw, "large-v3", "auto", nil, WorkerReady},
		{"no component manager", fw, "large-v3", "auto", &config.Config{Python: ""}, WorkerReady},
		{"test mode", fw, "large-v3", "auto", fake, WorkerReady},
		{"colab", model.BackendColab, "large-v3", "float16", withLoc("", instLocator{}), WorkerReady},
		{"ssh", model.DeviceBackend("ssh:lab"), "large-v3", "float16", withLoc("", instLocator{}), WorkerReady},

		// the environment
		{"not installed (desktop)", fw, "large-v3", "auto", withLoc("", instLocator{}), WorkerNoComponent},
		{"not installed (development, no .venv)", fw, "large-v3", "auto", withLoc(missing, instLocator{}), WorkerNoComponent},
		{"being installed", fw, "large-v3", "auto",
			withLoc("", taskLocator{busy: map[string]bool{"python/" + fw: true}}), WorkerInstalling},
		{"another backend being installed", fw, "large-v3", "auto",
			withLoc("", taskLocator{busy: map[string]bool{"python/" + ov: true}}), WorkerNoComponent},
		{"installed", fw, "large-v3", "auto", withLoc("", cudaOnly), WorkerReady},
		{"installed, exact precision", fw, "large-v3", "float16", withLoc("", cudaOnly), WorkerReady},
		{"configured interpreter (development .venv)", fw, "large-v3-turbo", "auto", withLoc(existing, instLocator{}), WorkerReady},

		// the model (asr-cuda installed)
		{"model not installed", fw, "large-v3-turbo", "auto", withLoc("", cudaOnly), WorkerNoModel},
		{"model being installed", fw, "large-v3-turbo", "auto",
			withLoc("", taskLocator{instLocator: cudaOnly, busy: map[string]bool{"model/" + fw + "/large-v3-turbo/float16": true}}), WorkerInstalling},
		{"another model being installed", fw, "large-v3-turbo", "auto",
			withLoc("", taskLocator{instLocator: cudaOnly, busy: map[string]bool{"model/" + fw + "/medium/float16": true}}), WorkerNoModel},

		// OpenVINO: the component's model files must be there
		{"openvino installed", ov, "large-v3-turbo", "fp16", withLoc("", ovOnly), WorkerReady},
		{"openvino model not installed", ov, "small", "int8", withLoc("", ovOnly), WorkerNoModel},
		{"openvino model being installed", ov, "small", "int8",
			withLoc("", taskLocator{instLocator: ovOnly, busy: map[string]bool{"model/" + ov + "/small/int8": true}}), WorkerInstalling},
		{"openvino not installed", ov, "large-v3-turbo", "fp16", withLoc("", cudaOnly), WorkerNoComponent},
	} {
		if got := WorkerState(tc.cfg, dataDir, tc.backend, tc.mdl, tc.precision); got != tc.want {
			t.Errorf("%s: WorkerState = %q, want %q", tc.name, got, tc.want)
		}
	}

	// An OpenVINO model the worker converted into the data directory serves
	// as before (ModelAvailable).
	writeOVModel(t, ModelDir(dataDir, "small", "int8"))
	if got := WorkerState(withLoc("", ovOnly), dataDir, ov, "small", "int8"); got != WorkerReady {
		t.Errorf("converted model in the data directory: %q", got)
	}
}

func TestWorkerWait(t *testing.T) {
	for st, want := range map[string]string{
		WorkerReady:       "",
		WorkerNoComponent: "component_missing:cuda:0",
		WorkerNoModel:     "model_missing:cuda:0",
		WorkerInstalling:  "component_installing:cuda:0",
	} {
		if got := WorkerWait(st, model.DeviceCUDA0); got != want {
			t.Errorf("WorkerWait(%q) = %q, want %q", st, got, want)
		}
	}
}
