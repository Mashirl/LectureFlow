// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// inUseLocator mimics the manager: asr-cuda and the python base venv
// installed, large-v3 float16 (the int8_float16 request falls back to it,
// as Manager.ModelDir does) and openvino medium int8; no OpenVINO venv.
type inUseLocator struct{ root string }

func (l inUseLocator) dir(id, version, leaf string) string {
	return filepath.Join(l.root, components.DirName(id), version, leaf)
}

func (l inUseLocator) Python(backend string) (string, bool) {
	switch backend {
	case model.BackendFasterWhisper:
		return l.dir(components.IDASRCUDA, "1.0.0", filepath.Join("venv", "Scripts", "python.exe")), true
	case config.ComponentBase:
		return l.dir(components.IDPython, "3.12.10", filepath.Join("venv", "Scripts", "python.exe")), true
	}
	return "", false
}

func (l inUseLocator) ModelDir(backend, name, precision string) (string, bool) {
	switch {
	case backend == model.BackendFasterWhisper && name == "large-v3":
		return l.dir(components.ModelID(backend, name, model.PrecisionFloat16), "1", "model"), true
	case backend == model.BackendOpenVINO && name == "medium" && precision == model.PrecisionINT8:
		return l.dir(components.ModelID(backend, name, precision), "1", "model"), true
	}
	return "", false
}

func TestComponentInUse(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	loc := inUseLocator{root: filepath.Join(cfg.DataDir, "components")}
	cfg.Components = loc
	u := newComponentUse(cfg)
	var work []sched.WorkerUse
	var checks []api.RunningPython
	var probes []string
	u.setWork(func() []sched.WorkerUse { return work })
	u.addPythons(serverPythons(func() []api.RunningPython { return checks }))
	u.addPythons(probePythons(func() []string { return probes }))

	fwF16 := components.ModelID(model.BackendFasterWhisper, "large-v3", model.PrecisionFloat16)
	fwI8 := components.ModelID(model.BackendFasterWhisper, "large-v3", model.PrecisionInt8Float16)
	fwSmall := components.ModelID(model.BackendFasterWhisper, "small", model.PrecisionFloat16)
	ovMedium := components.ModelID(model.BackendOpenVINO, "medium", model.PrecisionINT8)
	ovSmall := components.ModelID(model.BackendOpenVINO, "small", model.PrecisionINT8)
	type want struct {
		id     string
		in     bool
		reason string
	}
	check := func(name string, ws ...want) {
		t.Helper()
		for _, w := range ws {
			in, reason := u.InUse(w.id)
			if in != w.in || reason != w.reason {
				t.Errorf("%s: InUse(%s) = %v %q, want %v %q", name, w.id, in, reason, w.in, w.reason)
			}
		}
	}

	check("nothing runs",
		want{components.IDASRCUDA, false, ""}, want{components.IDPython, false, ""}, want{fwF16, false, ""})

	// asr on cuda:0 with int8_float16: the model falls back to float16.
	work = []sched.WorkerUse{{What: sched.UseASR, ID: "j1", Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: model.PrecisionInt8Float16}}
	check("asr faster-whisper",
		want{components.IDASRCUDA, true, "asr"}, want{components.IDPython, true, "asr"}, want{fwF16, true, "asr"},
		want{fwI8, false, ""}, want{fwSmall, false, ""}, want{components.IDASROpenVINO, false, ""}, want{ovMedium, false, ""})

	// asr on OpenVINO with the configured interpreter (no venv component):
	// only its model.
	work = []sched.WorkerUse{{What: sched.UseASR, ID: "j2", Backend: model.BackendOpenVINO, Model: "medium", Precision: model.PrecisionINT8}}
	check("asr openvino",
		want{ovMedium, true, "asr"}, want{ovSmall, false, ""}, want{components.IDASROpenVINO, false, ""},
		want{components.IDPython, false, ""}, want{components.IDASRCUDA, false, ""})

	// A device self-test may try any model of its backend.
	work = []sched.WorkerUse{{What: sched.UseDeviceTest, ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper}}
	check("device test",
		want{fwSmall, true, "device_test"}, want{fwI8, true, "device_test"}, want{components.IDASRCUDA, true, "device_test"},
		want{ovMedium, false, ""})

	// PDF extraction runs the python component's base venv.
	work = []sched.WorkerUse{{What: sched.UseMaterial, ID: "m1", Backend: config.ComponentBase}}
	check("material",
		want{components.IDPython, true, "material"}, want{components.IDASRCUDA, false, ""}, want{fwF16, false, ""})

	// A diagnostics check runs the asr-cuda interpreter.
	work = nil
	py, _ := loc.Python(model.BackendFasterWhisper)
	checks = []api.RunningPython{{Python: py, Reason: api.PythonUseSelfCheck}, {Python: cfg.Python, Reason: api.PythonUseSelfCheck}}
	check("diagnostics",
		want{components.IDASRCUDA, true, "self_check"}, want{components.IDPython, true, "self_check"}, want{fwF16, false, ""})
	checks = nil

	// The syllabus PDF extraction of a request runs the base venv.
	base, _ := loc.Python(config.ComponentBase)
	checks = []api.RunningPython{{Python: base, Reason: api.PythonUseMaterial}}
	check("syllabus extraction",
		want{components.IDPython, true, "material"}, want{components.IDASRCUDA, false, ""})
	checks = nil

	// The OpenVINO probe of the device detection runs a component venv.
	ovPy := loc.dir(components.IDASROpenVINO, "1.0.0", filepath.Join("venv", "Scripts", "python.exe"))
	probes = []string{ovPy}
	check("detection probe",
		want{components.IDASROpenVINO, true, "self_check"}, want{components.IDPython, true, "self_check"},
		want{components.IDASRCUDA, false, ""}, want{ovMedium, false, ""})
	probes = nil

	// An update self-test of asr-cuda with an installed model: both in use
	// while it runs, nothing afterwards.
	model16, _ := loc.ModelDir(model.BackendFasterWhisper, "large-v3", model.PrecisionFloat16)
	var during [3]bool
	st := u.wrapSelfTest(func(context.Context, components.SelfTestRequest) error {
		during[0], _ = u.InUse(fwF16)
		during[1], _ = u.InUse(components.IDASRCUDA)
		during[2], _ = u.InUse(fwSmall)
		return nil
	})
	newPy := loc.dir(components.IDASRCUDA, "1.1.0", filepath.Join("venv", "Scripts", "python.exe"))
	if err := st(context.Background(), components.SelfTestRequest{ID: components.IDASRCUDA, Version: "1.1.0", Kind: components.KindVenv,
		Backend: model.BackendFasterWhisper, Python: newPy, ModelDir: model16}); err != nil {
		t.Fatal(err)
	}
	if during != [3]bool{true, true, false} {
		t.Errorf("during the update self-test: %v", during)
	}
	check("after the self-test", want{fwF16, false, ""}, want{components.IDASRCUDA, false, ""})

	// Paths outside the components root never count.
	if id := u.componentOf(cfg.Python); id != "" {
		t.Errorf("componentOf(%s) = %q", cfg.Python, id)
	}
	if id := u.componentOf(loc.root); id != "" {
		t.Errorf("componentOf(root) = %q", id)
	}
}
