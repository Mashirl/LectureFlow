// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// Power management backend Gate round 1, item 4: a checkpoint is continued
// with another device, model or precision only after a power interrupt
// (discardForeignCheckpoint, checkpointResumable).

// powerRequeue does what the scheduler's finish does after the policy
// interrupted the run on device (power-brief §3.4, task P1): the stats get
// power_interrupts + 1, asr_resume = device and power_event (code, to),
// then state.PowerRequeue.
func (e *env) powerRequeue(jobID, device, code, to string) {
	e.t.Helper()
	j, err := e.st.GetJob(e.ctx, jobID)
	e.must(err)
	st := j.Stats
	at := "2026-10-24T07:00:00Z"
	st.PowerInterrupts++
	st.ASRResume = &model.ASRResume{DeviceID: device, Backend: model.DeviceBackend(device), At: at}
	st.PowerEvent = &model.PowerEvent{Code: code, At: at, FromDevice: device, ToDevice: to}
	e.must(e.st.SetJobStats(e.ctx, jobID, st))
	e.must(state.PowerRequeue(e.ctx, e.st, jobID))
}

// resultParts reads the parts of the lecture's last result.json.
func (e *env) resultParts(l model.Lecture) []model.ASRPart {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.lectureDir(l), "result.json"))
	e.must(err)
	var res struct{ Parts []model.ASRPart }
	e.must(json.Unmarshal(b, &res))
	return res.Parts
}

// TestDiscardForeignCheckpoint: before the worker starts, a checkpoint is
// kept when its last part ran with exactly the run's device, model and
// precision, or after a power interrupt (stats.asr_resume) when the run is
// bound to another device than the last part's (a migration: model and
// precision may differ). Another model or precision on the same device
// starts over even after a power interrupt. Every other checkpoint is
// removed together with its .tmp, and the removal is logged with both
// configurations. A file the stage cannot parse as a checkpoint is left to
// the worker, and so are the worker's own conditions (here: another job id).
func TestDiscardForeignCheckpoint(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	npu := model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: model.PrecisionFP16}
	run := model.NormalizeASRSnapshot(model.ASRDeviceSnapshot(npu, "en", true))
	part := func(from, to int64, dev, mdl, prec string) model.ASRPart {
		return model.ASRPart{FromMS: from, ToMS: to, DeviceID: dev, Model: mdl, Precision: prec}
	}
	same := part(0, 8200, npu.ID, npu.Model, npu.Precision)
	ckpt := func(jobID string, parts ...model.ASRPart) string {
		if parts == nil {
			parts = []model.ASRPart{}
		}
		return string(marshalT(t, devices.Checkpoint{Version: devices.CheckpointVersion, JobID: jobID, InputHash: "h",
			Backend: model.BackendOpenVINO, ResumeMS: 8200, Parts: parts, Segments: []devices.CheckpointSegment{}}))
	}
	// the asr_resume of the last power interrupt (the device it happened on)
	onGPU := &model.ASRResume{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, At: "2026-10-24T07:00:00Z"}
	onNPU := &model.ASRResume{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, At: "2026-10-24T07:00:00Z"}
	// gone is the log's tail for a removed checkpoint whose last part ran
	// with (dev, mdl, prec): both configurations, in this order
	gone := func(dev, mdl, prec string) string {
		q := func(s string) string {
			if s == "" {
				return `""`
			}
			return s
		}
		return fmt.Sprintf("checkpoint_device=%s checkpoint_model=%s checkpoint_precision=%s device=ov:NPU model=large-v3-turbo precision=fp16",
			q(dev), q(mdl), q(prec))
	}
	gpu, int8 := model.DeviceOVGPU, model.PrecisionINT8
	for _, c := range []struct {
		name   string
		file   string // the checkpoint's content; "" = no file (only the .tmp)
		resume *model.ASRResume
		gone   string // "" = kept; else the expected log tail (gone(...))
	}{
		// no power interrupt: only the same configuration continues
		{"same configuration", ckpt("j1", same), nil, ""},
		{"precision changed", ckpt("j1", part(0, 8200, npu.ID, npu.Model, int8)), nil, gone(npu.ID, npu.Model, int8)},
		{"model changed", ckpt("j1", part(0, 8200, npu.ID, "large-v3", npu.Precision)), nil, gone(npu.ID, "large-v3", npu.Precision)},
		{"device changed", ckpt("j1", part(0, 8200, gpu, npu.Model, npu.Precision)), nil, gone(gpu, npu.Model, npu.Precision)},
		{"earlier part differs, last part the same",
			ckpt("j1", part(0, 3800, gpu, "large-v3", int8), part(3800, 8200, npu.ID, npu.Model, npu.Precision)), nil, ""},
		{"earlier part the same, last part differs",
			ckpt("j1", part(0, 3800, npu.ID, npu.Model, npu.Precision), part(3800, 8200, gpu, npu.Model, npu.Precision)), nil,
			gone(gpu, npu.Model, npu.Precision)},
		{"no parts", ckpt("j1"), nil, gone("", "", "")},
		// power interrupt: another device continues whatever its
		// configuration, the same device only with the same configuration
		{"power: migrated from another device, model and precision", ckpt("j1", part(0, 8200, gpu, "large-v3", int8)), onGPU, ""},
		{"power: migrated from another device, same model and precision", ckpt("j1", part(0, 8200, gpu, npu.Model, npu.Precision)), onGPU, ""},
		{"power: second migration back to an earlier device (NPU → GPU → NPU)",
			ckpt("j1", part(0, 3800, npu.ID, npu.Model, npu.Precision), part(3800, 8200, gpu, "large-v3", int8)), onGPU, ""},
		{"power: same device and configuration (suspended, resumed)", ckpt("j1", same), onNPU, ""},
		{"power: same device, precision changed by the user", ckpt("j1", part(0, 8200, npu.ID, npu.Model, int8)), onNPU,
			gone(npu.ID, npu.Model, int8)},
		{"power: same device, model changed by the user", ckpt("j1", part(0, 8200, npu.ID, "large-v3", npu.Precision)), onNPU,
			gone(npu.ID, "large-v3", npu.Precision)},
		{"power on another device, but the last part ran here with another precision",
			ckpt("j1", part(0, 3800, gpu, "large-v3", int8), part(3800, 8200, npu.ID, npu.Model, int8)), onGPU,
			gone(npu.ID, npu.Model, int8)},
		{"power: no parts", ckpt("j1"), onGPU, gone("", "", "")},
		// the worker's business
		{"another job, same configuration (the worker's rule)", ckpt("j-older", same), nil, ""},
		{"unparsable (the worker's rule)", `{"version":`, nil, ""},
		{"not an object (the worker's rule)", `[1]`, onGPU, ""},
		{"no checkpoint, only a partial one", "", nil, ""},
	} {
		keep := c.gone == ""
		dir := t.TempDir()
		main, tmp := filepath.Join(dir, model.ASRCheckpointFile), filepath.Join(dir, model.ASRCheckpointFile+".tmp")
		if c.file != "" {
			if err := os.WriteFile(main, []byte(c.file), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(tmp, []byte(`{"version":1,`), 0o644); err != nil {
			t.Fatal(err)
		}
		logBuf.Reset()
		discardForeignCheckpoint(model.Job{ID: "j1", ScopeID: "l1", Stage: model.StageASR, Stats: model.JobStats{ASRResume: c.resume}}, run, dir)
		b, err := os.ReadFile(main)
		switch {
		case c.file == "":
			if !os.IsNotExist(err) {
				t.Errorf("%s: a checkpoint appeared: %v", c.name, err)
			}
		case keep:
			if err != nil || string(b) != c.file {
				t.Errorf("%s: checkpoint not kept as it was: %q %v", c.name, b, err)
			}
		default:
			if !os.IsNotExist(err) {
				t.Errorf("%s: checkpoint not removed: %v", c.name, err)
			}
		}
		if _, err := os.Stat(tmp); keep != (err == nil) {
			t.Errorf("%s: .tmp exists %v, want %v", c.name, err == nil, keep)
		}
		got := logBuf.String()
		if keep && got != "" {
			t.Errorf("%s: logged %q", c.name, got)
		}
		if !keep {
			if n := strings.Count(got, "\n"); n != 1 {
				t.Errorf("%s: %d log lines: %q", c.name, n, got)
			}
			for _, want := range []string{"level=INFO", "checkpoint not continued", "job=j1 lecture=l1 checkpoint_job=j1",
				fmt.Sprintf("power_interrupt=%v", c.resume != nil), c.gone} {
				if !strings.Contains(got, want) {
					t.Errorf("%s: log %q without %q", c.name, got, want)
				}
			}
		}
	}
}

// TestASRCheckpointConfigChanged: a user cancel keeps the checkpoint and the
// retry is the same job, but the scheduler binds the device row's current
// values to the auto snapshot at every start. When the user changed the
// row's precision or model, or the retry was bound to another device, and
// there was no power interrupt, the stage removes the checkpoint before the
// worker starts: the lecture is transcribed from the start with the new
// configuration only (one part, the snapshot as bound). With the row
// unchanged the retry continues from the checkpoint (two parts).
func TestASRCheckpointConfigChanged(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASR)
	log := e.jobLog()
	ref := e.lecture("Uninterrupted", "m.mp4")
	e.writeWAV(ref, "wav")
	e.setSnapshot(ref.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "auto", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), ref.ID); r.err != nil {
		t.Fatal(r.err)
	}

	first := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: model.PrecisionFloat16}
	for _, c := range []struct {
		name string
		dev  model.ASRDevice // the row bound at the retry's start
		keep bool
	}{
		{"precision", model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: model.PrecisionInt8Float16}, false},
		{"model", model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3-turbo", Precision: model.PrecisionFloat16}, false},
		{"device", model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: model.PrecisionFloat16}, false},
		{"unchanged", first, true},
	} {
		l := e.lecture("L "+c.name, "m.mp4")
		e.writeWAV(l, "wav")
		e.setSnapshot(l.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(first, "en", true)))
		if err := e.killAtFlag(l, "3", nil); err != context.Canceled {
			t.Fatalf("%s: user cancel: %v", c.name, err)
		}
		j := e.job(model.ScopeLecture, l.ID, model.StageASR)
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusRunning, model.StatusCancelled, db.JobFinish{}))
		old := model.ASRPart{FromMS: 0, ToMS: 12600, DeviceID: first.ID, Model: first.Model, Precision: first.Precision}
		if ck, ok := e.readCheckpoint(l); !ok || ck.JobID != j.ID || ck.ResumeMS != 12600 || !slices.Equal(ck.Parts, []model.ASRPart{old}) {
			t.Fatalf("%s: checkpoint after the cancel %+v %v", c.name, ck, ok)
		}
		if j.Stats.ASRResume != nil {
			t.Fatalf("%s: asr_resume after a user cancel: %+v", c.name, j.Stats.ASRResume)
		}

		// the user's retry (the same job, attempt+1); the scheduler binds
		// the row at the start (state.BindASRDevice)
		e.must(state.Retry(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageASR))
		e.must(state.BindASRDevice(e.ctx, e.st, j.ID, c.dev))
		bound, boundSnap := e.storedSnapshot(l)
		if boundSnap.DeviceID != c.dev.ID || boundSnap.Model != c.dev.Model || boundSnap.Precision != c.dev.Precision || !boundSnap.Auto {
			t.Fatalf("%s: bound snapshot %s", c.name, bound)
		}
		r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", c.name, r.err)
		}
		if r.job.ID != j.ID || r.job.Attempt != j.Attempt+1 {
			t.Errorf("%s: job %s attempt %d, want %s / %d", c.name, r.job.ID, r.job.Attempt, j.ID, j.Attempt+1)
		}
		jobs := fakeJobs(t, log)
		e.checkCheckpointJob(jobs[len(jobs)-1].Job, map[string]any{
			"path": filepath.Join(e.lectureDir(l), model.ASRCheckpointFile), "job_id": j.ID,
			"input_hash": jobs[len(jobs)-2].Job["checkpoint"].(map[string]any)["input_hash"],
			"device_id":  c.dev.ID, "model": c.dev.Model, "precision": c.dev.Precision, "chunk_s": 0.0})
		cur := model.ASRPart{DeviceID: c.dev.ID, Model: c.dev.Model, Precision: c.dev.Precision}
		if c.keep {
			cur.FromMS, cur.ToMS = 12600, 60000
			wantParts := []model.ASRPart{old, cur}
			if got := e.resultParts(l); !slices.Equal(got, wantParts) {
				t.Errorf("%s: result parts %+v, want %+v", c.name, got, wantParts)
			}
			want := boundSnap
			want.Parts = wantParts
			if _, after := e.storedSnapshot(l); !model.EqualASRSnapshot(after, want) {
				t.Errorf("%s: snapshot %+v, want %+v", c.name, after, want)
			}
		} else {
			cur.FromMS, cur.ToMS = 0, 60000
			if got := e.resultParts(l); !slices.Equal(got, []model.ASRPart{cur}) {
				t.Errorf("%s: result parts %+v, want only %+v (transcribed from the start)", c.name, got, cur)
			}
			if after, _ := e.storedSnapshot(l); after != bound {
				t.Errorf("%s: snapshot of a run that started over changed:\n%s\n%s", c.name, bound, after)
			}
		}
		sameSentences(t, e.sentences(l.ID), e.sentences(ref.ID))
		if _, ok := e.readCheckpoint(l); ok {
			t.Errorf("%s: checkpoint left after the successful retry", c.name)
		}
	}
}

// TestASRCheckpointPowerOtherModel (power requirement 4a): after a power
// interrupt (the scheduler's finish set stats.asr_resume) the same job
// continues from the checkpoint on the battery device even with another
// model and precision (here Intel GPU large-v3 int8 → NPU large-v3-turbo
// fp16); the result's two parts, each with its own configuration, go into
// the snapshot.
func TestASRCheckpointPowerOtherModel(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASROV)
	log := e.jobLog()
	gpu := model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3", Precision: model.PrecisionINT8}
	npu := model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: model.PrecisionFP16}
	ref := e.lecture("Uninterrupted", "m.mp4")
	e.writeWAV(ref, "wav")
	e.setSnapshot(ref.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(gpu, "en", false)))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), ref.ID); r.err != nil {
		t.Fatal(r.err)
	}

	l := e.lecture("Migrate", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(gpu, "en", true)))
	if err := e.killAtFlag(l, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("migrate: %v", err)
	}
	gpuPart := model.ASRPart{FromMS: 0, ToMS: 8200, DeviceID: gpu.ID, Model: gpu.Model, Precision: gpu.Precision}
	if ck, ok := e.readCheckpoint(l); !ok || !slices.Equal(ck.Parts, []model.ASRPart{gpuPart}) {
		t.Fatalf("checkpoint after the power interrupt %+v %v", ck, ok)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.powerRequeue(j.ID, gpu.ID, sched.CodePowerSwitched, npu.ID)
	if got := e.job(model.ScopeLecture, l.ID, model.StageASR).Stats.ASRResume; got == nil || got.DeviceID != gpu.ID {
		t.Fatalf("asr_resume %+v", got)
	}
	e.must(state.BindASRDevice(e.ctx, e.st, j.ID, npu)) // the scheduler binds the battery device
	_, before := e.storedSnapshot(l)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.job.ID != j.ID {
		t.Errorf("job %s, want %s", r.job.ID, j.ID)
	}
	jobs := fakeJobs(t, log)
	if got := jobs[len(jobs)-1].Job["checkpoint"].(map[string]any); got["job_id"] != j.ID || got["device_id"] != npu.ID ||
		got["model"] != npu.Model || got["precision"] != npu.Precision {
		t.Errorf("checkpoint object %v", got)
	}
	wantParts := []model.ASRPart{gpuPart, {FromMS: 8200, ToMS: 60000, DeviceID: npu.ID, Model: npu.Model, Precision: npu.Precision}}
	if got := e.resultParts(l); !slices.Equal(got, wantParts) {
		t.Errorf("result parts %+v, want %+v", got, wantParts)
	}
	want := before
	want.Parts = wantParts
	if _, after := e.storedSnapshot(l); !model.EqualASRSnapshot(after, want) {
		t.Errorf("snapshot %+v, want %+v", after, want)
	}
	sameSentences(t, e.sentences(l.ID), e.sentences(ref.ID))
	if _, ok := e.readCheckpoint(l); ok {
		t.Error("checkpoint left after the run finished")
	}
}

// TestASRCheckpointPowerTwice: two migrations in a row (Intel GPU → NPU,
// then the NPU becomes unavailable on battery and the lecture goes back to
// the GPU). Each target device differs from the checkpoint's last part, so
// every run continues and the snapshot records the three parts.
func TestASRCheckpointPowerTwice(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	e := newEnv(t)
	e.useFakeWorker("ckpt", fakeASROV)
	gpu := model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3", Precision: model.PrecisionINT8}
	npu := model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: model.PrecisionFP16}
	ref := e.lecture("Uninterrupted", "m.mp4")
	e.writeWAV(ref, "wav")
	e.setSnapshot(ref.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(gpu, "en", false)))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), ref.ID); r.err != nil {
		t.Fatal(r.err)
	}

	l := e.lecture("Twice", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(gpu, "en", true)))
	// on the GPU, migrated to the NPU after two windows
	if err := e.killAtFlag(l, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("first migration: %v", err)
	}
	j := e.job(model.ScopeLecture, l.ID, model.StageASR)
	e.powerRequeue(j.ID, gpu.ID, sched.CodePowerSwitched, npu.ID)
	e.must(state.BindASRDevice(e.ctx, e.st, j.ID, npu))
	// continued on the NPU, migrated back to the GPU after two more windows
	if err := e.killAtFlag(l, "2", sched.ErrPowerMigrate); !sched.IsPowerInterrupt(err) {
		t.Fatalf("second migration: %v", err)
	}
	p1 := model.ASRPart{FromMS: 0, ToMS: 8200, DeviceID: gpu.ID, Model: gpu.Model, Precision: gpu.Precision}
	p2 := model.ASRPart{FromMS: 8200, ToMS: 16000, DeviceID: npu.ID, Model: npu.Model, Precision: npu.Precision}
	if ck, ok := e.readCheckpoint(l); !ok || ck.JobID != j.ID || ck.ResumeMS != 16000 || !slices.Equal(ck.Parts, []model.ASRPart{p1, p2}) {
		t.Fatalf("checkpoint after the second migration %+v %v", ck, ok)
	}
	e.powerRequeue(j.ID, npu.ID, sched.CodePowerSwitched, gpu.ID)
	if st := e.job(model.ScopeLecture, l.ID, model.StageASR).Stats; st.ASRResume == nil || st.ASRResume.DeviceID != npu.ID || st.PowerInterrupts != 2 {
		t.Fatalf("stats after the second migration %+v", st)
	}
	e.must(state.BindASRDevice(e.ctx, e.st, j.ID, gpu))
	_, before := e.storedSnapshot(l)
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.job.ID != j.ID {
		t.Errorf("job %s, want %s", r.job.ID, j.ID)
	}
	wantParts := []model.ASRPart{p1, p2, {FromMS: 16000, ToMS: 60000, DeviceID: gpu.ID, Model: gpu.Model, Precision: gpu.Precision}}
	if got := e.resultParts(l); !slices.Equal(got, wantParts) {
		t.Errorf("result parts %+v, want %+v", got, wantParts)
	}
	want := before
	want.Parts = wantParts
	if _, after := e.storedSnapshot(l); !model.EqualASRSnapshot(after, want) {
		t.Errorf("snapshot %+v, want %+v", after, want)
	}
	sameSentences(t, e.sentences(l.ID), e.sentences(ref.ID))
	if _, ok := e.readCheckpoint(l); ok {
		t.Error("checkpoint left after the run finished")
	}
}

// TestASRCheckpointPowerThenUserChange: asr_resume survives a user cancel
// and the wait for AC, so on its own it does not allow another model or
// precision. When the next run is bound to the device the checkpoint's last
// part ran on, a model the user changed in the meantime (the device row
// edited while the job was cancelled or suspended) starts over even after a
// power interrupt: one part, the snapshot as bound. The same configuration
// continues (two parts).
func TestASRCheckpointPowerThenUserChange(t *testing.T) {
	old := ovCheckpointChunkS
	ovCheckpointChunkS = 30
	t.Cleanup(func() { ovCheckpointChunkS = old })
	gpu := model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3", Precision: model.PrecisionINT8}
	cuda := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: model.PrecisionFloat16}
	withModel := func(d model.ASRDevice, m string) model.ASRDevice { d.Model = m; return d }
	for _, c := range []struct {
		name     string
		result   string // the fake worker's source
		first    model.ASRDevice
		cause    error
		code, to string // power_event
		cancel   bool   // the user cancels the requeued job and retries it
		next     model.ASRDevice
		windows  string
		resumeMS int64
		keep     bool
	}{
		{"migration interrupted, cancelled by the user, model changed, retried on the same device",
			fakeASROV, gpu, sched.ErrPowerMigrate, sched.CodePowerSwitched, model.DeviceOVNPU, true, withModel(gpu, "large-v3-turbo"), "2", 8200, false},
		{"CUDA suspended, model changed while waiting for AC",
			fakeASR, cuda, sched.ErrPowerSuspend, sched.CodePowerSuspended, "", false, withModel(cuda, "large-v3-turbo"), "3", 12600, false},
		{"CUDA suspended, row unchanged",
			fakeASR, cuda, sched.ErrPowerSuspend, sched.CodePowerSuspended, "", false, cuda, "3", 12600, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.useFakeWorker("ckpt", c.result)
			log := e.jobLog()
			ref := e.lecture("Uninterrupted", "m.mp4")
			e.writeWAV(ref, "wav")
			e.setSnapshot(ref.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(c.first, "en", false)))
			if r := e.run(e.stage(e.deps(nil), model.StageASR), ref.ID); r.err != nil {
				t.Fatal(r.err)
			}

			l := e.lecture("L", "m.mp4")
			e.writeWAV(l, "wav")
			e.setSnapshot(l.ID, model.StageASR, model.MarshalSnapshot(model.ASRDeviceSnapshot(c.first, "en", true)))
			if err := e.killAtFlag(l, c.windows, c.cause); !errors.Is(err, c.cause) {
				t.Fatalf("power interrupt: %v", err)
			}
			j := e.job(model.ScopeLecture, l.ID, model.StageASR)
			e.powerRequeue(j.ID, c.first.ID, c.code, c.to)
			old := model.ASRPart{FromMS: 0, ToMS: c.resumeMS, DeviceID: c.first.ID, Model: c.first.Model, Precision: c.first.Precision}
			if ck, ok := e.readCheckpoint(l); !ok || ck.ResumeMS != c.resumeMS || !slices.Equal(ck.Parts, []model.ASRPart{old}) {
				t.Fatalf("checkpoint after the power interrupt %+v %v", ck, ok)
			}
			if c.cancel {
				e.must(state.CancelQueued(e.ctx, e.st, j.ID))
				e.must(state.Retry(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageASR))
				// the case under test has asr_resume at the start; should a
				// user retry ever clear it, the checkpoint must still go
				if cur := e.job(model.ScopeLecture, l.ID, model.StageASR); cur.Stats.ASRResume == nil {
					st := cur.Stats
					st.ASRResume = &model.ASRResume{DeviceID: c.first.ID, Backend: c.first.Backend, At: "2026-10-24T07:00:00Z"}
					e.must(e.st.SetJobStats(e.ctx, j.ID, st))
				}
			}
			if st := e.job(model.ScopeLecture, l.ID, model.StageASR).Stats; st.ASRResume == nil || st.ASRResume.DeviceID != c.first.ID {
				t.Fatalf("asr_resume at the next start %+v", st.ASRResume)
			}
			e.must(state.BindASRDevice(e.ctx, e.st, j.ID, c.next))
			bound, boundSnap := e.storedSnapshot(l)
			r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.job.ID != j.ID {
				t.Errorf("job %s, want %s", r.job.ID, j.ID)
			}
			jobs := fakeJobs(t, log)
			if got := jobs[len(jobs)-1].Job["checkpoint"].(map[string]any); got["job_id"] != j.ID || got["device_id"] != c.next.ID ||
				got["model"] != c.next.Model || got["precision"] != c.next.Precision {
				t.Errorf("checkpoint object %v", got)
			}
			cur := model.ASRPart{DeviceID: c.next.ID, Model: c.next.Model, Precision: c.next.Precision, ToMS: 60000}
			if c.keep {
				cur.FromMS = c.resumeMS
				wantParts := []model.ASRPart{old, cur}
				if got := e.resultParts(l); !slices.Equal(got, wantParts) {
					t.Errorf("result parts %+v, want %+v", got, wantParts)
				}
				want := boundSnap
				want.Parts = wantParts
				if _, after := e.storedSnapshot(l); !model.EqualASRSnapshot(after, want) {
					t.Errorf("snapshot %+v, want %+v", after, want)
				}
			} else {
				if got := e.resultParts(l); !slices.Equal(got, []model.ASRPart{cur}) {
					t.Errorf("result parts %+v, want only %+v (transcribed from the start)", got, cur)
				}
				if after, _ := e.storedSnapshot(l); after != bound {
					t.Errorf("snapshot of a run that started over changed:\n%s\n%s", bound, after)
				}
			}
			sameSentences(t, e.sentences(l.ID), e.sentences(ref.ID))
			if _, ok := e.readCheckpoint(l); ok {
				t.Error("checkpoint left after the successful run")
			}
		})
	}
}
