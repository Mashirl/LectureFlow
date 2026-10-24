// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// V12.2 acceptance issue 3 (NOTES "决定（v3 acc-fix-core）"): with the
// component manager the self-test runs the installed models only; with
// none installed it fails with selftest.no_model and runs no worker.
func TestDeviceTestInstalledModelsOnly(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("selftest", "")
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	_, ss := e.repairedLecture()
	_, _, ref, _ := devices.PlanClip(ss, devices.SelfTestClip)
	fwDir := filepath.Join(e.root, "components", "model__faster-whisper__large-v3__float16", "1.0.0", "model")
	e.selfTestScript(map[string]fakeCandidate{
		fwDir + "/float16":       {Text: ref, TranscribeS: 10},
		"large-v3-turbo/float16": {Text: ref, TranscribeS: 5}, // must not run: not installed
	})
	e.cfg.Components = compLocator{
		py:     map[string]string{model.BackendFasterWhisper: exe},
		models: map[string]string{"faster-whisper/large-v3/auto": fwDir},
	}
	cuda := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: "NVIDIA GeForce RTX", Enabled: true,
		Model: "large-v3", Precision: "auto", MaxParallel: 1}
	te := &testEnv{}
	b, err := e.deps(nil).DeviceTest(e.ctx, cuda, te.env())
	if err != nil || b.Status != model.BenchmarkDone || len(b.Results) != 1 || b.Results[0].Model != "large-v3" || !b.Results[0].OK {
		t.Fatalf("%+v %v", b, err)
	}
	jobs := fakeJobs(t, log)
	if len(jobs) != 1 || jobs[0].Job["model"] != fwDir {
		t.Fatalf("worker runs %+v, want one with the component directory", jobs)
	}
	for _, r := range te.reports {
		if r.Steps != 1 {
			t.Errorf("report %+v: steps should count the installed candidates only", r)
		}
	}

	// nothing installed: failed with selftest.no_model (device: no row, so
	// the id), no worker, no clip
	e.cfg.Components = compLocator{py: map[string]string{model.BackendFasterWhisper: exe}}
	b, err = e.deps(nil).DeviceTest(e.ctx, cuda, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkFailed || b.Error == nil || b.Error.Code != devices.CodeSelfTestNoModel ||
		b.Error.Params["device"] != model.DeviceCUDA0 || b.Error.Params["models"] != "large-v3 / large-v3-turbo" || len(b.Results) != 0 {
		t.Fatalf("no installed model: %+v %v (error %+v)", b, err, b.Error)
	}
	if n := len(fakeJobs(t, log)); n != 1 {
		t.Errorf("a worker ran without an installed model (%d runs)", n)
	}
	if _, err := os.Stat(filepath.Join(selfTestDir(e.cfg.DataDir, cuda.ID), model.AudioFile)); err == nil {
		t.Error("clip left behind")
	}

	// development (no component manager): every candidate, as before
	e.cfg.Python = exe
	e.cfg.Components = nil
	e.selfTestScript(map[string]fakeCandidate{
		"large-v3/float16":       {Text: ref, TranscribeS: 10},
		"large-v3-turbo/float16": {Text: ref, TranscribeS: 5},
	})
	b, err = e.deps(nil).DeviceTest(e.ctx, cuda, (&testEnv{}).env())
	if err != nil || b.Status != model.BenchmarkDone || len(b.Results) != 2 {
		t.Fatalf("development: %+v %v", b, err)
	}
}

// checkASRModel: the asr stage's guard against a model that is not
// installed (permanent devices.model_not_installed); nothing changes in
// development or for an installed model.
func TestCheckASRModel(t *testing.T) {
	e := newEnv(t)
	snap := func(mdl string) model.ASRSnapshot {
		return model.ASRSnapshot{Backend: model.BackendFasterWhisper, DeviceID: model.DeviceCUDA0, Model: mdl, Precision: "float16"}
	}
	d := e.deps(nil)
	if err := d.checkASRModel(snap("medium")); err != nil {
		t.Fatalf("development: %v", err)
	}
	e.cfg.Components = compLocator{
		py:     map[string]string{model.BackendFasterWhisper: "fw-python", config.ComponentBase: "base-python"},
		models: map[string]string{"faster-whisper/large-v3/float16": filepath.Join(e.root, "m")},
	}
	if err := d.checkASRModel(snap("large-v3")); err != nil {
		t.Fatalf("installed: %v", err)
	}
	err := d.checkASRModel(snap("medium"))
	if err == nil || !sched.IsPermanent(err) || codeOf(err) != devices.CodeModelNotInstalled {
		t.Fatalf("not installed: %v", err)
	}
	// the backend runs from the configured interpreter: not restricted
	e.cfg.Components = compLocator{models: map[string]string{}}
	if err := d.checkASRModel(snap("medium")); err != nil {
		t.Fatalf("configured interpreter: %v", err)
	}
}
