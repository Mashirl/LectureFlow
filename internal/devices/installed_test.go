// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// instLocator is a component manager with some components installed;
// faster-whisper models match any precision (as components.Manager.ModelDir).
type instLocator struct {
	py     map[string]string // backend → interpreter
	models map[string]string // backend/name/precision → dir
}

func (l instLocator) Python(backend string) (string, bool) {
	p, ok := l.py[backend]
	return p, ok
}

func (l instLocator) ModelDir(backend, name, precision string) (string, bool) {
	for k, d := range l.models {
		if k == backend+"/"+name+"/"+precision {
			return d, true
		}
	}
	if backend == model.BackendFasterWhisper {
		for k, d := range l.models {
			if p := strings.SplitN(k, "/", 3); p[0] == backend && p[1] == name {
				return d, true
			}
		}
	}
	return "", false
}

func candNames(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Model+"/"+c.Precision)
	}
	return out
}

// V12.2 acceptance issue 3: with the component manager and the backend's
// component environment, the self-test candidates are the installed models
// only; without the manager (development) or with the configured
// interpreter they are unchanged.
func TestInstalledCandidates(t *testing.T) {
	dataDir := t.TempDir()
	cuda := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper}
	all := candNames(Candidates(model.DeviceCUDA0))

	// no component manager: every candidate, every model available
	dev := &config.Config{}
	if got := candNames(InstalledCandidates(dev, dataDir, cuda)); !slices.Equal(got, all) {
		t.Errorf("development: %v, want %v", got, all)
	}
	if ComponentModelsOnly(dev, model.BackendFasterWhisper) || !ModelAvailable(dev, dataDir, model.BackendFasterWhisper, "large-v3-turbo", "auto") {
		t.Error("development must not restrict the models")
	}
	if ComponentModelsOnly(nil, model.BackendFasterWhisper) || !ModelAvailable(nil, dataDir, model.BackendOpenVINO, "small", "int8") {
		t.Error("nil config must not restrict the models")
	}

	// a manager, but faster-whisper runs from the configured .venv: unchanged
	venv := &config.Config{Components: instLocator{py: map[string]string{model.BackendOpenVINO: "ov-python"}}}
	if got := candNames(InstalledCandidates(venv, dataDir, cuda)); !slices.Equal(got, all) {
		t.Errorf("configured interpreter: %v, want %v", got, all)
	}

	// asr-cuda installed with large-v3 (float16) only: large-v3-turbo is
	// not tried (it would come from the global Hugging Face cache)
	fwDir := filepath.Join(dataDir, "components", "model__faster-whisper__large-v3__float16", "1", "model")
	comp := &config.Config{Components: instLocator{
		py:     map[string]string{model.BackendFasterWhisper: "fw-python"},
		models: map[string]string{"faster-whisper/large-v3/float16": fwDir},
	}}
	if !ComponentModelsOnly(comp, model.BackendFasterWhisper) {
		t.Fatal("component environment: models must be installed ones")
	}
	if got := candNames(InstalledCandidates(comp, dataDir, cuda)); !slices.Equal(got, []string{"large-v3/auto"}) {
		t.Errorf("components: %v", got)
	}
	if !ModelAvailable(comp, dataDir, model.BackendFasterWhisper, "large-v3", "int8_float16") ||
		ModelAvailable(comp, dataDir, model.BackendFasterWhisper, "large-v3-turbo", "auto") {
		t.Error("ModelAvailable faster-whisper")
	}
	// fw:cpu: neither small nor base installed → nothing to test
	if got := InstalledCandidates(comp, dataDir, model.ASRDevice{ID: model.DeviceFWCPU}); len(got) != 0 {
		t.Errorf("fw:cpu without models: %v", candNames(got))
	}
	if got := CandidateModels(model.DeviceFWCPU); !slices.Equal(got, []string{"small"}) {
		t.Errorf("CandidateModels fw:cpu %v (the fallback base is not offered)", got)
	}
	if got := CandidateModels(model.DeviceCUDA0); !slices.Equal(got, []string{"large-v3", "large-v3-turbo"}) {
		t.Errorf("CandidateModels cuda %v", got)
	}

	// OpenVINO from its component: the installed component, or a model the
	// worker converted earlier (no download needed); not a missing one
	ovComp := filepath.Join(dataDir, "components", "model__openvino__large-v3-turbo__int8", "1", "model")
	writeFiles(t, ovComp, ModelFiles...)
	writeFiles(t, ModelDir(dataDir, "small", "int8"), ModelFiles...)
	ov := &config.Config{Components: instLocator{
		py:     map[string]string{model.BackendOpenVINO: "ov-python"},
		models: map[string]string{"openvino/large-v3-turbo/int8": ovComp},
	}}
	ovGPU := model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO}
	if got := candNames(InstalledCandidates(ov, dataDir, ovGPU)); !slices.Equal(got, []string{"large-v3-turbo/int8", "small/int8"}) {
		t.Errorf("ov:GPU %v", got)
	}
	if got := candNames(InstalledCandidates(ov, dataDir, model.ASRDevice{ID: model.DeviceOVNPU})); !slices.Equal(got, []string{"large-v3-turbo/int8"}) {
		t.Errorf("ov:NPU (fp16 models missing) %v", got)
	}

	// MLX from its component: the model files must be there
	mlxDir := filepath.Join(dataDir, "components", "model__mlx__small__fp16", "1", "model")
	writeFiles(t, mlxDir, "config.json", "weights.safetensors")
	mlx := &config.Config{Components: instLocator{
		py:     map[string]string{model.BackendMLX: "mlx-python"},
		models: map[string]string{"mlx/small/fp16": mlxDir, "mlx/large-v3-turbo/fp16": filepath.Join(dataDir, "empty")},
	}}
	if got := candNames(InstalledCandidates(mlx, dataDir, model.ASRDevice{ID: model.DeviceMLXGPU})); !slices.Equal(got, []string{"small/fp16"}) {
		t.Errorf("mlx:gpu %v", got)
	}
}
