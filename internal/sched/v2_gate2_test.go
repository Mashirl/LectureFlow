// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Lead ruling, Gate 2 (core-builder): an LLM lecture job records the prompt
// version it actually runs with. When the scheduler moves it to running,
// prompt_version in its config_snapshot becomes the current one (only that
// field; empty or invalid snapshots are left alone). Jobs that have not run
// keep their old version, so a rerun still shows "提示词 v1 → v2" for them.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

func (e *env) promptVersion(lectureID, stage string) int {
	e.t.Helper()
	snap, ok := model.ParseLLMSnapshot(e.lj(lectureID, stage).ConfigSnapshot)
	if !ok {
		e.t.Fatalf("%s: no LLM snapshot", stage)
	}
	return model.SnapshotPromptVersion(snap)
}

// sameModelAs: provider, model and params of two LLM snapshots are equal.
func sameModelAs(t *testing.T, a, b string) bool {
	t.Helper()
	sa, okA := model.ParseLLMSnapshot(a)
	sb, okB := model.ParseLLMSnapshot(b)
	sa.PromptVersion, sb.PromptVersion = 0, 0
	return okA && okB && model.MarshalSnapshot(sa) == model.MarshalSnapshot(sb)
}

// The downstream jobs left pending by a retry run with the current prompt
// and say so once they run; a job that has not run keeps v1.
func TestRunningRecordsPromptVersionAfterRetry(t *testing.T) {
	e := newEnv(t, nil,
		instant(model.StageSummary, ResourceAPI), instant(model.StageRepair, ResourceAPI)) // translate never runs
	l := e.v1Lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR)
	e.drive(e.lj(l.ID, model.StageSummary).ID, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	before := e.snaps(l.ID)
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatal(err)
	}
	if v := e.promptVersion(l.ID, model.StageRepair); v != 1 {
		t.Fatalf("repair before it runs: v%d", v)
	}
	e.start()
	e.waitStatus(l.ID, model.StageRepair, model.StatusDone)
	e.waitFor("translate queued", 5*time.Second, func() bool { return e.lj(l.ID, model.StageTranslate).Status == model.StatusQueued })
	for _, s := range []string{model.StageSummary, model.StageRepair} {
		if v := e.promptVersion(l.ID, s); v != model.PromptVersionOf(s) {
			t.Errorf("%s after running: v%d", s, v)
		}
		if !sameModelAs(t, e.lj(l.ID, s).ConfigSnapshot, before[s]) {
			t.Errorf("%s: more than the prompt version changed: %s → %s", s, before[s], e.lj(l.ID, s).ConfigSnapshot)
		}
	}
	if got := e.lj(l.ID, model.StageTranslate).ConfigSnapshot; got != before[model.StageTranslate] {
		t.Errorf("translate has not run, its snapshot must stay: %s", got)
	}
	if asr := e.lj(l.ID, model.StageASR).ConfigSnapshot; asr != before[model.StageASR] {
		t.Errorf("asr snapshot changed: %s", asr)
	}
}

// The incremental translate copies the last translate snapshot (A4.2) and
// records the current prompt version once it runs.
func TestRunningRecordsPromptVersionIncremental(t *testing.T) {
	var seen atomic.Value
	tr := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		if j, ok := JobFrom(ctx); ok {
			seen.Store(j.ConfigSnapshot)
		}
		return nil
	}}
	e := newEnv(t, nil, tr, instant(model.StageExport, ResourceCPU))
	l := e.v1Lecture()
	e.stagesDone(l.ID, model.LectureStages...)
	before := e.lj(l.ID, model.StageTranslate)
	if ok, err := state.StartIncrementalTranslate(e.ctx, e.st, l.ID); err != nil || !ok {
		t.Fatalf("incremental: %v %v", ok, err)
	}
	if v := e.promptVersion(l.ID, model.StageTranslate); v != 1 {
		t.Fatalf("new incremental job before it runs: v%d", v)
	}
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	j := e.lj(l.ID, model.StageTranslate)
	if j.ID == before.ID || !state.IsIncremental(j.Stats.Note) {
		t.Fatalf("not the incremental job: %+v", j)
	}
	if v := e.promptVersion(l.ID, model.StageTranslate); v != model.PromptVersionTranslate {
		t.Fatalf("incremental translate after running: v%d", v)
	}
	if !sameModelAs(t, j.ConfigSnapshot, before.ConfigSnapshot) {
		t.Fatalf("more than the prompt version changed: %s → %s", before.ConfigSnapshot, j.ConfigSnapshot)
	}
	if got, _ := seen.Load().(string); got != j.ConfigSnapshot {
		t.Fatalf("the stage ran with %s, the job records %s", got, j.ConfigSnapshot)
	}
}

// Empty or invalid snapshots and non-LLM stages are left alone.
func TestRunningLeavesOtherSnapshotsAlone(t *testing.T) {
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), instant(model.StageSummary, ResourceAPI))
	l := e.v1Lecture()
	if err := e.st.SetJobSnapshot(e.ctx, e.lj(l.ID, model.StageSummary).ID, model.EmptySnapshot); err != nil {
		t.Fatal(err)
	}
	e.stagesDone(l.ID, model.StageASR) // asr done without running (prep is upstream only)
	prep := e.lj(l.ID, model.StagePrep).ConfigSnapshot
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusDone)
	if got := e.lj(l.ID, model.StageSummary).ConfigSnapshot; got != model.EmptySnapshot {
		t.Fatalf("empty summary snapshot became %s", got)
	}
	if got := e.lj(l.ID, model.StagePrep).ConfigSnapshot; got != prep {
		t.Fatalf("prep snapshot became %s", got)
	}
}
