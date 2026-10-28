// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"errors"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// walk moves the current job of a lecture stage through legal transitions
// (the scheduler is not started: no timing depends on the machine's load).
func (e *env) walk(lectureID, stage string, path ...string) {
	e.t.Helper()
	for _, to := range path {
		j := e.lj(lectureID, stage)
		if err := state.TransitionJob(e.ctx, e.st, j.ID, j.Status, to, db.JobFinish{}); err != nil {
			e.t.Fatalf("%s %s→%s: %v", stage, j.Status, to, err)
		}
	}
}

// done walks the stages of a lecture to done, up to and including last.
func (e *env) doneUpTo(lectureID, last string) {
	e.t.Helper()
	for _, st := range model.LectureStages {
		e.walk(lectureID, st, model.StatusQueued, model.StatusRunning, model.StatusDone)
		if st == last {
			return
		}
	}
}

// Batch rerun (course-ux): lectures that cannot rerun the stage are skipped
// with their reason; the model question is asked once for the others and
// changes nothing; with a choice each lecture is rerun exactly as Rerun
// does it. The scheduler loop is not started: the states are set by legal
// transitions, so the test does not depend on how fast jobs run.
func TestRerunLecturesAsksOnceAndSkips(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l1, l2, busy, wait := e.lecture(), e.lecture(), e.lecture(), e.lecture()
	d := e.draft()
	e.doneUpTo(l1.ID, model.StageExport)
	e.doneUpTo(l2.ID, model.StageExport)
	e.doneUpTo(busy.ID, model.StageRepair)
	e.walk(busy.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning) // summary done, translate running
	e.doneUpTo(wait.ID, model.StagePrep)
	e.walk(wait.ID, model.StageASR, model.StatusQueued, model.StatusRunning) // summary still pending
	e.assign(p2.ID, "model-b", model.StageTranslate)

	ids := []string{l1.ID, busy.ID, l2.ID, wait.ID, d.ID, "missing", l1.ID}
	before1, before2 := e.jobIDs(l1.ID), e.jobIDs(l2.ID)

	// the plan: blocks per lecture, the diffs of the two that would rerun
	plan, diffs, err := e.s.PlanRerun(e.ctx, ids, model.StageSummary)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 6 {
		t.Fatalf("plan has %d entries (repeated ids once): %+v", len(plan), plan)
	}
	checkBlocks := func(rs []RerunResult) {
		t.Helper()
		if rs[0].LectureID != l1.ID || rs[0].Block != nil || rs[0].Err != nil ||
			rs[2].LectureID != l2.ID || rs[2].Block != nil || rs[2].Err != nil {
			t.Fatalf("l1, l2 are rerun: %+v", rs)
		}
		if b := rs[1].Block; rs[1].LectureID != busy.ID || b == nil || b.Reason != RerunBlockBusy || b.Stage != model.StageTranslate {
			t.Fatalf("busy: %+v", rs[1])
		}
		if b := rs[3].Block; rs[3].LectureID != wait.ID || b == nil || b.Reason != RerunBlockStatus || b.Stage != model.StageSummary || b.Status != model.StatusPending {
			t.Fatalf("summary pending: %+v", rs[3])
		}
		if b := rs[4].Block; rs[4].LectureID != d.ID || b == nil || b.Reason != RerunBlockDraft {
			t.Fatalf("draft: %+v", rs[4])
		}
		if rs[5].LectureID != "missing" || !errors.Is(rs[5].Err, db.ErrNotFound) {
			t.Fatalf("missing: %+v", rs[5])
		}
	}
	checkBlocks(plan)
	byLecture := map[string]bool{}
	for _, x := range diffs {
		if x.Stage != model.StageTranslate {
			t.Fatalf("diff %+v", x)
		}
		byLecture[x.LectureID] = true
	}
	if len(byLecture) != 2 || !byLecture[l1.ID] || !byLecture[l2.ID] {
		t.Fatalf("diffs %+v", diffs)
	}

	// choice "": one question for all, nothing changes
	res, err := e.s.RerunLectures(e.ctx, ids, model.StageSummary, "")
	if got := needsChoice(t, err); res != nil || len(got) != 2 {
		t.Fatalf("one aggregated question: res=%v diffs=%+v", res, got)
	}
	for s, id := range e.jobIDs(l1.ID) {
		if id != before1[s] || e.jobIDs(l2.ID)[s] != before2[s] {
			t.Fatal("the question must not change anything")
		}
	}

	// with a choice: each lecture through Rerun
	res, err = e.s.RerunLectures(e.ctx, ids, model.StageSummary, model.ModelChoiceCurrent)
	if err != nil || len(res) != 6 {
		t.Fatalf("rerun with a choice: %v %+v", err, res)
	}
	checkBlocks(res)
	for _, l := range []model.Lecture{l1, l2} {
		before := map[string]map[string]string{l1.ID: before1, l2.ID: before2}[l.ID]
		for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
			j := e.lj(l.ID, st)
			if j.ID == before[st] || j.Status != model.StatusPending {
				t.Fatalf("%s: %s must be a new pending job: %s %s", l.ID, st, j.ID, j.Status)
			}
		}
		for _, st := range []string{model.StagePrep, model.StageASR} {
			if j := e.lj(l.ID, st); j.ID != before[st] || j.Status != model.StatusDone {
				t.Fatalf("%s: %s (upstream) must keep its job", l.ID, st)
			}
		}
		if providerOfSnap(e.lj(l.ID, model.StageTranslate).ConfigSnapshot) != p2.ID {
			t.Fatalf("%s translate not switched to p2", l.ID)
		}
		if !e.hasChanged("lecture", l.ID) {
			t.Fatalf("no SSE event for %s", l.ID)
		}
	}
	// the skipped ones are untouched
	if j := e.lj(busy.ID, model.StageTranslate); j.Status != model.StatusRunning || providerOfSnap(j.ConfigSnapshot) != p1.ID {
		t.Fatalf("busy translate %s %s", j.Status, j.ConfigSnapshot)
	}
	if j := e.lj(wait.ID, model.StageSummary); j.Status != model.StatusPending {
		t.Fatalf("waiting summary %s", j.Status)
	}
}

// The batch takes the stages of the lecture pipeline only, and the A4.3
// choices.
func TestRerunLecturesInput(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	l := e.lecture()
	for _, st := range []string{"", model.StageTitle, "glossary_seed", "nope"} {
		if _, err := e.s.RerunLectures(e.ctx, []string{l.ID}, st, ""); !errors.Is(err, state.ErrBadInput) {
			t.Errorf("stage %q: %v", st, err)
		}
	}
	if _, err := e.s.RerunLectures(e.ctx, []string{l.ID}, model.StageExport, "both"); !errors.Is(err, state.ErrBadInput) {
		t.Errorf("bad choice: %v", err)
	}
}
