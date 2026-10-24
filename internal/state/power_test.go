// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// Power management: a power interrupt requeues the job like §4.7 (one
// transaction, never a committed "failed"), without a retry limit and
// without an error text.
func TestPowerRequeue(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindCUDA, "en"))
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning)
	j := f.lj(l.ID, model.StageASR)
	// More interrupts than MaxAutoRetries: still requeued every time.
	for i := 0; i <= model.MaxAutoRetries+1; i++ {
		if err := PowerRequeue(f.ctx, f.st, j.ID); err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
		got := f.lj(l.ID, model.StageASR)
		if got.ID != j.ID || got.Status != model.StatusQueued || got.Attempt != j.Attempt+i+1 || got.Progress != 0 ||
			(got.Error != nil && *got.Error != "") || got.StartedAt != nil && *got.StartedAt != "" {
			t.Fatalf("after requeue %d: %+v", i, got)
		}
		f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	}
	// Not running: conflict, nothing written.
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusDone)
	before := f.lj(l.ID, model.StageASR)
	if err := PowerRequeue(f.ctx, f.st, j.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("done job: %v", err)
	}
	if after := f.lj(l.ID, model.StageASR); after.Status != model.StatusDone || after.Attempt != before.Attempt {
		t.Fatalf("done job changed: %+v", after)
	}
}

func TestRecordASRParts(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindNPU, "en"))
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	j := f.lj(l.ID, model.StageASR)
	parts := []model.ASRPart{
		{FromMS: 0, ToMS: 90000, DeviceID: model.DeviceOVGPU, Model: "large-v3-turbo", Precision: model.PrecisionFP16},
		{FromMS: 90000, ToMS: 600000, DeviceID: model.DeviceOVNPU, Model: "large-v3-turbo", Precision: model.PrecisionFP16},
	}
	// Only while running.
	if err := RecordASRParts(f.ctx, f.st, j.ID, parts); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued job: %v", err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	raw := f.lj(l.ID, model.StageASR).ConfigSnapshot
	// One part: the snapshot keeps its bytes.
	if err := RecordASRParts(f.ctx, f.st, j.ID, parts[:1]); err != nil {
		t.Fatal(err)
	}
	if got := f.lj(l.ID, model.StageASR).ConfigSnapshot; got != raw {
		t.Fatalf("one part changed the snapshot: %s", got)
	}
	if err := RecordASRParts(f.ctx, f.st, j.ID, parts); err != nil {
		t.Fatal(err)
	}
	got, ok := model.ParseASRSnapshot(f.lj(l.ID, model.StageASR).ConfigSnapshot)
	want, _ := model.ParseASRSnapshot(raw)
	want.Parts = parts
	if !ok || !model.EqualASRSnapshot(got, want) {
		t.Fatalf("snapshot %+v, want %+v", got, want)
	}
}

// The retry limit counts model.CountedAttempts (attempt minus
// stats.power_interrupts): after several power requeues a real failure
// still gets its two automatic retries, in FailJob, AutoRetry and §4.7
// recovery.
func TestRetryLimitIgnoresPowerRequeues(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindCUDA, "en"))
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning)
	j := f.lj(l.ID, model.StageASR)
	interrupt := func() {
		t.Helper()
		st := f.lj(l.ID, model.StageASR).Stats
		st.PowerInterrupts++
		if err := f.st.SetJobStats(f.ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
		if err := PowerRequeue(f.ctx, f.st, j.ID); err != nil {
			t.Fatal(err)
		}
		f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	}
	for i := 0; i < 3; i++ {
		interrupt()
	}
	// attempt 3, power_interrupts 3: a real failure is retried twice.
	for i := 0; i < model.MaxAutoRetries; i++ {
		requeued, err := FailJob(f.ctx, f.st, j.ID, "boom", true)
		if err != nil || !requeued {
			t.Fatalf("failure %d: requeued %v err %v", i, requeued, err)
		}
		f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	}
	if requeued, err := FailJob(f.ctx, f.st, j.ID, "boom", true); err != nil || requeued {
		t.Fatalf("third failure: requeued %v err %v", requeued, err)
	}
	got := f.lj(l.ID, model.StageASR)
	if got.Status != model.StatusFailed || got.Attempt != 3+model.MaxAutoRetries || model.CountedAttempts(got) != model.MaxAutoRetries {
		t.Fatalf("after the retries: %s attempt %d counted %d", got.Status, got.Attempt, model.CountedAttempts(got))
	}
	if err := AutoRetry(f.ctx, f.st, got); !errors.Is(err, ErrConflict) {
		t.Fatalf("AutoRetry past the limit: %v", err)
	}
	// One more power requeue counted (stats) makes one retry available again.
	st := got.Stats
	st.PowerInterrupts++
	if err := f.st.SetJobStats(f.ctx, j.ID, st); err != nil {
		t.Fatal(err)
	}
	if err := AutoRetry(f.ctx, f.st, f.lj(l.ID, model.StageASR)); err != nil {
		t.Fatalf("AutoRetry with a power requeue not counted: %v", err)
	}

	// §4.7: a restart while it runs requeues by the counted attempts too
	// (attempt 6, power_interrupts 4 → counted 2: stays failed; with 5 → 1:
	// requeued).
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if got := f.lj(l.ID, model.StageASR); got.Status != model.StatusFailed {
		t.Fatalf("recovery past the limit: %s (attempt %d, interrupts %d)", got.Status, got.Attempt, got.Stats.PowerInterrupts)
	}
	f2 := newFixture(t)
	l2 := f2.launchedWithASR(autoSnap(bindCUDA, "en"))
	f2.done(model.ScopeLecture, l2.ID, model.StagePrep)
	f2.drive(model.ScopeLecture, l2.ID, model.StageASR, model.StatusQueued, model.StatusRunning)
	j2 := f2.lj(l2.ID, model.StageASR)
	for i := 0; i < 2; i++ {
		st := f2.lj(l2.ID, model.StageASR).Stats
		st.PowerInterrupts++
		f2.st.SetJobStats(f2.ctx, j2.ID, st)
		if err := PowerRequeue(f2.ctx, f2.st, j2.ID); err != nil {
			t.Fatal(err)
		}
		f2.drive(model.ScopeLecture, l2.ID, model.StageASR, model.StatusRunning)
	}
	// attempt 2 (≥ MaxAutoRetries by the raw count), counted 0: requeued.
	if err := Recover(f2.ctx, f2.st, f2.dataDir); err != nil {
		t.Fatal(err)
	}
	if got := f2.lj(l2.ID, model.StageASR); got.Status != model.StatusQueued || got.Attempt != 3 {
		t.Fatalf("recovery after power requeues: %s attempt %d", got.Status, got.Attempt)
	}
}

// Backend Gate round 1, fix 4: when the user changes the asr model on a
// retry ("使用当前设置" with another configuration), the run starts over:
// RetryWith clears stats.asr_resume and stats.power_event and removes
// asr.ckpt.json and its .tmp. Without a new snapshot, or with one of the
// same configuration, the checkpoint stays (a cancelled run continues).
func TestRetryWithNewASRSnapshotDropsCheckpoint(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindNPU, "en"))
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusCancelled)
	j := f.lj(l.ID, model.StageASR)
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	ckpt := filepath.Join(dir, model.ASRCheckpointFile)
	files := []string{ckpt, ckpt + ".tmp"}
	interrupted := func() {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range files {
			if err := os.WriteFile(p, []byte(`{"version":1}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		st := f.lj(l.ID, model.StageASR).Stats
		st.PowerInterrupts = 1
		st.ASRResume = &model.ASRResume{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, At: "2026-10-24T08:00:00Z"}
		st.PowerEvent = &model.PowerEvent{Code: "power.suspended", At: "2026-10-24T08:00:00Z", FromDevice: model.DeviceOVGPU}
		if err := f.st.SetJobStats(f.ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(what string) {
		t.Helper()
		for _, p := range files {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s: %s removed (%v)", what, filepath.Base(p), err)
			}
		}
		if st := f.lj(l.ID, model.StageASR).Stats; st.ASRResume == nil || st.PowerEvent == nil || st.PowerInterrupts != 1 {
			t.Fatalf("%s: stats %+v", what, st)
		}
	}
	interrupted()

	// No new snapshot: the retry continues from the checkpoint.
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, nil); err != nil {
		t.Fatal(err)
	}
	kept("RetryWith(nil)")
	// A new snapshot of the same configuration ("沿用原模型" pins it).
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning, model.StatusCancelled)
	same := model.PinASRSnapshot(f.lj(l.ID, model.StageASR).ConfigSnapshot)
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, map[string]string{model.StageASR: same}); err != nil {
		t.Fatal(err)
	}
	kept("same configuration")
	if got := f.lj(l.ID, model.StageASR).ConfigSnapshot; got != same {
		t.Fatalf("snapshot %s, want %s", got, same)
	}

	// Another model: the checkpoint and the power state are gone.
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning, model.StatusFailed)
	small := bindNPU
	small.Model, small.Precision = "small", model.PrecisionINT8
	other := autoSnap(small, "en")
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, map[string]string{model.StageASR: other}); err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after a model change (%v)", filepath.Base(p), err)
		}
	}
	got := f.lj(l.ID, model.StageASR)
	if got.Status != model.StatusQueued || got.ConfigSnapshot != other || got.Stats.ASRResume != nil || got.Stats.PowerEvent != nil ||
		got.Stats.PowerInterrupts != 1 {
		t.Fatalf("after a model change: %s %s %+v", got.Status, got.ConfigSnapshot, got.Stats)
	}
	// Nothing to remove: fine.
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning, model.StatusFailed)
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, map[string]string{model.StageASR: autoSnap(bindNPU, "en")}); err != nil {
		t.Fatal(err)
	}

	// A refused retry (the job is queued) removes nothing.
	interrupted()
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, map[string]string{model.StageASR: other}); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a queued job: %v", err)
	}
	kept("refused retry")

	// A pending asr job below a retried prep gets the new model too: same rule.
	f2 := newFixture(t)
	l2 := f2.launchedWithASR(autoSnap(bindNPU, "en"))
	f2.drive(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	a2 := f2.lj(l2.ID, model.StageASR)
	st := a2.Stats
	st.ASRResume = &model.ASRResume{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, At: "2026-10-24T08:00:00Z"}
	if err := f2.st.SetJobStats(f2.ctx, a2.ID, st); err != nil {
		t.Fatal(err)
	}
	dir2 := model.LectureDir(f2.dataDir, l2.CourseID, l2.ID)
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, model.ASRCheckpointFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RetryWith(f2.ctx, f2.st, f2.dataDir, model.ScopeLecture, l2.ID, model.StagePrep, map[string]string{model.StageASR: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir2, model.ASRCheckpointFile)); !os.IsNotExist(err) {
		t.Fatalf("downstream checkpoint kept (%v)", err)
	}
	if got := f2.lj(l2.ID, model.StageASR); got.ConfigSnapshot != other || got.Stats.ASRResume != nil {
		t.Fatalf("downstream asr: %s %+v", got.ConfigSnapshot, got.Stats)
	}
}
