// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
	"lectureflow/internal/subs"
)

// Phase 3b task D (SPEC-v2 I6, I7, I11): the asr stage on the devices of
// the job's snapshot, with the test binary playing worker/asr_openvino.py.

const fakeASROV = "../../testdata/fake_asr_openvino.json"

// deviceSnapshot is the I6 asr snapshot of a fixed device.
func deviceSnapshot(id, mdl, precision, language string) string {
	return model.MarshalSnapshot(model.ASRDeviceSnapshot(model.ASRDevice{ID: id, Backend: model.DeviceBackend(id), Model: mdl, Precision: precision}, language, false))
}

// jobLog points LF_FAKE_JOBLOG at a fresh file and returns its path.
func (e *env) jobLog() string {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), "jobs.jsonl")
	e.t.Setenv(fakeJobLogEnv, p)
	return p
}

func (e *env) absDataDir() string {
	e.t.Helper()
	abs, err := filepath.Abs(e.cfg.DataDir)
	if err != nil {
		e.t.Fatal(err)
	}
	return abs
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestASROpenVINOWorkerJob: an openvino snapshot runs worker/asr_openvino.py
// with python_openvino (config python is not even there) and the job.json
// of brief section 4; result, timing, observed memory and the v3 hash are
// recorded.
func TestASROpenVINOWorkerJob(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ov_ok", fakeASROV)
	log := e.jobLog()
	e.cfg.Python = filepath.Join(e.root, "no-venv", "python.exe") // only python_openvino may be used
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	data := e.absDataDir()

	for _, c := range []struct{ id, dev, mdl, prec, cache string }{
		{model.DeviceOVNPU, "NPU", "large-v3-turbo", "fp16", devices.CacheDir(data, "NPU", "large-v3-turbo", "fp16")},
		{model.DeviceOVGPU, "GPU", "large-v3-turbo", "int8", devices.CacheDir(data, "GPU", "large-v3-turbo", "int8")},
		{model.DeviceOVCPU, "CPU", "small", "int8", ""},
	} {
		l := e.lecture("L "+c.dev, "m.mp4")
		e.writeWAV(l, "wav "+c.dev)
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(c.id, c.mdl, c.prec, "de"))
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", c.id, r.err)
		}
		jobs := fakeJobs(t, log)
		got := jobs[len(jobs)-1]
		dir, _ := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
		want := map[string]any{
			"audio": filepath.Join(dir, model.AudioFile), "model": c.mdl, "precision": c.prec,
			"model_dir": devices.ModelDir(data, c.mdl, c.prec), "device": c.dev, "language": "de",
			"initial_prompt": "cache", "cache_dir": c.cache, "hf_id": "openai/whisper-" + c.mdl,
			"hf_home": devices.HFHome(data), "word_timestamps": true, "min_free_bytes": float64(10 << 30),
		}
		if got.Script != "asr_openvino.py" {
			t.Errorf("%s: ran %s", c.id, got.Script)
		}
		// power management (power-brief §4): pipeline runs carry the checkpoint object
		wantKeys := append(sortedKeys(want), "checkpoint")
		sort.Strings(wantKeys)
		if !slices.Equal(sortedKeys(got.Job), wantKeys) {
			t.Errorf("%s: job.json keys %v, want %v", c.id, sortedKeys(got.Job), wantKeys)
		}
		for k, v := range want {
			if got.Job[k] != v {
				t.Errorf("%s: job.%s = %v, want %v", c.id, k, got.Job[k], v)
			}
		}
		wavSum, _ := e.stage(e.deps(nil), model.StageASR).(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
		e.checkCheckpointJob(got.Job, map[string]any{"path": filepath.Join(dir, model.ASRCheckpointFile),
			"job_id": r.job.ID, "input_hash": ctxbuild.ASRInputHashV3(wavSum, "de"), "device_id": c.id, "model": c.mdl,
			"precision": c.prec, "chunk_s": float64(devices.OVCheckpointChunkS)})
		ss := e.sentences(l.ID)
		if len(ss) != 14 {
			t.Fatalf("%s: %d sentences", c.id, len(ss))
		}
		for _, s := range ss {
			if s.Words != nil || len(s.LowConfWords) != 0 {
				t.Errorf("%s: sentence %d words %v low_conf %v", c.id, s.Idx, s.Words, s.LowConfWords)
			}
		}
		wantTiming := model.ASRTiming{LoadMS: 1380, TranscribeMS: 600, AudioMS: 60000, PeakMemMB: 2683}
		if r.stats.ASRTiming == nil || *r.stats.ASRTiming != wantTiming || r.stats.Status != "" || r.stats.HashVersion != ASRHashVersion {
			t.Errorf("%s: stats %+v timing %+v", c.id, r.stats, r.stats.ASRTiming)
		}
		wav, _ := e.stage(e.deps(nil), model.StageASR).(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
		if r.job.InputHash == nil || *r.job.InputHash != ctxbuild.ASRInputHashV3(wav, "de") {
			t.Errorf("%s: input_hash %v", c.id, r.job.InputHash)
		}
		// the observed peak calibrates the memory admission (I8)
		if v, ok, err := e.st.GetKV(e.ctx, model.MemObservedKey(c.id, c.mdl, c.prec)); err != nil || !ok || v != "2683" {
			t.Errorf("%s: mem_observed %q %v %v", c.id, v, ok, err)
		}
		// the stored stats carry the timing
		if j := e.job(model.ScopeLecture, l.ID, model.StageASR); j.Stats.ASRTiming == nil || *j.Stats.ASRTiming != wantTiming {
			t.Errorf("%s: stored asr_timing %+v", c.id, j.Stats.ASRTiming)
		}
	}
}

// TestASRFasterWhisperDeviceJob: I6 snapshots of cuda:0 and fw:cpu run
// worker/asr.py with config python (python_openvino is not there); the
// precision resolves like compute_type did (SPEC §17.2) and the worker is
// asked for its timing. Both workers' results of the same content give the
// same sentences (contract, I11).
func TestASRFasterWhisperDeviceJob(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASROV)
	log := e.jobLog()
	e.cfg.PythonOpenVINO = filepath.Join(e.root, "no-venv-ov", "python.exe")
	var rows [][]model.Sentence
	for _, c := range []struct{ id, prec, dev, ct string }{
		{model.DeviceCUDA0, "auto", "cuda", "float16"},
		{model.DeviceFWCPU, "auto", "cpu", "int8"},
		{model.DeviceCUDA0, "int8_float16", "cuda", "int8_float16"},
	} {
		l := e.lecture("L "+c.id, "m.mp4")
		e.writeWAV(l, "wav")
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(c.id, "large-v3", c.prec, "en"))
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", c.id, r.err)
		}
		jobs := fakeJobs(t, log)
		got := jobs[len(jobs)-1]
		if got.Script != "asr.py" || got.Job["device"] != c.dev || got.Job["compute_type"] != c.ct || got.Job["model"] != "large-v3" ||
			got.Job["timing"] != true || got.Job["model_dir"] != nil || got.Job["precision"] != nil {
			t.Errorf("%s: %s job.json %v", c.id, got.Script, got.Job)
		}
		if (got.Job["cpu_threads"] == 4.0) != (c.dev == "cuda") {
			t.Errorf("%s: cpu_threads %v", c.id, got.Job["cpu_threads"])
		}
		dir, _ := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
		wavSum, _ := e.stage(e.deps(nil), model.StageASR).(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
		e.checkCheckpointJob(got.Job, map[string]any{"path": filepath.Join(dir, model.ASRCheckpointFile),
			"job_id": r.job.ID, "input_hash": ctxbuild.ASRInputHashV3(wavSum, "en"), "device_id": c.id, "model": "large-v3",
			"precision": c.prec, "chunk_s": 0.0})
		// power management (power-brief §3.7, lead ruling 19): the observed
		// peak of a real faster-whisper run is recorded too (it was OpenVINO
		// only in 3b), for the worker memory budget
		if v, ok, err := e.st.GetKV(e.ctx, model.MemObservedKey(c.id, "large-v3", c.prec)); err != nil || !ok || v != "2683" {
			t.Errorf("%s: mem_observed %q %v %v", c.id, v, ok, err)
		}
		if r.stats.ASRTiming == nil || r.stats.ASRTiming.PeakMemMB != 2683 {
			t.Errorf("%s: asr_timing %+v", c.id, r.stats.ASRTiming)
		}
		rows = append(rows, e.sentences(l.ID))
	}
	// the same result through asr_openvino.py
	e.useFakeWorker("ok", fakeASROV)
	l := e.lecture("L ov", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	ov := e.sentences(l.ID)
	for _, fw := range rows {
		if len(fw) != len(ov) {
			t.Fatalf("%d sentences vs %d", len(fw), len(ov))
		}
		for i := range ov {
			a, b := fw[i], ov[i]
			if a.StartMS != b.StartMS || a.EndMS != b.EndMS || a.ASRText != b.ASRText || a.Text != b.Text ||
				!slices.Equal(a.LowConfWords, b.LowConfWords) || !slices.Equal(a.Words, b.Words) {
				t.Errorf("sentence %d differs between the workers: %+v / %+v", i, a, b)
			}
		}
	}
	// a v1.1 snapshot runs on cuda:0 exactly as before 3b (TestASRWorkerProtocol)
	l2 := e.lecture("L v11", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, model.MarshalSnapshot(model.ASRSnapshot{Model: "medium", ComputeType: "auto", Language: "en"}))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	jobs := fakeJobs(t, log)
	if got := jobs[len(jobs)-1]; got.Script != "asr.py" || got.Job["device"] != "cuda" || got.Job["compute_type"] != "float16" {
		t.Errorf("v1.1 snapshot without device: %s %v", got.Script, got.Job)
	}
}

// TestASRInterpreterMissing: the OpenVINO worker without its interpreter
// fails permanently with devices.ov_python_missing (not worker.python_missing).
func TestASRInterpreterMissing(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASROV)
	missing := filepath.Join(e.root, "no-venv-ov", "Scripts", "python.exe")
	e.cfg.PythonOpenVINO = missing
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	info, _ := state.ParseJobError(deref(r.job.Error))
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != devices.CodeOVPythonMissing ||
		!paramsMatch(info, "path", missing) || !strings.Contains(r.err.Error(), "python_openvino") {
		t.Errorf("err %v, job.error %+v", r.err, info)
	}
	e.cfg.PythonOpenVINO = ""
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); codeOf(r.err) != devices.CodeOVPythonMissing {
		t.Errorf("empty python_openvino: %v (must not fall back to config python)", r.err)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestASRDeviceMissing: a snapshot naming a device or backend this build
// cannot run fails permanently with asr.device_missing {device} (the row's
// label when there is one) and starts no worker.
func TestASRDeviceMissing(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASROV)
	log := e.jobLog()
	e.must(e.st.InsertASRDevice(e.ctx, model.ASRDevice{ID: "ov:VPU", Backend: model.BackendOpenVINO, Label: "Mystery VPU",
		Enabled: true, Model: "small", Precision: "int8", MaxParallel: 1}))
	for _, c := range []struct{ snap, device string }{
		{model.MarshalSnapshot(model.ASRSnapshot{DeviceID: "ov:VPU", Backend: model.BackendOpenVINO, Model: "small", Precision: "int8", Language: "en"}), "Mystery VPU"},
		{model.MarshalSnapshot(model.ASRSnapshot{DeviceID: "cuda:0", Backend: model.BackendOpenVINO, Model: "small", Precision: "int8", Language: "en"}), "cuda:0"},
		{model.MarshalSnapshot(model.ASRSnapshot{DeviceID: "ov:GPU", Backend: "whisper.cpp", Model: "small", Precision: "int8", Language: "en"}), "ov:GPU"},
		{model.MarshalSnapshot(model.ASRSnapshot{DeviceID: "tpu:0", Model: "small", Language: "en"}), "tpu:0"},
	} {
		l := e.lecture("L", "m.mp4")
		e.writeWAV(l, "wav")
		e.setSnapshot(l.ID, model.StageASR, c.snap)
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		info, _ := state.ParseJobError(deref(r.job.Error))
		if !sched.IsPermanent(r.err) || codeOf(r.err) != CodeASRDeviceMissing || !paramsMatch(info, "device", c.device) {
			t.Errorf("%s: err %v, job.error %+v", c.snap, r.err, info)
		}
	}
	if n := len(fakeJobs(t, log)); n != 0 {
		t.Errorf("%d worker runs", n)
	}
}

// TestASROpenVINOStatus: STATUS preparing_model and compiling_model show in
// stats.status (the stage card notes) until the first PROGRESS line.
func TestASROpenVINOStatus(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ov_status", fakeASROV)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	stg := e.stage(e.deps(nil), model.StageASR)
	rec := &sched.StatsRecorder{}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	ctx := sched.WithStatsSink(sched.WithJob(e.ctx, j), rec)
	pr := &progressRec{}
	done := make(chan error, 1)
	go func() { done <- stg.Run(ctx, l.ID, pr.fn) }()
	waitStatus := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for rec.Snapshot().Status != want {
			if time.Now().After(deadline) {
				t.Fatalf("stats.status never became %q (is %q)", want, rec.Snapshot().Status)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitStatus(model.ASRStatusPreparingModel)
	e.must(os.WriteFile(filepath.Join(dir, "compile.flag"), nil, 0o644))
	waitStatus(model.ASRStatusCompilingModel)
	if p := pr.last(); p > 0 {
		t.Errorf("progress %v before the model was loaded", p)
	}
	e.must(os.WriteFile(filepath.Join(dir, "go.flag"), nil, 0o644))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := rec.Snapshot(); s.Status != "" || s.ASRTiming == nil {
		t.Errorf("stats after the run %+v", s)
	}
}

// TestASROpenVINOErrorCodes: ERROR_CODE lines become the asr.ov_* codes
// with their params; exit code 2 is permanent, others retryable; without a
// known ERROR_CODE the asr_ov.worker_* codes apply.
func TestASROpenVINOErrorCodes(t *testing.T) {
	cases := []struct {
		mode      string
		permanent bool
		code      string
		params    []any
		msg       string
	}{
		{"ov_err_device", true, CodeASROVDeviceUnavailable, []any{"device", "Intel(R) Arc(TM) Graphics"}, "vendor 0x8086"},
		{"ov_err_prepare_net", false, CodeASROVModelPrepareFailed, []any{"model", "large-v3-turbo", "precision", "int8"}, "connection reset"},
		{"ov_err_prepare_env", true, CodeASROVModelPrepareFailed, []any{"model", "large-v3-turbo", "precision", "int8"}, "optimum.intel"},
		{"ov_err_space", true, CodeASROVNoSpace, []any{"free_bytes", float64(5368709120), "need_bytes", float64(10737418240)}, "not enough free space"},
		{"ov_err_unknown", false, workerASROV + workerFailed, []any{"exit_code", 1}, "a new failure"},
		{"exit2", true, workerASROV + workerInputError, nil, "audio file cannot be decoded"},
		{"exit1", false, workerASROV + workerFailed, []any{"exit_code", 1}, "CUDA out of memory"},
		{"noout", false, workerASROV + workerNoOutput, nil, "asr_openvino.py"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			e := newEnv(t)
			e.useFakeWorker(c.mode, fakeASROV)
			e.must(e.st.InsertASRDevice(e.ctx, model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO,
				Label: "Intel(R) Arc(TM) Graphics", Enabled: true, Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1}))
			l := e.lecture("L", "m.mp4")
			e.writeWAV(l, "wav")
			e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "int8", "en"))
			r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
			info, _ := state.ParseJobError(deref(r.job.Error))
			if r.err == nil || sched.IsPermanent(r.err) != c.permanent || codeOf(r.err) != c.code || jobErrorCode(r.job) != c.code {
				t.Fatalf("err %v (permanent %v), job.error %v", r.err, sched.IsPermanent(r.err), deref(r.job.Error))
			}
			if !paramsMatch(info, c.params...) || !strings.Contains(r.err.Error(), c.msg) {
				t.Errorf("job.error %+v, err %v", info, r.err)
			}
			if len(e.sentences(l.ID)) != 0 {
				t.Error("sentences written")
			}
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			if _, err := os.Stat(filepath.Join(dir, "result.json.tmp")); !os.IsNotExist(err) {
				t.Error("result.json.tmp left behind")
			}
		})
	}
	// the faster-whisper worker's job errors are unchanged: an ERROR_CODE line is just logged
	e := newEnv(t)
	e.useFakeWorker("ov_err_space", fakeASR)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); codeOf(r.err) != workerASR+workerInputError {
		t.Errorf("asr.py with an ERROR_CODE line: %v", r.err)
	}
}

// TestASROpenVINOCancel (lead ruling 9, replacing I11's chunked cancel):
// killing the worker mid-transcription ends the run within 3 s as
// cancelled, leaves no result (no .tmp, no result.json, no sentences), and
// the next job on the same device runs normally.
func TestASROpenVINOCancel(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("hang_tmp", fakeASROV)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	snap := deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en")
	e.setSnapshot(l.ID, model.StageASR, snap)
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	tmp := filepath.Join(dir, "result.json.tmp")
	ctx, cancel := context.WithCancel(e.ctx)
	at := make(chan time.Time, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(tmp); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		at <- time.Now()
		cancel()
	}()
	r := e.runCtx(ctx, e.stage(e.deps(nil), model.StageASR), l.ID)
	took := time.Since(<-at)
	if r.err != context.Canceled || r.job.Status != model.StatusCancelled {
		t.Fatalf("err %v, status %s", r.err, r.job.Status)
	}
	if took > 3*time.Second {
		t.Errorf("cancel took %v", took)
	}
	for _, f := range []string{"result.json.tmp", "result.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s exists after the cancel", f)
		}
	}
	if len(e.sentences(l.ID)) != 0 {
		t.Error("sentences written")
	}
	// the same device right away
	e.useFakeWorker("ok", fakeASROV)
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, snap)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil || len(e.sentences(l2.ID)) != 14 {
		t.Errorf("next job on the device: %v", r.err)
	}
}

// TestASRFakeResultOpenVINO (I11): in test mode an openvino snapshot reads
// asr.fake_result_openvino (fake_result when it is not set); its sentences
// have no word timestamps, so sentence.words is NULL and the cues of
// CueSentences + SegmentLecture come from the P4.4 interpolation by
// character position.
func TestASRFakeResultOpenVINO(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	e.cfg.ASR.FakeResultOpenVINO = fakeASROV
	ov := e.lecture("OV", "m.mp4")
	e.writeWAV(ov, "wav")
	e.setSnapshot(ov.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	fw := e.lecture("FW", "m.mp4")
	e.writeWAV(fw, "wav")
	e.setSnapshot(fw.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	d := e.deps(nil)
	r := e.run(e.stage(d, model.StageASR), ov.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stats.ASRTiming == nil || r.stats.ASRTiming.PeakMemMB != 2683 {
		t.Errorf("asr_timing %+v", r.stats.ASRTiming)
	}
	// a fake run is no observation of the device's memory
	if _, ok, _ := e.st.GetKV(e.ctx, model.MemObservedKey(model.DeviceOVNPU, "large-v3-turbo", "fp16")); ok {
		t.Error("mem_observed written by a fake run")
	}
	if r := e.run(e.stage(d, model.StageASR), fw.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, ok, _ := e.st.GetKV(e.ctx, model.MemObservedKey(model.DeviceCUDA0, "large-v3", "auto")); ok {
		t.Error("mem_observed written by a fake faster-whisper run")
	}
	withWords := func(ss []model.Sentence) int {
		n := 0
		for _, s := range ss {
			if s.Words != nil {
				n++
			}
		}
		return n
	}
	ovSS, fwSS := e.sentences(ov.ID), e.sentences(fw.ID)
	if len(ovSS) != 14 || withWords(ovSS) != 0 || len(fwSS) != 14 || withWords(fwSS) != 5 {
		t.Fatalf("ov %d sentences (%d with words), fw %d (%d)", len(ovSS), withWords(ovSS), len(fwSS), withWords(fwSS))
	}
	for _, s := range ovSS {
		if len(s.LowConfWords) != 0 {
			t.Errorf("sentence %d: low_conf_words %v", s.Idx, s.LowConfWords)
		}
	}
	checkInterpolated(t, ovSS)

	// a long sentence without words is cut by character position
	long := filepath.Join(e.root, "long.json")
	text := "Today we look at how a set associative cache finds a line, splitting the address into a tag, an index and an offset, and then we compare every tag of the set."
	e.must(os.WriteFile(long, []byte(`{"prompt_terms_used": 0, "sentences": [
		{"start_ms": 2000, "end_ms": 14000, "text": "`+text+`", "low_conf_words": []},
		{"start_ms": 14500, "end_ms": 16000, "text": "Questions?", "low_conf_words": []}]}`), 0o644))
	e.cfg.ASR.FakeResultOpenVINO = long
	l := e.lecture("Long", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	if r := e.run(e.stage(d, model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	ss := e.sentences(l.ID)
	if withWords(ss) != 0 {
		t.Fatal("words stored")
	}
	cues := checkInterpolated(t, ss)
	if n := countCues(cues, 0); n < 2 {
		t.Errorf("the 155-character sentence gave %d cues", n)
	}

	// without fake_result_openvino, openvino snapshots read fake_result
	e.cfg.ASR.FakeResultOpenVINO = ""
	l3 := e.lecture("Fallback", "m.mp4")
	e.writeWAV(l3, "wav")
	e.setSnapshot(l3.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	if r := e.run(e.stage(d, model.StageASR), l3.ID); r.err != nil || withWords(e.sentences(l3.ID)) != 5 {
		t.Errorf("fallback to fake_result: %v", r.err)
	}
}

func countCues(cues []subs.Cue, idx int) int {
	n := 0
	for _, c := range cues {
		if c.SentenceIdx == idx {
			n++
		}
	}
	return n
}

// checkInterpolated asserts that every cue of sentences without words
// starts where the P4.4 rule puts it: the sentence's start plus its
// duration × the character offset of the cue's first word ÷ the
// sentence's length (words joined by single spaces).
func checkInterpolated(t *testing.T, ss []model.Sentence) []subs.Cue {
	t.Helper()
	cues := subs.SegmentLecture(CueSentences(ss))
	at := map[int]int{} // sentence idx → index of its next word
	for _, c := range cues {
		s := ss[c.SentenceIdx]
		if s.Words != nil {
			continue
		}
		toks := strings.Fields(s.Text)
		off, span := 0, -1
		for k, tk := range toks {
			if k == at[c.SentenceIdx] {
				off = span + 1
			}
			span += len([]rune(tk)) + 1
		}
		want := s.StartMS + (s.EndMS-s.StartMS)*int64(off)/int64(span)
		if c.StartMS != want {
			t.Errorf("sentence %d cue %q starts at %d, want %d (character %d of %d)", s.Idx, c.En, c.StartMS, want, off, span)
		}
		at[c.SentenceIdx] += len(strings.Fields(c.En))
	}
	return cues
}

// TestASRHashV3 (SPEC-v2 I6 revising A1): a new run's input hash depends on
// the wav and the language only — changing the device or the model of the
// snapshot never makes asr stale; a job recorded with the v1.1 formula
// (hash_version 2) keeps being compared with it.
func TestASRHashV3(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "RIFF one")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	stg := e.stage(e.deps(nil), model.StageASR)
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stats.HashVersion != 3 {
		t.Fatalf("hash_version %d", r.stats.HashVersion)
	}
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	wav, _ := stg.(*asrStage).wavSHA256(filepath.Join(dir, model.AudioFile))
	want := ctxbuild.ASRInputHashV3(wav, "en")
	if r.job.InputHash == nil || *r.job.InputHash != want {
		t.Fatalf("input_hash %v, want %s", r.job.InputHash, want)
	}
	hashers := map[string]state.Hasher{model.StageASR: stg}
	notStale := func(what string) {
		t.Helper()
		if h, _ := stg.InputHash(e.ctx, l.ID); h != want {
			t.Errorf("%s changed the hash", what)
		}
		if stale, err := state.CheckStale(e.ctx, e.st, hashers, l.ID); err != nil || len(stale) != 0 {
			t.Errorf("%s: stale %v %v", what, staleStages(stale), err)
		}
	}
	for _, snap := range []string{
		deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"), // another device and model
		deviceSnapshot(model.DeviceCUDA0, "large-v2", "int8_float16", "en"),
		model.MarshalSnapshot(model.ASRSnapshot{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "small",
			Precision: "int8", Language: "en", Auto: true}),
		model.MarshalSnapshot(model.ASRSnapshot{Model: "tiny", ComputeType: "float16", Device: "cpu", Language: "en"}),
	} {
		e.setSnapshot(l.ID, model.StageASR, snap)
		notStale(snap)
	}
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "de"))
	if h, _ := stg.InputHash(e.ctx, l.ID); h == want || h != ctxbuild.ASRInputHashV3(wav, "de") {
		t.Error("the language is not part of the v3 hash")
	}
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	e.writeWAV(l, "RIFF two, longer")
	if stale, err := state.CheckStale(e.ctx, e.st, hashers, l.ID); err != nil || !slices.Equal(staleStages(stale), []string{model.StageASR}) {
		t.Errorf("new audio: stale %v %v", staleStages(stale), err)
	}

	// a v1.1 job (hash_version 2, v1.1 snapshot) keeps the old formula
	l2 := e.lecture("Old", "m.mp4")
	e.writeWAV(l2, "RIFF old")
	old := model.MarshalSnapshot(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"})
	e.setSnapshot(l2.ID, model.StageASR, old)
	e.markDone(model.ScopeLecture, l2.ID, model.StageASR)
	j := e.job(model.ScopeLecture, l2.ID, model.StageASR)
	wav2, _ := stg.(*asrStage).wavSHA256(filepath.Join(model.LectureDir(e.cfg.DataDir, l2.CourseID, l2.ID), model.AudioFile))
	v2 := ctxbuild.ASRInputHash(wav2, ctxbuild.ASRParams{Model: "large-v2", ComputeType: "auto", Language: "en"})
	e.must(e.st.SetJobStats(e.ctx, j.ID, model.JobStats{HashVersion: 2}))
	e.must(e.st.SetJobInputHash(e.ctx, j.ID, v2))
	if h, err := stg.InputHash(e.ctx, l2.ID); err != nil || h != v2 {
		t.Errorf("v1.1 job: InputHash %q %v, want the v1.1 hash", h, err)
	}
	if stale, err := state.CheckStale(e.ctx, e.st, hashers, l2.ID); err != nil || len(stale) != 0 {
		t.Errorf("v1.1 job marked stale: %v %v", staleStages(stale), err)
	}
	// its snapshot normalizes to cuda:0 (lead ruling 2) but the bytes, and
	// so the old hash, stay as stored
	if n := model.NormalizeASRSnapshot(must1(model.ParseASRSnapshot(old))); n.DeviceID != model.DeviceCUDA0 || n.Precision != "auto" {
		t.Errorf("normalized %+v", n)
	}
}

func must1[T any](v T, _ bool) T { return v }
