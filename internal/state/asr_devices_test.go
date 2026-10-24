// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"errors"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// SPEC-v2 I8, lead ruling 1: BindASRDevice records the device an auto-mode
// asr job runs on.

func autoSnap(dev model.ASRDevice, language string) string {
	return model.MarshalSnapshot(model.ASRDeviceSnapshot(dev, language, true))
}

var (
	bindCUDA = model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: "RTX", Enabled: true,
		Model: "large-v3", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 300}
	bindNPU = model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "NPU", Enabled: true,
		Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 100}
)

// launchedWithASR launches a lecture whose asr job carries asrRaw.
func (f *fixture) launchedWithASR(asrRaw string) model.Lecture {
	f.t.Helper()
	snaps := launchSnaps()
	snaps[model.StageASR] = asrRaw
	return f.launched(snaps)
}

func TestBindASRDeviceRewritesAutoSnapshot(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindCUDA, "de"))
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	f.setHash(model.ScopeLecture, l.ID, model.StageASR, "h-before")
	before := f.lj(l.ID, model.StageASR)

	if err := BindASRDevice(f.ctx, f.st, before.ID, bindNPU); err != nil {
		t.Fatal(err)
	}
	after := f.lj(l.ID, model.StageASR)
	want := model.ASRSnapshot{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo",
		Precision: model.PrecisionFP16, Language: "de", Auto: true}
	if got, ok := model.ParseASRSnapshot(after.ConfigSnapshot); !ok || !model.EqualASRSnapshot(got, want) {
		t.Fatalf("snapshot = %s (%+v), want %+v", after.ConfigSnapshot, got, want)
	}
	if after.Status != model.StatusQueued || after.InputHash == nil || *after.InputHash != "h-before" ||
		after.Attempt != before.Attempt || after.ID != before.ID {
		t.Fatalf("bind changed more than the snapshot: before %+v after %+v", before, after)
	}

	// A running job is rebound too (the scheduler binds after queued → running).
	if err := TransitionJob(f.ctx, f.st, after.ID, model.StatusQueued, model.StatusRunning, db.JobFinish{}); err != nil {
		t.Fatal(err)
	}
	if err := BindASRDevice(f.ctx, f.st, after.ID, bindCUDA); err != nil {
		t.Fatal(err)
	}
	got, _ := model.ParseASRSnapshot(f.lj(l.ID, model.StageASR).ConfigSnapshot)
	if got.DeviceID != model.DeviceCUDA0 || got.Backend != model.BackendFasterWhisper || got.Model != "large-v3" ||
		got.Precision != model.PrecisionAuto || got.Language != "de" || !got.Auto {
		t.Fatalf("rebound snapshot = %+v", got)
	}
	if j := f.lj(l.ID, model.StageASR); j.Status != model.StatusRunning {
		t.Fatalf("status = %s", j.Status)
	}
}

func TestBindASRDeviceFillsBackendFromID(t *testing.T) {
	f := newFixture(t)
	l := f.launchedWithASR(autoSnap(bindCUDA, "en"))
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	dev := bindNPU
	dev.Backend = ""
	if err := BindASRDevice(f.ctx, f.st, f.lj(l.ID, model.StageASR).ID, dev); err != nil {
		t.Fatal(err)
	}
	got, _ := model.ParseASRSnapshot(f.lj(l.ID, model.StageASR).ConfigSnapshot)
	if got.Backend != model.BackendOpenVINO {
		t.Fatalf("backend = %q", got.Backend)
	}
	if err := BindASRDevice(f.ctx, f.st, f.lj(l.ID, model.StageASR).ID, model.ASRDevice{}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("device without id: %v", err)
	}
}

func TestBindASRDeviceRefusals(t *testing.T) {
	f := newFixture(t)
	fixed := model.MarshalSnapshot(model.ASRDeviceSnapshot(bindCUDA, "en", false))
	cases := []struct {
		name  string
		asr   string
		stage string // job to bind (default asr)
		path  []string
	}{
		{"fixed snapshot", fixed, "", []string{model.StatusQueued}},
		{"v1.1 snapshot", model.MarshalSnapshot(model.ASRSnapshot{Model: "large-v3", ComputeType: "auto", Device: "cuda", Language: "en"}), "", []string{model.StatusQueued}},
		{"pending", autoSnap(bindCUDA, "en"), "", nil},
		{"done", autoSnap(bindCUDA, "en"), "", []string{model.StatusQueued, model.StatusRunning, model.StatusDone}},
		{"failed", autoSnap(bindCUDA, "en"), "", []string{model.StatusQueued, model.StatusRunning, model.StatusFailed}},
		{"not asr", autoSnap(bindCUDA, "en"), model.StageSummary, []string{model.StatusQueued}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := f.launchedWithASR(c.asr)
			stage := c.stage
			if stage == "" {
				stage = model.StageASR
			}
			f.drive(model.ScopeLecture, l.ID, stage, c.path...)
			j := f.lj(l.ID, stage)
			err := BindASRDevice(f.ctx, f.st, j.ID, bindNPU)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want ErrConflict", err)
			}
			if got := f.lj(l.ID, stage); got.ConfigSnapshot != j.ConfigSnapshot || got.Status != j.Status {
				t.Fatalf("refused bind changed the job: %s → %s", j.ConfigSnapshot, got.ConfigSnapshot)
			}
		})
	}

	// A retired (not current) job is never rebound.
	l := f.launchedWithASR(autoSnap(bindCUDA, "en"))
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	old := f.lj(l.ID, model.StageASR)
	if err := f.st.RetireJob(f.ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := BindASRDevice(f.ctx, f.st, old.ID, bindNPU); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired job: %v", err)
	}
	if err := BindASRDevice(f.ctx, f.st, "no-such-job", bindNPU); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

// The bind is part of the caller's transaction: a rollback leaves the
// snapshot as it was.
func TestBindASRDeviceInRolledBackTransaction(t *testing.T) {
	f := newFixture(t)
	raw := autoSnap(bindCUDA, "en")
	l := f.launchedWithASR(raw)
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	id := f.lj(l.ID, model.StageASR).ID
	boom := errors.New("boom")
	err := f.st.Tx(f.ctx, func(tx db.Store) error {
		if err := TransitionJob(f.ctx, tx, id, model.StatusQueued, model.StatusRunning, db.JobFinish{}); err != nil {
			return err
		}
		if err := RecordPromptVersion(f.ctx, tx, id); err != nil {
			return err
		}
		if err := BindASRDevice(f.ctx, tx, id, bindNPU); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("tx err = %v", err)
	}
	if j := f.lj(l.ID, model.StageASR); j.Status != model.StatusQueued || j.ConfigSnapshot != raw {
		t.Fatalf("after rollback: %s %s", j.Status, j.ConfigSnapshot)
	}
}
