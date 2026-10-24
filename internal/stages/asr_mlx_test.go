// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// SPEC-v3 V5 (v5-integration-a): an mlx snapshot runs worker/asr_mlx.py
// with python_mlx, job.json with the OpenVINO worker's model_dir / device /
// precision fields and checkpoint contract (fake worker).

func (e *env) useFakeMLX(mode, result string) {
	e.t.Helper()
	e.useFakeWorker(mode, result)
	if err := os.WriteFile(filepath.Join(e.cfg.WorkerDir, "asr_mlx.py"), []byte("# fake\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.cfg.PythonMLX = e.cfg.Python
	e.cfg.Python = filepath.Join(e.root, "no-venv", "python.exe") // only python_mlx may be used
	e.cfg.PythonOpenVINO = filepath.Join(e.root, "no-venv-ov", "python.exe")
}

func TestASRMLXWorkerJob(t *testing.T) {
	e := newEnv(t)
	e.useFakeMLX("ov_ok", fakeASROV)
	log := e.jobLog()
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	data := e.absDataDir()
	l := e.lecture("L mlx", "m.mp4")
	e.writeWAV(l, "wav mlx")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "large-v3-turbo", "fp16", "de"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	jobs := fakeJobs(t, log)
	got := jobs[len(jobs)-1]
	if got.Script != "asr_mlx.py" {
		t.Fatalf("ran %s", got.Script)
	}
	dir, _ := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
	want := map[string]any{
		"audio": filepath.Join(dir, model.AudioFile), "model": "large-v3-turbo", "precision": "fp16",
		"model_dir": devices.MLXModelDir(data, "large-v3-turbo"), "device": model.DeviceMLXGPU, "language": "de",
		"initial_prompt": "cache", "word_timestamps": true,
	}
	wantKeys := append(sortedKeys(want), "checkpoint")
	sort.Strings(wantKeys)
	if !slices.Equal(sortedKeys(got.Job), wantKeys) {
		t.Errorf("job.json keys %v, want %v", sortedKeys(got.Job), wantKeys)
	}
	for k, v := range want {
		if got.Job[k] != v {
			t.Errorf("job.%s = %v, want %v", k, got.Job[k], v)
		}
	}
	wavSum, _ := e.stage(e.deps(nil), model.StageASR).(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
	e.checkCheckpointJob(got.Job, map[string]any{"path": filepath.Join(dir, model.ASRCheckpointFile),
		"job_id": r.job.ID, "input_hash": ctxbuild.ASRInputHashV3(wavSum, "de"), "device_id": model.DeviceMLXGPU,
		"model": "large-v3-turbo", "precision": "fp16", "chunk_s": float64(devices.OVCheckpointChunkS)})
	if n := len(e.sentences(l.ID)); n != 14 {
		t.Errorf("%d sentences", n)
	}
	// the observed peak calibrates the memory admission (unified memory)
	if v, ok, err := e.st.GetKV(e.ctx, model.MemObservedKey(model.DeviceMLXGPU, "large-v3-turbo", "fp16")); err != nil || !ok || v != "2683" {
		t.Errorf("mem_observed %q %v %v", v, ok, err)
	}
}

// ERROR_CODE device_unavailable: permanent, as for OpenVINO, with the MLX
// code (v5-integration-b1).
func TestASRMLXDeviceUnavailable(t *testing.T) {
	e := newEnv(t)
	e.useFakeMLX("ov_err_device", fakeASROV)
	e.must(e.st.InsertASRDevice(e.ctx, model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX,
		Label: "Apple M3 Pro", Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1}))
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "large-v3-turbo", "fp16", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	info, _ := state.ParseJobError(deref(r.job.Error))
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeASRMLXDeviceUnavailable || !paramsMatch(info, "device", "Apple M3 Pro") {
		t.Fatalf("err %v, job.error %+v", r.err, info)
	}
}

// A missing python_mlx fails permanently without running anything
// (devices.mlx_python_missing since v5-integration-b1).
func TestASRMLXInterpreterMissing(t *testing.T) {
	e := newEnv(t)
	e.useFakeMLX("ov_ok", fakeASROV)
	e.cfg.PythonMLX = filepath.Join(e.root, "no-venv-mlx", "python")
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "large-v3-turbo", "fp16", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != devices.CodeMLXPythonMissing {
		t.Fatalf("err %v", r.err)
	}
}

func TestRunnableDeviceV3(t *testing.T) {
	if !runnableDevice(model.DeviceMLXGPU, model.BackendMLX) {
		t.Error("mlx:gpu must be runnable")
	}
	if runnableDevice(model.DeviceColab, model.BackendColab) {
		t.Error("colab is not run by the asr stage yet")
	}
	if runnableDevice(model.DeviceMLXGPU, model.BackendOpenVINO) {
		t.Error("backend mismatch")
	}
}
