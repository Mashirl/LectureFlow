// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Lecture kinds through the scheduler (lead freeze .claude/kinds-brief.md
// 1.5, K1): asr waits for the slides bound to its teaching week, and a
// changed material set makes summary stale with materials_changed, with a
// job event.
func TestWeekMaterialsThroughScheduler(t *testing.T) {
	terms := newGate()
	slidesHash := func(ctx context.Context, st db.Store, id string) (string, error) {
		snap, err := state.LoadSnapshot(ctx, st, id)
		if err != nil {
			return "", err
		}
		h := sha256.Sum256([]byte(ctxbuild.LectureSlides(snap)))
		return hex.EncodeToString(h[:]), nil
	}
	var e *env
	summary := &fakeStage{name: model.StageSummary, res: ResourceAPI,
		hash: func(ctx context.Context, id string) (string, error) { return slidesHash(ctx, e.st, id) }}
	stages := append(allInstant(summary), instant(model.StageMaterialExtract, ResourceCPU),
		&fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: terms.run})
	e = newEnv(t, nil, stages...)
	term := "2026-09-28"
	if err := e.st.SetCourseCalendar(e.ctx, e.course.ID, &term, nil); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	rec := "2026-10-13T09:35" // week 3
	if err := e.st.SetLectureRecordedAt(e.ctx, l.ID, &rec, model.RecordedAtFilename); err != nil {
		t.Fatal(err)
	}
	w := 3
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "Week3.pdf", Path: "p", PagesMD: []string{"LFSR"}, BindWeek: &w}
	if err := e.st.CreateMaterial(e.ctx, m); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitFor("asr waits for the week's slides", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitMaterialTerms("Week3.pdf")
	})
	close(terms.release)
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	sum := e.lj(l.ID, model.StageSummary)
	if sum.InputHash == nil || *sum.InputHash == "" {
		t.Fatal("summary recorded no hash")
	}

	// The baseline: nothing changed, nothing stale.
	if err := e.s.InvalidateCourseWith(e.ctx, e.course.ID, state.StaleCause{Kind: state.StaleMaterials}); err != nil {
		t.Fatal(err)
	}
	if s := e.lj(l.ID, model.StageSummary).Status; s != model.StatusDone {
		t.Fatalf("summary = %s", s)
	}
	// Rebinding the slides to week 4 removes them from the lecture's set.
	w4 := 4
	if err := e.st.SetMaterialBinding(e.ctx, m.ID, nil, &w4); err != nil {
		t.Fatal(err)
	}
	e.ev.mu.Lock()
	n := len(e.ev.jobs)
	e.ev.mu.Unlock()
	if err := e.s.InvalidateCourseWith(e.ctx, e.course.ID, state.StaleCause{Kind: state.StaleMaterials}); err != nil {
		t.Fatal(err)
	}
	j := e.lj(l.ID, model.StageSummary)
	want := model.StaleReasonJSON(model.StaleCodeMaterials, map[string]any{"removed": []string{"Week3.pdf"}})
	if j.Status != model.StatusStale || j.StaleReason == nil || *j.StaleReason != want {
		t.Fatalf("summary = %s %v, want stale %s", j.Status, j.StaleReason, want)
	}
	e.ev.mu.Lock()
	emitted := false
	for _, x := range e.ev.jobs[n:] {
		if x.ID == j.ID && x.Status == model.StatusStale {
			emitted = true
		}
	}
	e.ev.mu.Unlock()
	if !emitted {
		t.Fatal("no job event for the stale summary")
	}
	time.Sleep(40 * time.Millisecond)
	if s := e.lj(l.ID, model.StageSummary).Status; s != model.StatusStale {
		t.Fatalf("stale must not rerun automatically: %s", s)
	}
}
