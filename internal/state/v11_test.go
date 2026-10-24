// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// Tests of the v1.1 state operations (SPEC-v1.1 A1–A4; B.1/B.2, state layer).
// Written before the implementation (user guardrail: test first).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---------- snapshot helpers ----------

func llmSnap(provider, mdl string, temp float64, window ...int) string {
	s := model.LLMSnapshot{ProviderID: provider, ProviderName: strings.ToUpper(provider), Model: mdl,
		Params: model.RoleParams{Temperature: &temp}}
	if len(window) > 0 {
		w := window[0]
		s.Params.Window = &w
	}
	return model.MarshalSnapshot(s)
}

func asrSnap(mdl string) string {
	return model.MarshalSnapshot(model.ASRSnapshot{Model: mdl, ComputeType: "auto", Device: "cuda", Language: "en"})
}

// launchSnaps is a full set of launch snapshots ("current settings").
func launchSnaps() map[string]string {
	return map[string]string{
		model.StageASR:       asrSnap("large-v2"),
		model.StageSummary:   llmSnap("deepseek", "deepseek-chat", 0.2),
		model.StageRepair:    llmSnap("deepseek", "deepseek-chat", 0.1),
		model.StageTranslate: llmSnap("deepseek", "deepseek-chat", 0.3, 3),
	}
}

func (f *fixture) snapshots(lectureID string) map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, s := range model.LectureStages {
		out[s] = f.lj(lectureID, s).ConfigSnapshot
	}
	return out
}

func (f *fixture) launched(snaps map[string]string) model.Lecture {
	f.t.Helper()
	l := f.draft()
	if err := Launch(f.ctx, f.st, l.ID, snaps); err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fixture) allDone(lectureID string) {
	f.t.Helper()
	for _, s := range model.LectureStages {
		f.done(model.ScopeLecture, lectureID, s)
	}
}

func jobsOf(statuses ...string) []model.Job {
	var out []model.Job
	for i, s := range statuses {
		out = append(out, model.Job{Scope: model.ScopeLecture, Stage: model.LectureStages[i], Status: s})
	}
	return out
}

// ---------- A3 launch ----------

func TestLaunchWritesSnapshotsAndLaunchedAt(t *testing.T) {
	f := newFixture(t)
	l := f.draft()
	got, err := f.st.GetLecture(f.ctx, l.ID)
	if err != nil || !IsDraft(got) {
		t.Fatalf("new lecture must be a draft: %+v %v", got, err)
	}
	for s, snap := range f.snapshots(l.ID) {
		if snap != model.EmptySnapshot {
			t.Fatalf("draft %s snapshot = %q", s, snap)
		}
	}
	snaps := launchSnaps()
	if err := Launch(f.ctx, f.st, l.ID, snaps); err != nil {
		t.Fatal(err)
	}
	got, _ = f.st.GetLecture(f.ctx, l.ID)
	if IsDraft(got) || got.LaunchedAt == nil || *got.LaunchedAt == "" {
		t.Fatalf("launched_at not written: %+v", got)
	}
	want := map[string]string{
		model.StagePrep:   model.EmptySnapshot,
		model.StageExport: model.EmptySnapshot,
	}
	for k, v := range snaps {
		want[k] = v
	}
	if have := f.snapshots(l.ID); !reflect.DeepEqual(have, want) {
		t.Fatalf("snapshots after launch:\n have %v\n want %v", have, want)
	}
	// All six jobs are still pending: launching is not a status change.
	for _, s := range model.LectureStages {
		if st := f.lj(l.ID, s).Status; st != model.StatusPending {
			t.Fatalf("%s = %s after launch", s, st)
		}
	}

	// Launching again: 409, nothing overwritten.
	other := map[string]string{model.StageTranslate: llmSnap("anthropic", "claude", 0.3, 5)}
	err = Launch(f.ctx, f.st, l.ID, other)
	if !errors.Is(err, ErrAlreadyLaunched) || !errors.Is(err, ErrConflict) {
		t.Fatalf("second launch: %v", err)
	}
	if have := f.snapshots(l.ID); !reflect.DeepEqual(have, want) {
		t.Fatalf("second launch changed snapshots: %v", have)
	}
	again, _ := f.st.GetLecture(f.ctx, l.ID)
	if *again.LaunchedAt != *got.LaunchedAt {
		t.Fatalf("launched_at overwritten: %q → %q", *got.LaunchedAt, *again.LaunchedAt)
	}

	if err := Launch(f.ctx, f.st, "nope", snaps); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown lecture: %v", err)
	}
}

func TestDraftJobsAreNotStarted(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	// A syllabus without a finished glossary_seed would block asr: the draft
	// check must come first.
	f.st.UpsertSyllabus(ctx, model.Syllabus{CourseID: f.course.ID, Filename: "s.md", Path: "p"})
	if err := ResetGlossarySeed(ctx, f.st, f.course.ID); err != nil {
		t.Fatal(err)
	}
	l := f.draft()
	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, LectureID: &l.ID, Filename: "w1.pdf", Path: "p"}
	if err := f.st.CreateMaterial(ctx, m); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.LectureStages {
		ok, w, err := Deps(ctx, f.st, f.lj(l.ID, s))
		if err != nil || ok || w != model.WaitNotStarted {
			t.Fatalf("draft %s: ok=%v waiting_for=%q err=%v", s, ok, w, err)
		}
	}
	// Course and material jobs are not affected by drafts.
	if ok, _, err := Deps(ctx, f.st, f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)); !ok || err != nil {
		t.Fatalf("glossary_seed must not wait for the draft: %v %v", ok, err)
	}
	if ok, _, err := Deps(ctx, f.st, f.job(model.ScopeMaterial, m.ID, model.StageMaterialExtract)); !ok || err != nil {
		t.Fatalf("material_extract must not wait for the draft: %v %v", ok, err)
	}

	if err := Launch(ctx, f.st, l.ID, launchSnaps()); err != nil {
		t.Fatal(err)
	}
	if ok, w, err := Deps(ctx, f.st, f.lj(l.ID, model.StagePrep)); !ok || w != "" || err != nil {
		t.Fatalf("prep after launch: %v %q %v", ok, w, err)
	}
	if ok, w, _ := Deps(ctx, f.st, f.lj(l.ID, model.StageASR)); ok || w != model.WaitUpstream(model.StagePrep) {
		t.Fatalf("asr after launch: %v %q", ok, w)
	}
}

// ---------- A2.1 display status ----------

func TestDisplayStatusesOutdated(t *testing.T) {
	D := model.StatusDone
	O := model.DisplayOutdated
	// Every non-done upstream status makes done downstream jobs outdated.
	for _, bad := range []string{model.StatusStale, model.StatusPending, model.StatusQueued,
		model.StatusRunning, model.StatusFailed, model.StatusCancelled} {
		got := DisplayStatuses(jobsOf(D, D, bad, D, D, D))
		want := map[string]string{
			model.StagePrep: D, model.StageASR: D, model.StageSummary: bad,
			model.StageRepair: O, model.StageTranslate: O, model.StageExport: O,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("summary %s:\n got  %v\n want %v", bad, got, want)
		}
	}
	// All done → done.
	all := DisplayStatuses(jobsOf(D, D, D, D, D, D))
	for _, s := range model.LectureStages {
		if all[s] != D {
			t.Fatalf("all done: %v", all)
		}
	}
	// Non-done statuses are returned as they are, even below a bad upstream.
	got := DisplayStatuses(jobsOf(D, model.StatusFailed, model.StatusPending, model.StatusPending, model.StatusPending, model.StatusPending))
	if got[model.StageASR] != model.StatusFailed || got[model.StageSummary] != model.StatusPending || got[model.StagePrep] != D {
		t.Fatalf("non-done statuses: %v", got)
	}
	// Transitive: repair stale → translate and export outdated (any upstream,
	// not only the immediate one).
	got = DisplayStatuses(jobsOf(D, D, D, model.StatusStale, D, D))
	if got[model.StageRepair] != model.StatusStale || got[model.StageTranslate] != O || got[model.StageExport] != O ||
		got[model.StageSummary] != D {
		t.Fatalf("repair stale: %v", got)
	}
	// export done below a pending translate whose upstream is all done.
	got = DisplayStatuses(jobsOf(D, D, D, D, model.StatusQueued, D))
	if got[model.StageExport] != O || got[model.StageTranslate] != model.StatusQueued || got[model.StageRepair] != D {
		t.Fatalf("translate queued: %v", got)
	}
	// Order of the input does not matter.
	js := jobsOf(D, model.StatusStale, D, D, D, D)
	rev := []model.Job{js[5], js[4], js[3], js[2], js[1], js[0]}
	if g1, g2 := DisplayStatuses(js), DisplayStatuses(rev); !reflect.DeepEqual(g1, g2) || g1[model.StageExport] != O {
		t.Fatalf("order dependent: %v vs %v", g1, g2)
	}
	if got := DisplayStatuses(nil); len(got) != 0 {
		t.Fatalf("no jobs: %v", got)
	}
	// Material scope works the same way.
	mat := []model.Job{
		{Scope: model.ScopeMaterial, Stage: model.StageMaterialExtract, Status: model.StatusStale},
		{Scope: model.ScopeMaterial, Stage: model.StageMaterialTerms, Status: D},
	}
	if got := DisplayStatuses(mat); got[model.StageMaterialTerms] != O {
		t.Fatalf("material: %v", got)
	}
}

// ---------- A2.2 stale reason and text, A2.3 earliest stale ----------

func TestStaleReasonFormats(t *testing.T) {
	three, zero := 3, 0
	tr := model.StageTranslate // its input contains all three kinds of cause
	j := model.Job{Stage: tr, Stats: model.JobStats{GlossaryVersion: &three}}
	at := func(stage string) model.Job {
		return model.Job{Stage: stage, Stats: model.JobStats{GlossaryVersion: &three}}
	}
	cases := []struct {
		cause StaleCause
		j     model.Job
		want  string
	}{
		{StaleCause{Kind: StaleGlossary}, j, rGlossary(ip(3), 4)},
		{StaleCause{Kind: StaleGlossary}, model.Job{Stage: tr}, rGlossary(nil, 4)},
		{StaleCause{Kind: StaleGlossary}, model.Job{Stage: tr, Stats: model.JobStats{GlossaryVersion: &zero}}, rGlossary(ip(0), 4)},
		{StaleCause{Kind: StaleMaterial, Material: "week3.pdf"}, j, rMaterial("week3.pdf")},
		{StaleCause{Kind: StaleSummary}, j, rSummary()},
		{StaleCause{}, j, rInput()},
		{StaleCause{Kind: "something-else"}, j, rInput()},
		// a cause only describes the stages whose input contains it (§4.4)
		{StaleCause{Kind: StaleGlossary}, at(model.StageRepair), rGlossary(ip(3), 4)},
		{StaleCause{Kind: StaleGlossary}, at(model.StageSummary), rInput()},
		{StaleCause{Kind: StaleMaterial, Material: "week3.pdf"}, at(model.StageSummary), rMaterial("week3.pdf")},
		{StaleCause{Kind: StaleMaterial, Material: "week3.pdf"}, at(model.StageRepair), rInput()},
		{StaleCause{Kind: StaleSummary}, at(model.StageRepair), rSummary()},
		{StaleCause{Kind: StaleSummary}, at(model.StageASR), rInput()},
	}
	for _, c := range cases {
		if got := StaleReason(c.cause, c.j, 4); got != c.want {
			t.Errorf("StaleReason(%+v, %s) = %q, want %q", c.cause, c.j.Stage, got, c.want)
		}
	}
}

func TestStaleStagesTextAndEarliest(t *testing.T) {
	D, S := model.StatusDone, model.StatusStale
	none := jobsOf(D, D, D, D, D, D)
	if len(StaleStages(none)) != 0 || StaleText(none) != "" || EarliestStale(none) != "" {
		t.Fatalf("nothing stale: %v %q %q", StaleStages(none), StaleText(none), EarliestStale(none))
	}

	g := "术语表已更新（v3 → v4）"
	js := jobsOf(D, D, D, S, S, D)
	js[3].StaleReason, js[4].StaleReason = sp(g), sp(g)
	if got := StaleStages(js); !reflect.DeepEqual(got, []string{model.StageRepair, model.StageTranslate}) {
		t.Fatalf("StaleStages = %v", got)
	}
	if got, want := StaleText(js), "术语表已更新（v3 → v4），需要更新：修复、翻译"; got != want {
		t.Fatalf("StaleText = %q, want %q", got, want)
	}
	if got := EarliestStale(js); got != model.StageRepair {
		t.Fatalf("EarliestStale = %q", got)
	}

	// Distinct reasons in pipeline order; no reason (nil or "") → 输入已变化,
	// counted once; input order does not matter.
	js = jobsOf(D, S, S, S, S, D)
	js[1].StaleReason = nil
	js[2].StaleReason = sp("")
	js[3].StaleReason = sp(g)
	js[4].StaleReason = sp("课件已变化：week3.pdf")
	rev := []model.Job{js[5], js[4], js[3], js[2], js[1], js[0]}
	want := "输入已变化；术语表已更新（v3 → v4）；课件已变化：week3.pdf，需要更新：转写、摘要、修复、翻译"
	if got := StaleText(rev); got != want {
		t.Fatalf("StaleText = %q\nwant        %q", got, want)
	}
	if got := StaleStages(rev); !reflect.DeepEqual(got, []string{model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate}) {
		t.Fatalf("StaleStages = %v", got)
	}
	if got := EarliestStale(rev); got != model.StageASR {
		t.Fatalf("EarliestStale = %q", got)
	}
}

// ---------- A2.2 stale_reason written by the invalidation ----------

// v11Hashers: asr hashes only the wav (A1: no glossary); repair and
// translate hash the glossary (en + zh); summary hashes the materials.
func v11Hashers(st db.Store, courseID string) map[string]Hasher {
	sum := func(parts ...string) string {
		h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return hex.EncodeToString(h[:])
	}
	glossary := func(ctx context.Context) string {
		ts, _ := st.ListTerms(ctx, courseID)
		var parts []string
		for _, t := range ts {
			if t.Enabled {
				parts = append(parts, t.En+"=>"+t.Zh)
			}
		}
		return strings.Join(parts, ", ")
	}
	materials := func(ctx context.Context) string {
		ms, _ := st.ListMaterials(ctx, courseID)
		var parts []string
		for _, m := range ms {
			parts = append(parts, m.Filename)
		}
		return strings.Join(parts, ", ")
	}
	return map[string]Hasher{
		model.StageASR: HasherFunc(func(ctx context.Context, id string) (string, error) { return sum("wav", id), nil }),
		model.StageSummary: HasherFunc(func(ctx context.Context, id string) (string, error) {
			return sum("asr", id, materials(ctx)), nil
		}),
		model.StageRepair: HasherFunc(func(ctx context.Context, id string) (string, error) {
			return sum("repair", id, glossary(ctx)), nil
		}),
		model.StageTranslate: HasherFunc(func(ctx context.Context, id string) (string, error) {
			return sum("translate", id, glossary(ctx)), nil
		}),
	}
}

// recordHashes stores the current hash of every hashed stage as the job's
// input_hash (as if the stages had just run).
func (f *fixture) recordHashes(hashers map[string]Hasher, lectureID string) {
	f.t.Helper()
	for stage, h := range hashers {
		v, err := h.InputHash(f.ctx, lectureID)
		if err != nil {
			f.t.Fatal(err)
		}
		f.setHash(model.ScopeLecture, lectureID, stage, v)
	}
}

func (f *fixture) setGlossaryVersionStat(lectureID, stage string, v int) {
	f.t.Helper()
	j := f.lj(lectureID, stage)
	j.Stats.GlossaryVersion = &v
	if err := f.st.SetJobStats(f.ctx, j.ID, j.Stats); err != nil {
		f.t.Fatal(err)
	}
}

func reasonOf(j model.Job) string {
	if j.StaleReason == nil {
		return "<nil>"
	}
	return *j.StaleReason
}

func TestInvalidationWritesStaleReason(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Source: model.SourceManual, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BumpGlossaryVersion(ctx, f.course.ID); err != nil { // v1
		t.Fatal(err)
	}
	hashers := v11Hashers(f.st, f.course.ID)
	l1 := f.lecture()
	l2 := f.lecture()
	for _, l := range []model.Lecture{l1, l2} {
		f.allDone(l.ID)
		f.recordHashes(hashers, l.ID)
	}
	// l1's repair and translate ran with glossary v1; l2's jobs recorded no
	// version (e.g. runs from before v1.1).
	f.setGlossaryVersionStat(l1.ID, model.StageRepair, 1)
	f.setGlossaryVersionStat(l1.ID, model.StageTranslate, 1)

	// Nothing changed → nothing stale, no reason anywhere.
	if changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary}); err != nil || len(changed) != 0 {
		t.Fatalf("no-op: %v %v", changed, err)
	}

	// Two glossary edits (v1 → v3), then one invalidation.
	term.Zh = "缓存"
	f.st.UpdateTerm(ctx, term)
	f.st.BumpGlossaryVersion(ctx, f.course.ID)
	f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "TLB", Source: model.SourceManual, Enabled: true})
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v3
	changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 4 {
		t.Fatalf("changed = %+v", changed)
	}
	for _, j := range changed {
		if j.Status != model.StatusStale || j.StaleReason == nil {
			t.Fatalf("returned job must carry status and reason: %+v", j)
		}
	}
	for _, s := range []string{model.StageRepair, model.StageTranslate} {
		if j := f.lj(l1.ID, s); j.Status != model.StatusStale || reasonOf(j) != rGlossary(ip(1), 3) {
			t.Errorf("l1 %s: %s %q", s, j.Status, reasonOf(j))
		}
		if j := f.lj(l2.ID, s); j.Status != model.StatusStale || reasonOf(j) != rGlossary(nil, 3) {
			t.Errorf("l2 %s: %s %q", s, j.Status, reasonOf(j))
		}
	}
	// Jobs that did not become stale get no reason (A1: asr ignores the glossary).
	for _, s := range []string{model.StagePrep, model.StageASR, model.StageSummary, model.StageExport} {
		if j := f.lj(l1.ID, s); j.Status != model.StatusDone || j.StaleReason != nil {
			t.Errorf("l1 %s: %s %q", s, j.Status, reasonOf(j))
		}
	}
	if got, want := StaleText(mustJobs(t, f, l1.ID)), "术语表已更新（v1 → v3），需要更新：修复、翻译"; got != want {
		t.Fatalf("StaleText = %q, want %q", got, want)
	}

	// A material change only writes a reason with done → stale: it makes
	// summary stale, and repair/translate keep their glossary reason (only
	// a glossary cause refreshes a glossary reason).
	mat := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "week3.pdf", Path: "p"}
	f.st.CreateMaterial(ctx, mat)
	changed, err = CheckStaleWith(ctx, f.st, hashers, l1.ID, StaleCause{Kind: StaleMaterial, Material: "week3.pdf"})
	if err != nil || len(changed) != 1 || changed[0].Stage != model.StageSummary {
		t.Fatalf("material change: %+v %v", changed, err)
	}
	if j := f.lj(l1.ID, model.StageSummary); reasonOf(j) != rMaterial("week3.pdf") {
		t.Fatalf("summary reason = %q", reasonOf(j))
	}
	if j := f.lj(l1.ID, model.StageRepair); reasonOf(j) != rGlossary(ip(1), 3) {
		t.Fatalf("repair reason overwritten: %q", reasonOf(j))
	}
	if got, want := StaleText(mustJobs(t, f, l1.ID)), "课件已变化：week3.pdf；术语表已更新（v1 → v3），需要更新：摘要、修复、翻译"; got != want {
		t.Fatalf("StaleText = %q, want %q", got, want)
	}
	// CheckStaleWith only touched l1.
	if j := f.lj(l2.ID, model.StageSummary); j.Status != model.StatusDone {
		t.Fatalf("l2 summary = %s", j.Status)
	}

	// The new jobs of a rerun have no reason.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l1.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		if j := f.lj(l1.ID, s); j.Status != model.StatusPending || j.StaleReason != nil {
			t.Fatalf("rerun %s: %s %q", s, j.Status, reasonOf(j))
		}
	}
	if StaleText(mustJobs(t, f, l1.ID)) != "" {
		t.Fatal("no stale job after rerun")
	}

	// The backward-compatible wrappers (no cause) write "输入已变化".
	f.st.CreateMaterial(ctx, model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "week4.pdf", Path: "p"})
	if _, err := InvalidateCourse(ctx, f.st, hashers, f.course.ID); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l2.ID, model.StageSummary); j.Status != model.StatusStale || reasonOf(j) != rInput() {
		t.Fatalf("wrapper: %s %q", j.Status, reasonOf(j))
	}
}

func mustJobs(t *testing.T, f *fixture, lectureID string) []model.Job {
	t.Helper()
	js, err := f.st.ListCurrentJobs(f.ctx, model.ScopeLecture, lectureID)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// ---------- A4.2 retry keeps the snapshot; RetryWith replaces it ----------

func TestRetryKeepsSnapshot(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := launchSnaps()
	l := f.launched(snaps)
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	before := f.lj(l.ID, model.StageASR)

	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StageASR); err != nil {
		t.Fatal(err)
	}
	after := f.lj(l.ID, model.StageASR)
	if after.ID != before.ID || after.Status != model.StatusQueued || after.Attempt != before.Attempt+1 {
		t.Fatalf("retry: %+v", after)
	}
	if !reflect.DeepEqual(f.snapshots(l.ID), mergeSnaps(snaps)) {
		t.Fatalf("retry changed snapshots: %v", f.snapshots(l.ID))
	}
	// RetryWith(nil) behaves exactly like Retry.
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning, model.StatusCancelled)
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, nil); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageASR); j.ID != before.ID || j.Status != model.StatusQueued || j.Attempt != before.Attempt+2 {
		t.Fatalf("RetryWith(nil): %+v", j)
	}
	if !reflect.DeepEqual(f.snapshots(l.ID), mergeSnaps(snaps)) {
		t.Fatalf("RetryWith(nil) changed snapshots: %v", f.snapshots(l.ID))
	}
}

func TestRetryWithReplacesSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := launchSnaps()
	l := f.launched(snaps)
	for _, s := range []string{model.StagePrep, model.StageASR} {
		f.done(model.ScopeLecture, l.ID, s)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageSummary, model.StatusQueued, model.StatusRunning)
	if err := TransitionJob(ctx, f.st, f.lj(l.ID, model.StageSummary).ID, model.StatusRunning, model.StatusFailed,
		db.JobFinish{Error: sp("provider deleted")}); err != nil {
		t.Fatal(err)
	}
	failed := f.lj(l.ID, model.StageSummary)
	cur := map[string]string{
		model.StageASR:       asrSnap("large-v3"), // upstream: never touched
		model.StageSummary:   llmSnap("anthropic", "claude-x", 0.2),
		model.StageRepair:    llmSnap("anthropic", "claude-x", 0.1),
		model.StageTranslate: llmSnap("anthropic", "claude-x", 0.3, 3),
	}

	// Not retryable → 409 and no snapshot changed (same transaction).
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageRepair, cur); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry pending repair: %v", err)
	}
	if !reflect.DeepEqual(f.snapshots(l.ID), mergeSnaps(snaps)) {
		t.Fatalf("refused retry changed snapshots: %v", f.snapshots(l.ID))
	}

	// Only the retried stage and the listed downstream stages change.
	newSnaps := map[string]string{
		model.StageASR:     cur[model.StageASR],
		model.StageSummary: cur[model.StageSummary],
		model.StageRepair:  cur[model.StageRepair],
	}
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary, newSnaps); err != nil {
		t.Fatal(err)
	}
	j := f.lj(l.ID, model.StageSummary)
	if j.ID != failed.ID || j.Status != model.StatusQueued || j.Attempt != failed.Attempt+1 || j.Error != nil {
		t.Fatalf("RetryWith: %+v", j)
	}
	want := mergeSnaps(snaps)
	want[model.StageSummary] = cur[model.StageSummary]
	want[model.StageRepair] = cur[model.StageRepair]
	if have := f.snapshots(l.ID); !reflect.DeepEqual(have, want) {
		t.Fatalf("RetryWith snapshots:\n have %v\n want %v", have, want)
	}
}

// mergeSnaps completes launch snapshots with the empty ones of prep/export.
func mergeSnaps(snaps map[string]string) map[string]string {
	out := map[string]string{model.StagePrep: model.EmptySnapshot, model.StageExport: model.EmptySnapshot}
	for k, v := range snaps {
		out[k] = v
	}
	return out
}

// ---------- A4.2 incremental translate keeps the last translate snapshot ----------

func TestIncrementalTranslateKeepsSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := launchSnaps()
	l := f.launched(snaps)
	f.allDone(l.ID)
	// A distinguishable export snapshot proves export copies its own one.
	exportSnap := `{"marker":"export-1"}`
	if err := f.st.SetJobSnapshot(ctx, f.lj(l.ID, model.StageExport).ID, exportSnap); err != nil {
		t.Fatal(err)
	}
	oldTr := f.lj(l.ID, model.StageTranslate)
	started, err := StartIncrementalTranslate(ctx, f.st, l.ID)
	if err != nil || !started {
		t.Fatalf("start: %v %v", started, err)
	}
	tr := f.lj(l.ID, model.StageTranslate)
	if tr.ID == oldTr.ID || tr.ConfigSnapshot != snaps[model.StageTranslate] {
		t.Fatalf("incremental translate snapshot = %q, want %q", tr.ConfigSnapshot, snaps[model.StageTranslate])
	}
	if ex := f.lj(l.ID, model.StageExport); ex.ConfigSnapshot != exportSnap {
		t.Fatalf("incremental export snapshot = %q", ex.ConfigSnapshot)
	}

	// After a rerun with "使用当前设置" the most recent translate snapshot is
	// the new one, and the next incremental run uses it.
	f.done(model.ScopeLecture, l.ID, model.StageTranslate)
	f.done(model.ScopeLecture, l.ID, model.StageExport)
	t2 := llmSnap("anthropic", "claude-x", 0.3, 4)
	if err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate,
		RerunOpts{NewSnapshots: map[string]string{model.StageTranslate: t2}}); err != nil {
		t.Fatal(err)
	}
	f.done(model.ScopeLecture, l.ID, model.StageTranslate)
	f.done(model.ScopeLecture, l.ID, model.StageExport)
	if started, err := StartIncrementalTranslate(ctx, f.st, l.ID); err != nil || !started {
		t.Fatalf("second start: %v %v", started, err)
	}
	if tr := f.lj(l.ID, model.StageTranslate); tr.ConfigSnapshot != t2 {
		t.Fatalf("incremental after rerun: %q", tr.ConfigSnapshot)
	}
	if ex := f.lj(l.ID, model.StageExport); ex.ConfigSnapshot != exportSnap {
		t.Fatalf("export snapshot lost: %q", ex.ConfigSnapshot)
	}
}

// ---------- A4.2/A4.3 rerun and snapshots ----------

func TestRerunWithSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := launchSnaps()
	l := f.launched(snaps)
	f.allDone(l.ID)
	old := map[string]model.Job{}
	for _, s := range model.LectureStages {
		old[s] = f.lj(l.ID, s)
	}

	// Default (Rerun): every new job copies the snapshot of the job it
	// replaces, with the current prompt version (v2 G5.5, lead Gate 1).
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.LectureStages {
		j := f.lj(l.ID, s)
		if want := withPromptVersion(s, old[s].ConfigSnapshot, model.PromptVersionOf(s)); j.ConfigSnapshot != want {
			t.Errorf("%s snapshot %q, want %q", s, j.ConfigSnapshot, want)
		}
	}
	if j := f.lj(l.ID, model.StageRepair); j.ID == old[model.StageRepair].ID || j.Status != model.StatusPending {
		t.Fatalf("repair must be a new pending job: %+v", j)
	}

	// NewSnapshots overrides only the listed stages; stages outside the
	// rerun range (summary when rerunning repair) are never touched.
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		f.done(model.ScopeLecture, l.ID, s)
	}
	r2 := llmSnap("anthropic", "claude-x", 0.1)
	sumBefore := f.lj(l.ID, model.StageSummary)
	err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageRepair, RerunOpts{NewSnapshots: map[string]string{
		model.StageRepair:  r2,
		model.StageSummary: llmSnap("anthropic", "claude-x", 0.2),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageSummary); j.ID != sumBefore.ID || j.ConfigSnapshot != sumBefore.ConfigSnapshot || j.Status != model.StatusDone {
		t.Fatalf("summary outside the rerun range changed: %+v", j)
	}
	if j := f.lj(l.ID, model.StageRepair); j.ConfigSnapshot != r2 || j.Status != model.StatusPending {
		t.Fatalf("repair: %+v", j)
	}
	if j := f.lj(l.ID, model.StageTranslate); j.ConfigSnapshot != withPromptVersion(model.StageTranslate, snaps[model.StageTranslate], model.PromptVersionTranslate) {
		t.Fatalf("translate (not listed) must keep its snapshot: %q", j.ConfigSnapshot)
	}
	if j := f.lj(l.ID, model.StageExport); j.ConfigSnapshot != model.EmptySnapshot {
		t.Fatalf("export: %q", j.ConfigSnapshot)
	}

	// A draft cannot be rerun.
	d := f.draft()
	err = RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, d.ID, model.StagePrep, RerunOpts{})
	if !errors.Is(err, ErrDraft) || !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun draft: %v", err)
	}
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, d.ID, model.StagePrep); !errors.Is(err, ErrDraft) {
		t.Fatalf("Rerun draft: %v", err)
	}

	// Course and material jobs keep "{}".
	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "a.pdf", Path: "p"}
	f.st.CreateMaterial(ctx, m)
	f.done(model.ScopeMaterial, m.ID, model.StageMaterialExtract)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeMaterial, m.ID, model.StageMaterialExtract); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.MaterialStages {
		if j := f.job(model.ScopeMaterial, m.ID, s); j.ConfigSnapshot != model.EmptySnapshot {
			t.Fatalf("material %s snapshot %q", s, j.ConfigSnapshot)
		}
	}
	if err := ResetGlossarySeed(ctx, f.st, f.course.ID); err != nil {
		t.Fatal(err)
	}
	if j := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed); j.ConfigSnapshot != model.EmptySnapshot {
		t.Fatalf("glossary_seed snapshot %q", j.ConfigSnapshot)
	}
}

// A model/provider change never makes a stage stale (A4.3 last line): the
// snapshots are not hash inputs. The hashers below cover every input of the
// lecture stages that lives in the database (sentences, summary, glossary,
// materials), so any change of those would mark stale — a snapshot change
// must not.
func TestSnapshotChangesNeverMakeStale(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Zh: "缓存", Source: model.SourceManual, Enabled: true})
	snaps := launchSnaps()
	l := f.launched(snaps)
	f.allDone(l.ID)
	f.st.InsertSentences(ctx, l.ID, []model.Sentence{{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "the cache", RepairedText: "the cache"}})
	f.st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "sum", InputHash: "h"})

	inputs := func(ctx context.Context, id string) string {
		var parts []string
		ss, _ := f.st.ListSentences(ctx, id, 0, 0)
		for _, s := range ss {
			parts = append(parts, s.ASRText, s.Text)
		}
		if sm, err := f.st.GetSummary(ctx, id); err == nil {
			parts = append(parts, sm.Text)
		}
		ts, _ := f.st.ListTerms(ctx, f.course.ID)
		for _, t := range ts {
			parts = append(parts, fmt.Sprintf("%s=>%s:%v", t.En, t.Zh, t.Enabled))
		}
		ms, _ := f.st.ListMaterials(ctx, f.course.ID)
		for _, m := range ms {
			parts = append(parts, m.Filename, strings.Join(m.PagesMD, "\n"))
		}
		h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return hex.EncodeToString(h[:])
	}
	hashers := map[string]Hasher{}
	for _, s := range model.SnapshotStages {
		stage := s
		hashers[s] = HasherFunc(func(ctx context.Context, id string) (string, error) { return stage + inputs(ctx, id), nil })
	}
	f.recordHashes(hashers, l.ID)

	// Every snapshot changes (provider, model, temperature, window, ASR model).
	other := map[string]string{
		model.StageASR:       asrSnap("large-v3"),
		model.StageSummary:   llmSnap("anthropic", "claude-x", 0.5),
		model.StageRepair:    llmSnap("anthropic", "claude-x", 0.5),
		model.StageTranslate: llmSnap("anthropic", "claude-x", 0.5, 7),
	}
	for stage, snap := range other {
		if err := f.st.SetJobSnapshot(ctx, f.lj(l.ID, stage).ID, snap); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := CheckStale(ctx, f.st, hashers, l.ID); err != nil || len(changed) != 0 {
		t.Fatalf("snapshot change made stages stale: %+v %v", changed, err)
	}
	if changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{}); err != nil || len(changed) != 0 {
		t.Fatalf("snapshot change made stages stale (course): %+v %v", changed, err)
	}
	for _, s := range model.LectureStages {
		if st := f.lj(l.ID, s).Status; st != model.StatusDone {
			t.Fatalf("%s = %s", s, st)
		}
	}
	// Control: the hashers do react to a real input change.
	f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "TLB", Source: model.SourceManual, Enabled: true})
	if changed, err := CheckStale(ctx, f.st, hashers, l.ID); err != nil || len(changed) != len(model.SnapshotStages) {
		t.Fatalf("control: %+v %v", changed, err)
	}
}

// ---------- A4.3 snapshot diffs ----------

func TestSnapshotDiffs(t *testing.T) {
	snaps := mergeSnaps(launchSnaps())
	mk := func(s map[string]string) []model.Job {
		var out []model.Job
		for _, st := range model.LectureStages {
			out = append(out, model.Job{Scope: model.ScopeLecture, Stage: st, Status: model.StatusDone, ConfigSnapshot: s[st]})
		}
		return out
	}
	jobs := mk(snaps)
	current := launchSnaps()

	// Identical → nil, for every stage.
	for _, st := range model.LectureStages {
		if d := SnapshotDiffs(jobs, st, current, nil); d != nil {
			t.Fatalf("rerun %s with identical settings: %+v", st, d)
		}
	}
	// provider_name is display only.
	renamed := launchSnaps()
	ls, _ := model.ParseLLMSnapshot(renamed[model.StageRepair])
	ls.ProviderName = "DeepSeek (renamed)"
	renamed[model.StageRepair] = model.MarshalSnapshot(ls)
	if d := SnapshotDiffs(jobs, model.StageASR, renamed, nil); d != nil {
		t.Fatalf("provider rename is not a model change: %+v", d)
	}

	// The translate model changed: rerunning summary lists translate (a
	// downstream stage), with the raw snapshots; usable defaults to true.
	cur := launchSnaps()
	cur[model.StageTranslate] = llmSnap("anthropic", "claude-x", 0.3, 3)
	d := SnapshotDiffs(jobs, model.StageSummary, cur, nil)
	want := []ModelDiff{{Stage: model.StageTranslate, Snapshot: snaps[model.StageTranslate], Current: cur[model.StageTranslate], SnapshotUsable: true}}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("diffs:\n got  %+v\n want %+v", d, want)
	}
	if d := SnapshotDiffs(jobs, model.StageExport, cur, nil); d != nil {
		t.Fatalf("rerun export: %+v", d)
	}

	// Upstream differences are not listed; order is the pipeline order even
	// when the jobs are shuffled; temperature and window count.
	cur = launchSnaps()
	cur[model.StageASR] = asrSnap("large-v3")
	cur[model.StageRepair] = llmSnap("deepseek", "deepseek-chat", 0.7)
	cur[model.StageTranslate] = llmSnap("deepseek", "deepseek-chat", 0.3, 5)
	shuffled := []model.Job{jobs[4], jobs[0], jobs[5], jobs[2], jobs[1], jobs[3]}
	d = SnapshotDiffs(shuffled, model.StageSummary, cur, nil)
	if len(d) != 2 || d[0].Stage != model.StageRepair || d[1].Stage != model.StageTranslate {
		t.Fatalf("diffs = %+v", d)
	}
	d = SnapshotDiffs(shuffled, model.StagePrep, cur, nil)
	if len(d) != 3 || d[0].Stage != model.StageASR || d[1].Stage != model.StageRepair || d[2].Stage != model.StageTranslate {
		t.Fatalf("diffs from prep = %+v", d)
	}

	// Unusable snapshots: usable fills SnapshotUsable/Reason.
	gone := mergeSnaps(launchSnaps())
	gone[model.StageSummary] = llmSnap("old-provider", "deepseek-chat", 0.2)
	gone[model.StageRepair] = llmSnap("old-provider", "deepseek-chat", 0.1)
	usable := func(stage, raw string) (bool, *model.ErrorInfo) {
		if s, ok := model.ParseLLMSnapshot(raw); ok && s.ProviderID == "old-provider" {
			return false, &model.ErrorInfo{Code: "snapshot.unusable.provider_deleted", Params: map[string]any{"provider": "old-provider"}, Detail: "提供商已删除"}
		}
		return true, nil
	}
	d = SnapshotDiffs(mk(gone), model.StageSummary, launchSnaps(), usable)
	if len(d) != 2 {
		t.Fatalf("diffs = %+v", d)
	}
	for _, x := range d {
		if x.SnapshotUsable || x.ReasonInfo == nil || x.ReasonInfo.Code != "snapshot.unusable.provider_deleted" || x.Reason != x.ReasonInfo.Detail {
			t.Fatalf("unusable diff = %+v", x)
		}
	}
	// A job without a snapshot (e.g. never backfilled) differs from a set one.
	empty := mk(map[string]string{})
	d = SnapshotDiffs(empty, model.StageTranslate, launchSnaps(), usable)
	if len(d) != 1 || d[0].Stage != model.StageTranslate || d[0].Snapshot != "" {
		t.Fatalf("empty snapshot diff = %+v", d)
	}
}

// ---------- translate window ----------

func TestTranslateWindow(t *testing.T) {
	f := newFixture(t)
	snaps := launchSnaps()
	snaps[model.StageTranslate] = llmSnap("deepseek", "deepseek-chat", 0.3, 5)
	l := f.launched(snaps)
	if x, err := TranslateWindow(f.ctx, f.st, l.ID); err != nil || x != 5 {
		t.Fatalf("window = %d %v", x, err)
	}
	d := f.draft() // empty snapshot → default
	if x, err := TranslateWindow(f.ctx, f.st, d.ID); err != nil || x != model.DefaultTranslateWindow {
		t.Fatalf("default window = %d %v", x, err)
	}
	if _, err := TranslateWindow(f.ctx, f.st, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown lecture: %v", err)
	}
}

// ---------- one-time backfill of v1 lectures ----------

func TestBackfillSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	cur := launchSnaps()

	l1 := f.lecture() // launched by v1 (004 backfill), no snapshots
	f.allDone(l1.ID)
	retired := f.lj(l1.ID, model.StageTranslate)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l1.ID, model.StageTranslate); err != nil {
		t.Fatal(err)
	}
	l2 := f.draft()
	l3 := f.lecture()
	keep := llmSnap("other", "kept", 0.2)
	if err := f.st.SetJobSnapshot(ctx, f.lj(l3.ID, model.StageSummary).ID, keep); err != nil {
		t.Fatal(err)
	}
	c2 := model.Course{ID: db.NewID(), Name: "Other"}
	f.st.CreateCourse(ctx, c2)
	now := db.Now()
	l4 := model.Lecture{ID: db.NewID(), CourseID: c2.ID, Title: "X", MediaPath: "m", LaunchedAt: &now}
	if err := f.st.CreateLecture(ctx, l4); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillSnapshots(ctx, f.st, cur)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4+3+4 {
		t.Fatalf("updated %d jobs, want 11", n)
	}
	if have := f.snapshots(l1.ID); !reflect.DeepEqual(have, mergeSnaps(cur)) {
		t.Fatalf("l1: %v", have)
	}
	want3 := mergeSnaps(cur)
	want3[model.StageSummary] = keep
	if have := f.snapshots(l3.ID); !reflect.DeepEqual(have, want3) {
		t.Fatalf("l3: %v", have)
	}
	if have := f.snapshots(l4.ID); !reflect.DeepEqual(have, mergeSnaps(cur)) {
		t.Fatalf("l4 (other course): %v", have)
	}
	for s, snap := range f.snapshots(l2.ID) {
		if snap != model.EmptySnapshot {
			t.Fatalf("draft %s got %q", s, snap)
		}
	}
	if old, _ := f.st.GetJob(ctx, retired.ID); old.IsCurrent || old.ConfigSnapshot != model.EmptySnapshot {
		t.Fatalf("non-current job touched: %+v", old)
	}
	if v, ok, _ := f.st.GetKV(ctx, model.KVSnapshotsBackfill); !ok || v == "" {
		t.Fatalf("kv marker not set: %q %v", v, ok)
	}

	// Second call: nothing, even with other settings and a new empty job.
	l5 := f.lecture()
	other := map[string]string{model.StageTranslate: llmSnap("anthropic", "claude-x", 0.3, 3)}
	if n, err := BackfillSnapshots(ctx, f.st, other); err != nil || n != 0 {
		t.Fatalf("second backfill: %d %v", n, err)
	}
	if have := f.snapshots(l1.ID); !reflect.DeepEqual(have, mergeSnaps(cur)) {
		t.Fatalf("second backfill changed l1: %v", have)
	}
	if j := f.lj(l5.ID, model.StageTranslate); j.ConfigSnapshot != model.EmptySnapshot {
		t.Fatalf("second backfill touched l5: %q", j.ConfigSnapshot)
	}
}

// "使用当前设置" for translate on retry: the whole lecture is translated
// again by the new model (no mix of old and new translations or zh_hash
// conventions, A4.2); re-snapshotting other stages leaves translations alone.
func TestRetryWithNewTranslateSnapshotClearsTranslations(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.fullLecture()
	f.st.SetTranslation(ctx, l.ID, 2, db.SentenceTr{Zh: sp("坏"), ZhStatus: model.ZhQAFailed, ZhHash: sp("zh"), QAError: sp("术语")})
	// Export fails and is retried with a new export snapshot: no clearing.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageExport, map[string]string{model.StageExport: `{"x":1}`}); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.st.CountZh(ctx, l.ID); c[model.ZhOK] != 2 || c[model.ZhQAFailed] != 1 {
		t.Fatalf("export retry touched translations: %v", c)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusRunning, model.StatusDone)

	// translate fails; retry keeping the snapshot: translations stay.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the rerun cleared them; a partial run wrote some again
		f.st.SetTranslation(ctx, l.ID, i, db.SentenceTr{Zh: sp("旧"), ZhStatus: model.ZhOK, ZhHash: sp("old-x")})
	}
	f.drive(model.ScopeLecture, l.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.st.CountZh(ctx, l.ID); c[model.ZhOK] != 3 {
		t.Fatalf("plain retry touched translations: %v", c)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageTranslate, model.StatusRunning, model.StatusFailed)

	// Retry with a new translate snapshot: everything is translated again.
	newTr := llmSnap("anthropic", "claude-x", 0.3, 5)
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate, map[string]string{model.StageTranslate: newTr}); err != nil {
		t.Fatal(err)
	}
	ss, _ := f.st.ListSentences(ctx, l.ID, 0, 0)
	for _, s := range ss {
		if s.Zh != nil || s.ZhStatus != model.ZhNone || s.ZhHash != nil || s.QAError != nil {
			t.Fatalf("sentence %d not cleared: %+v", s.Idx, s)
		}
	}
	if j := f.lj(l.ID, model.StageTranslate); j.ConfigSnapshot != newTr || j.Status != model.StatusQueued {
		t.Fatalf("translate = %+v", j)
	}
	// human_text and text are untouched.
	if s, _ := f.st.GetSentence(ctx, l.ID, 1); s.HumanText == nil || s.Text != "a miss penalty!" {
		t.Fatalf("sentence 1 = %+v", s)
	}
}

// An incremental translate run that failed and is retried with a new
// translate snapshot: the translations are cleared, so the run is a full
// translation and must not carry the incremental marker and base hash
// (the stage would otherwise compare the old base hash with the prefix hash
// of the new window and fail permanently, every time).
func TestRetryWithNewTranslateSnapshotDropsIncrementalNote(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.fullLecture()
	f.setHash(model.ScopeLecture, l.ID, model.StageTranslate, "prefix-hash-x3")
	f.st.SetZhStatus(ctx, l.ID, []int{1}, model.ZhStale)
	if started, err := StartIncrementalTranslate(ctx, f.st, l.ID); err != nil || !started {
		t.Fatalf("incremental: %v %v", started, err)
	}
	inc := f.lj(l.ID, model.StageTranslate)
	if base, ok := IncrementalBaseHash(inc.Stats.Note); !ok || base != "prefix-hash-x3" {
		t.Fatalf("setup note = %q", inc.Stats.Note)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning, model.StatusFailed)

	// A plain retry stays incremental (same model, same window).
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate, nil); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageTranslate); j.Stats.Note != inc.Stats.Note {
		t.Fatalf("plain retry changed the note: %q", j.Stats.Note)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageTranslate, model.StatusRunning, model.StatusFailed)

	// "使用当前设置" with another window: a full translation.
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate,
		map[string]string{model.StageTranslate: llmSnap("anthropic", "claude-x", 0.3, 5)}); err != nil {
		t.Fatal(err)
	}
	j := f.lj(l.ID, model.StageTranslate)
	if IsIncremental(j.Stats.Note) || j.Stats.Note != "" {
		t.Fatalf("note after retry with a new translate snapshot = %q", j.Stats.Note)
	}
	if _, ok := IncrementalBaseHash(j.Stats.Note); ok {
		t.Fatal("base hash survived")
	}
	if j.ID != inc.ID || j.Status != model.StatusQueued || j.Attempt != inc.Attempt+2 {
		t.Fatalf("job = %+v", j)
	}
	if c, _ := f.st.CountZh(ctx, l.ID); c[model.ZhNone] != 3 {
		t.Fatalf("translations not cleared: %v", c)
	}
}

// NOTES "v1.1 冻结与裁决" 9: the glossary reason is "the job's recorded
// version → the course's current version", so later glossary edits refresh
// the reason of jobs that are already stale for the glossary (real run: the
// glossary was at v6, lectures still said "v2 → v3"). A reason of another
// kind is kept (see TestInvalidationWritesStaleReason).
func TestGlossaryReasonFollowsLaterEdits(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Source: model.SourceManual, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v1
	hashers := v11Hashers(f.st, f.course.ID)
	l1 := f.lecture()
	l2 := f.lecture()
	for _, l := range []model.Lecture{l1, l2} {
		f.allDone(l.ID)
		f.recordHashes(hashers, l.ID)
	}
	f.setGlossaryVersionStat(l1.ID, model.StageRepair, 1)
	f.setGlossaryVersionStat(l1.ID, model.StageTranslate, 1)

	edit := func(zh string) {
		term.Zh = zh
		if err := f.st.UpdateTerm(ctx, term); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.BumpGlossaryVersion(ctx, f.course.ID); err != nil {
			t.Fatal(err)
		}
	}
	edit("缓存") // v2
	if _, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary}); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l1.ID, model.StageRepair); reasonOf(j) != rGlossary(ip(1), 2) {
		t.Fatalf("first edit: %q", reasonOf(j))
	}
	// both summaries become stale for a material: that reason must survive.
	mat := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "week3.pdf", Path: "p"}
	f.st.CreateMaterial(ctx, mat)
	for _, l := range []model.Lecture{l1, l2} {
		if _, err := CheckStaleWith(ctx, f.st, hashers, l.ID, StaleCause{Kind: StaleMaterial, Material: "week3.pdf"}); err != nil {
			t.Fatal(err)
		}
	}

	edit("高速缓存") // v3
	edit("缓存器")  // v4
	changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{model.StageRepair, model.StageTranslate} {
		if j := f.lj(l1.ID, s); j.Status != model.StatusStale || reasonOf(j) != rGlossary(ip(1), 4) {
			t.Errorf("l1 %s: %s %q", s, j.Status, reasonOf(j))
		}
		if j := f.lj(l2.ID, s); j.Status != model.StatusStale || reasonOf(j) != rGlossary(nil, 4) {
			t.Errorf("l2 %s: %s %q", s, j.Status, reasonOf(j))
		}
	}
	for _, l := range []model.Lecture{l1, l2} {
		if j := f.lj(l.ID, model.StageSummary); reasonOf(j) != rMaterial("week3.pdf") {
			t.Errorf("material reason overwritten: %q", reasonOf(j))
		}
	}
	// The refreshed jobs are returned (the scheduler announces them, so the
	// UI's stale text updates); the material-stale summary is not.
	n := 0
	for _, j := range changed {
		if j.Stage == model.StageSummary {
			t.Errorf("summary returned: %+v", j)
		}
		if j.StaleReason == nil || StaleReasonOf(j).Params["to"] != float64(4) {
			t.Errorf("returned job without the new reason: %+v", j)
		}
		n++
	}
	if n != 4 {
		t.Errorf("changed = %d jobs, want 4 (repair and translate of both lectures)", n)
	}
	// Nothing new: no rewrite, nothing returned.
	if changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary}); err != nil || len(changed) != 0 {
		t.Errorf("no-op: %+v %v", changed, err)
	}
}

// The refresh only applies to glossary reasons whose job input still differs
// from what it ran with: a "输入已变化" reason is kept, and a job whose input
// went back to its recorded hash keeps its reason (spec review).
func TestGlossaryReasonRefreshLimits(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Source: model.SourceManual, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v1
	hashers := v11Hashers(f.st, f.course.ID)
	l1 := f.lecture() // will get a glossary reason
	l2 := f.lecture() // will get "输入已变化"
	for _, l := range []model.Lecture{l1, l2} {
		f.allDone(l.ID)
		f.recordHashes(hashers, l.ID)
	}
	f.setGlossaryVersionStat(l1.ID, model.StageRepair, 1)
	setZh := func(zh string) {
		term.Zh = zh
		if err := f.st.UpdateTerm(ctx, term); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.BumpGlossaryVersion(ctx, f.course.ID); err != nil {
			t.Fatal(err)
		}
	}
	setZh("缓存")                                                      // v2
	if _, err := CheckStale(ctx, f.st, hashers, l2.ID); err != nil { // no cause
		t.Fatal(err)
	}
	if _, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary}); err != nil {
		t.Fatal(err)
	}
	if got := reasonOf(f.lj(l1.ID, model.StageRepair)); got != rGlossary(ip(1), 2) {
		t.Fatalf("l1 = %q", got)
	}
	if got := reasonOf(f.lj(l2.ID, model.StageRepair)); got != rInput() {
		t.Fatalf("l2 = %q", got)
	}

	setZh("高速缓存") // v3
	if _, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary}); err != nil {
		t.Fatal(err)
	}
	if got := reasonOf(f.lj(l2.ID, model.StageRepair)); got != rInput() {
		t.Errorf("a no-cause reason must be kept, got %q", got)
	}
	if got := reasonOf(f.lj(l1.ID, model.StageRepair)); got != rGlossary(ip(1), 3) {
		t.Errorf("l1 = %q", got)
	}

	// Back to the glossary the jobs ran with (v4 has v1's content): the
	// input equals the recorded hash, nothing is rewritten or announced.
	setZh("") // v4
	changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary})
	if err != nil {
		t.Fatal(err)
	}
	if got := reasonOf(f.lj(l1.ID, model.StageRepair)); got != rGlossary(ip(1), 3) {
		t.Errorf("input back to the recorded hash: reason must be kept, got %q", got)
	}
	for _, j := range changed {
		if j.ScopeID == l1.ID && j.Stage == model.StageRepair {
			t.Errorf("unchanged job announced: %+v", j)
		}
	}
}
