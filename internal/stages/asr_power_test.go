// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// Power management, task P3 (.claude/power-brief.md §3.4, §3.8, §4): the asr
// stage passes the checkpoint object, keeps the checkpoint through cancels
// and power interrupts, records the parts of a resumed run in the snapshot
// and removes the checkpoint after a successful run. The test binary plays
// the workers in mode "ckpt" (helpers_test.go).

// checkCheckpointJob compares job.json's checkpoint object with want
// (JSON-decoded values).
func (e *env) checkCheckpointJob(job map[string]any, want map[string]any) {
	e.t.Helper()
	got, ok := job["checkpoint"].(map[string]any)
	if !ok {
		e.t.Errorf("job.json without a checkpoint object: %v", job)
		return
	}
	if !slices.Equal(sortedKeys(got), sortedKeys(want)) {
		e.t.Errorf("checkpoint keys %v, want %v", sortedKeys(got), sortedKeys(want))
	}
	for k, v := range want {
		if got[k] != v {
			e.t.Errorf("checkpoint.%s = %v, want %v", k, got[k], v)
		}
	}
}

// lectureDir is the absolute directory of l.
func (e *env) lectureDir(l model.Lecture) string {
	e.t.Helper()
	dir, err := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
	if err != nil {
		e.t.Fatal(err)
	}
	return dir
}

// startASR moves the lecture's asr job to running (from pending, queued,
// failed or cancelled) and returns the context the scheduler gives Run (the
// job and a stats sink on top of ctx).
func (e *env) startASR(ctx context.Context, l model.Lecture) (context.Context, model.Job) {
	e.t.Helper()
	rctx, j, _ := e.startASRRec(ctx, l)
	return rctx, j
}

// startASRRec is startASR that also returns the stats sink.
func (e *env) startASRRec(ctx context.Context, l model.Lecture) (context.Context, model.Job, *sched.StatsRecorder) {
	e.t.Helper()
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	switch j.Status {
	case model.StatusPending, model.StatusFailed, model.StatusCancelled:
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, j.Status, model.StatusQueued, db.JobFinish{}))
	case model.StatusQueued:
	default:
		e.t.Fatalf("asr job is %s", j.Status)
	}
	e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusQueued, model.StatusRunning, db.JobFinish{}))
	j, _ = e.st.GetJob(e.ctx, j.ID)
	rec := &sched.StatsRecorder{Stats: j.Stats}
	return sched.WithStatsSink(sched.WithJob(ctx, j), rec), j, rec
}

// killAtFlag runs the asr stage of l with a worker that stops after some
// windows (LF_FAKE_CKPT_STOP) and cancels the run once the worker wrote
// ckpt.flag: with cause (nil = a plain cancel). It returns the stage's
// error; the job is left running, with the stats the run recorded
// persisted (as the scheduler's finish does on every outcome).
func (e *env) killAtFlag(l model.Lecture, windows string, cause error) error {
	e.t.Helper()
	e.t.Setenv(fakeCkptStopEnv, windows)
	defer os.Unsetenv(fakeCkptStopEnv)
	flag := filepath.Join(e.lectureDir(l), "ckpt.flag")
	os.Remove(flag)
	ctx, cancel := context.WithCancelCause(e.ctx)
	defer cancel(nil)
	rctx, j, rec := e.startASRRec(ctx, l)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(flag); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel(cause)
	}()
	started := time.Now()
	err := e.stage(e.deps(nil), model.StageASR).Run(rctx, l.ID, func(float64) {})
	if took := time.Since(started); took > 15*time.Second {
		e.t.Errorf("the run took %v: the worker was not killed", took)
	}
	e.must(e.st.SetJobStats(e.ctx, j.ID, rec.Snapshot()))
	return err
}

func marshalT(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sameSentences compares the transcribed content of two lectures.
func sameSentences(t *testing.T, got, want []model.Sentence) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d sentences, want %d", len(got), len(want))
	}
	for i := range want {
		a, b := got[i], want[i]
		if a.StartMS != b.StartMS || a.EndMS != b.EndMS || a.ASRText != b.ASRText || a.Text != b.Text ||
			!slices.Equal(a.LowConfWords, b.LowConfWords) || !slices.Equal(a.Words, b.Words) {
			t.Errorf("sentence %d: %+v, want %+v", i, a, b)
		}
	}
}

// storedSnapshot is the asr job's snapshot as stored.
func (e *env) storedSnapshot(l model.Lecture) (string, model.ASRSnapshot) {
	e.t.Helper()
	raw := e.job(model.ScopeLecture, l.ID, model.StageASR).ConfigSnapshot
	s, ok := model.ParseASRSnapshot(raw)
	if !ok {
		e.t.Fatalf("snapshot %q", raw)
	}
	return raw, s
}

// TestASRCheckpointJobChunk: the OpenVINO worker gets chunk_s =
// devices.OVCheckpointChunkS (here changed to 30), faster-whisper always 0;
// every pipeline run of the same job passes the same job id.
func TestASRCheckpointJobChunk(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASROV)
	log := e.jobLog()
	for _, c := range []struct {
		id    string
		chunk float64
	}{{model.DeviceOVGPU, 30}, {model.DeviceOVNPU, 30}, {model.DeviceCUDA0, 0}, {model.DeviceFWCPU, 0}} {
		l := e.lecture("L "+c.id, "m.mp4")
		e.writeWAV(l, "wav "+c.id)
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(c.id, "large-v3-turbo", map[bool]string{true: "int8", false: "auto"}[c.chunk > 0], "en"))
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", c.id, r.err)
		}
		jobs := fakeJobs(t, log)
		got := jobs[len(jobs)-1].Job["checkpoint"].(map[string]any)
		if got["chunk_s"] != c.chunk || got["job_id"] != r.job.ID || got["device_id"] != c.id {
			t.Errorf("%s: checkpoint %v", c.id, got)
		}
	}
}

// TestASRCheckpointUninterrupted: a run that was never interrupted returns
// one part — the snapshot stays byte for byte — and its checkpoint (and a
// stray .tmp) is removed once the sentences are stored. In test mode
// (fake_result) no checkpoint is involved.
func TestASRCheckpointUninterrupted(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	log := e.jobLog()
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	before, _ := e.storedSnapshot(l)
	dir := e.lectureDir(l)
	e.must(os.WriteFile(filepath.Join(dir, model.ASRCheckpointFile+".tmp"), []byte("{"), 0o644))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if after, _ := e.storedSnapshot(l); after != before {
		t.Errorf("snapshot changed by a one-part run:\n%s\n%s", before, after)
	}
	for _, f := range []string{model.ASRCheckpointFile, model.ASRCheckpointFile + ".tmp"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a successful run", f)
		}
	}
	if n := len(fakeJobs(t, log)); n != 1 {
		t.Fatalf("%d worker runs", n)
	}
	// the worker did write a checkpoint while it ran: its result has the one part
	var ref struct{ Parts []model.ASRPart }
	b, _ := os.ReadFile(filepath.Join(dir, "result.json"))
	e.must(json.Unmarshal(b, &ref))
	if len(ref.Parts) != 1 || ref.Parts[0] != (model.ASRPart{FromMS: 0, ToMS: 60000, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: "auto"}) {
		t.Errorf("parts %+v", ref.Parts)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Errorf("%d sentences", len(e.sentences(l.ID)))
	}

	// test mode: no worker, no checkpoint (a file there is not touched)
	e.cfg.ASR.FakeResult = fakeASR
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "wav")
	stray := filepath.Join(e.lectureDir(l2), model.ASRCheckpointFile)
	e.must(os.WriteFile(stray, []byte("{}"), 0o644))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("test mode touched the checkpoint: %v", err)
	}
}

// TestASRCheckpointCancelRetry: a user cancel kills the worker and keeps the
// checkpoint; the retry is the same job, so the worker continues from it
// (on the same device, model and precision; without a power interrupt any
// other configuration starts over, TestASRCheckpointConfigChanged). The
// result's two parts go
// into the snapshot in the sentences' transaction, every other snapshot
// field as stored; the sentences equal an uninterrupted run's; the
// checkpoint is removed.
func TestASRCheckpointCancelRetry(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	log := e.jobLog()
	ref := e.lecture("Uninterrupted", "m.mp4")
	e.writeWAV(ref, "wav")
	e.setSnapshot(ref.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), ref.ID); r.err != nil {
		t.Fatal(r.err)
	}

	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	err := e.killAtFlag(l, "3", nil)
	if err != context.Canceled || sched.IsPowerInterrupt(err) {
		t.Fatalf("user cancel: %v", err)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusRunning, model.StatusCancelled, db.JobFinish{}))
	ck, ok := e.readCheckpoint(l)
	if !ok {
		t.Fatal("the cancel removed the checkpoint")
	}
	if ck.JobID != j.ID || ck.ResumeMS != 12600 || len(ck.Segments) != 3 ||
		!slices.Equal(ck.Parts, []model.ASRPart{{FromMS: 0, ToMS: 12600, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: "auto"}}) {
		t.Errorf("checkpoint %+v", ck)
	}
	if len(e.sentences(l.ID)) != 0 {
		t.Error("sentences written by the cancelled run")
	}

	// the user's retry (SPEC §4.6: the same job, attempt+1), bound to the
	// same device, model and precision
	e.must(state.Retry(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageASR))
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	_, before := e.storedSnapshot(l)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.job.ID != j.ID || r.job.Attempt != j.Attempt+1 {
		t.Errorf("job %s attempt %d, want %s / %d", r.job.ID, r.job.Attempt, j.ID, j.Attempt+1)
	}
	jobs := fakeJobs(t, log)
	for _, k := range jobs[len(jobs)-2:] {
		if id := k.Job["checkpoint"].(map[string]any)["job_id"]; id != j.ID {
			t.Errorf("checkpoint.job_id %v, want %s", id, j.ID)
		}
	}
	_, after := e.storedSnapshot(l)
	want := before
	want.Parts = []model.ASRPart{
		{FromMS: 0, ToMS: 12600, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: "auto"},
		{FromMS: 12600, ToMS: 60000, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: "auto"},
	}
	if !model.EqualASRSnapshot(after, want) {
		t.Errorf("snapshot %+v, want %+v", after, want)
	}
	sameSentences(t, e.sentences(l.ID), e.sentences(ref.ID))
	if _, ok := e.readCheckpoint(l); ok {
		t.Error("checkpoint left after the successful retry")
	}
	if r.stats.PromptTermsUsed != 3 || r.stats.HashVersion != ASRHashVersion {
		t.Errorf("stats %+v", r.stats)
	}

	// a checkpoint of another job (a rerun is a new job) is void: the
	// worker starts over, one part, the snapshot is unchanged
	l2 := e.lecture("L2", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	e.must(os.WriteFile(filepath.Join(e.lectureDir(l2), model.ASRCheckpointFile), marshalT(t, devices.Checkpoint{
		Version: devices.CheckpointVersion, JobID: "j-older", InputHash: "x", Backend: model.BackendFasterWhisper, ResumeMS: 12600,
		Parts: []model.ASRPart{{FromMS: 0, ToMS: 12600, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: "auto"}},
	}), 0o644))
	raw2, _ := e.storedSnapshot(l2)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if after, _ := e.storedSnapshot(l2); after != raw2 {
		t.Errorf("snapshot of a fresh run changed: %s", after)
	}
	sameSentences(t, e.sentences(l2.ID), e.sentences(ref.ID))
}

// TestASRCheckpointRecovery (§4.7): the server stops while asr runs (the
// run's context is cancelled, the job stays running); recovery requeues
// the same job and removes *.tmp but keeps the complete checkpoint, so the
// next run continues from it.
func TestASRCheckpointRecovery(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	if err := e.killAtFlag(l, "5", nil); err != context.Canceled {
		t.Fatalf("shutdown: %v", err)
	}
	dir := e.lectureDir(l)
	e.must(os.WriteFile(filepath.Join(dir, model.ASRCheckpointFile+".tmp"), []byte("{\"version\":"), 0o644))
	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	if _, err := os.Stat(filepath.Join(dir, model.ASRCheckpointFile+".tmp")); !os.IsNotExist(err) {
		t.Error("recovery left the partial checkpoint")
	}
	if ck, ok := e.readCheckpoint(l); !ok || ck.ResumeMS != 20400 {
		t.Fatalf("checkpoint after recovery %+v %v", ck, ok)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	if j.Status != model.StatusQueued {
		t.Fatalf("recovered job %s", j.Status)
	}
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil || r.job.ID != j.ID {
		t.Fatal(r.err)
	}
	_, s := e.storedSnapshot(l)
	if len(s.Parts) != 2 || s.Parts[0].ToMS != 20400 || s.Parts[1].FromMS != 20400 || s.Parts[1].DeviceID != model.DeviceCUDA0 {
		t.Errorf("parts %+v", s.Parts)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Errorf("%d sentences", len(e.sentences(l.ID)))
	}
}

// TestASRPowerInterrupt (power-brief §3.4): the scheduler cancels the run
// with sched.ErrPowerMigrate / ErrPowerSuspend as the cause; the stage
// returns an error wrapping it (sched.IsPowerInterrupt, not a plain
// cancel) and keeps the checkpoint. After the scheduler's power requeue
// (powerRequeue: stats.asr_resume, state.PowerRequeue) the same job
// continues: Intel GPU → NPU from the checkpoint (OpenVINO with a chunk
// length), CUDA later on the same device.
func TestASRPowerInterrupt(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASROV)

	// migrate: ov:GPU → ov:NPU
	l := e.lecture("Migrate", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	err := e.killAtFlag(l, "2", sched.ErrPowerMigrate)
	if !sched.IsPowerInterrupt(err) || !errors.Is(err, sched.ErrPowerMigrate) || errors.Is(err, sched.ErrPowerSuspend) {
		t.Fatalf("migrate: %v", err)
	}
	ck, ok := e.readCheckpoint(l)
	if !ok || ck.ResumeMS != 8200 || ck.Backend != model.BackendOpenVINO {
		t.Fatalf("checkpoint after the power interrupt %+v %v", ck, ok)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.powerRequeue(j.ID, model.DeviceOVGPU, sched.CodePowerSwitched, model.DeviceOVNPU) // what the scheduler's finish does (task P1)
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	_, before := e.storedSnapshot(l)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := before
	want.Parts = []model.ASRPart{
		{FromMS: 0, ToMS: 8200, DeviceID: model.DeviceOVGPU, Model: "large-v3-turbo", Precision: "fp16"},
		{FromMS: 8200, ToMS: 60000, DeviceID: model.DeviceOVNPU, Model: "large-v3-turbo", Precision: "fp16"},
	}
	if _, after := e.storedSnapshot(l); !model.EqualASRSnapshot(after, want) {
		t.Errorf("snapshot %+v, want %+v", after, want)
	}
	if len(e.sentences(l.ID)) != 14 {
		t.Errorf("%d sentences", len(e.sentences(l.ID)))
	}
	if _, ok := e.readCheckpoint(l); ok {
		t.Error("checkpoint left after the run finished")
	}

	// suspend: cuda:0, continued on cuda:0 once the charger is back
	e.useFakeWorker("ckpt", fakeASR)
	l2 := e.lecture("Suspend", "m.mp4")
	e.writeWAV(l2, "wav")
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	err = e.killAtFlag(l2, "4", sched.ErrPowerSuspend)
	if !sched.IsPowerInterrupt(err) || !errors.Is(err, sched.ErrPowerSuspend) {
		t.Fatalf("suspend: %v", err)
	}
	if ck, ok := e.readCheckpoint(l2); !ok || ck.ResumeMS != 16000 {
		t.Fatalf("checkpoint %+v %v", ck, ok)
	}
	j2 := e.job(model.ScopeLecture, l2.ID, model.StageASR)
	e.powerRequeue(j2.ID, model.DeviceCUDA0, sched.CodePowerSuspended, "")
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, s := e.storedSnapshot(l2); len(s.Parts) != 2 || s.Parts[0].DeviceID != model.DeviceCUDA0 || s.Parts[1].DeviceID != model.DeviceCUDA0 ||
		s.Parts[0].ToMS != 16000 || s.Parts[1].FromMS != 16000 || s.DeviceID != model.DeviceCUDA0 {
		t.Errorf("snapshot %+v", s)
	}

	// the OpenVINO worker without chunking (chunk_s 0) has no checkpoint:
	// the next device starts over and the run has one part
	ovCheckpointChunkS = 0
	e.useFakeWorker("ckpt", fakeASROV)
	l3 := e.lecture("No chunks", "m.mp4")
	e.writeWAV(l3, "wav")
	e.setSnapshot(l3.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "large-v3-turbo", "fp16", "en"))
	if err := e.killAtFlag(l3, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("migrate without chunks: %v", err)
	}
	if _, ok := e.readCheckpoint(l3); ok {
		t.Error("a checkpoint without chunking")
	}
	e.powerRequeue(e.job(model.ScopeLecture, l3.ID, model.StageASR).ID, model.DeviceOVGPU, sched.CodePowerSwitched, model.DeviceOVNPU)
	e.setSnapshot(l3.ID, model.StageASR, deviceSnapshot(model.DeviceOVNPU, "large-v3-turbo", "fp16", "en"))
	raw3, _ := e.storedSnapshot(l3)
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l3.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if after, _ := e.storedSnapshot(l3); after != raw3 {
		t.Errorf("snapshot of a restarted run changed: %s", after)
	}
}

// TestASRPowerInterruptAnyStep: the power cause is reported whatever the
// run was doing when the context ended (here before the fake_result copy
// of test mode even started); a cancel without a power cause stays
// context.Canceled.
func TestASRPowerInterruptAnyStep(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	for _, cause := range []error{sched.ErrPowerMigrate, sched.ErrPowerSuspend} {
		ctx, cancel := context.WithCancelCause(e.ctx)
		rctx, j := e.startASR(ctx, l)
		cancel(cause)
		err := e.stage(e.deps(nil), model.StageASR).Run(rctx, l.ID, func(float64) {})
		if !errors.Is(err, cause) || !sched.IsPowerInterrupt(err) {
			t.Errorf("%v: %v", cause, err)
		}
		e.must(state.PowerRequeue(e.ctx, e.st, j.ID))
	}
	ctx, cancel := context.WithCancel(e.ctx)
	rctx, _ := e.startASR(ctx, l)
	cancel()
	if err := e.stage(e.deps(nil), model.StageASR).Run(rctx, l.ID, func(float64) {}); sched.IsPowerInterrupt(err) || !errors.Is(err, context.Canceled) {
		t.Errorf("plain cancel: %v", err)
	}
	if len(e.sentences(l.ID)) != 0 {
		t.Error("sentences written")
	}
}
