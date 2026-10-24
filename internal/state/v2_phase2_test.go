// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// Tests of the v2 phase 2 state rules: auxiliary tasks (SPEC-v2 P13.5, P7,
// P8.1, P13.3.2; lead ruling 1 of phase 2), the filename-rule trigger
// (P13.3.2, ruling 2), stale_reason as JSON (P14.3, ruling 5) and coded job
// errors (P14.3, ruling 6). Written before the implementation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---------- helpers ----------

// aux creates (or replaces) the auxiliary job of a stage and drives it to status.
func (f *fixture) aux(scope, scopeID, stage, status string) model.Job {
	f.t.Helper()
	if _, err := EnqueueAux(f.ctx, f.st, scope, scopeID, stage); err != nil {
		f.t.Fatalf("EnqueueAux %s: %v", stage, err)
	}
	f.to(scope, scopeID, stage, status)
	return f.job(scope, scopeID, stage)
}

func (f *fixture) lecAux(lectureID, stage, status string) model.Job {
	return f.aux(model.ScopeLecture, lectureID, stage, status)
}

func (f *fixture) hasJob(scope, scopeID, stage string) bool {
	f.t.Helper()
	_, err := f.st.GetCurrentJob(f.ctx, scope, scopeID, stage)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

// setRole assigns role to providerID/mdl ("" leaves the column NULL).
func (f *fixture) setRole(role, providerID, mdl string) {
	f.t.Helper()
	r := model.RoleSetting{Role: role, Params: "{}"}
	if providerID != "" {
		r.ProviderID = &providerID
	}
	if mdl != "" {
		r.Model = &mdl
	}
	if err := f.st.UpsertRoleSetting(f.ctx, r); err != nil {
		f.t.Fatal(err)
	}
}

// Stale reasons as P14.3 JSON (absent from: pass nil).
func rGlossary(from *int, to int) string {
	return model.StaleReasonJSON(model.StaleCodeGlossary, testVersionParams(from, to))
}
func rJanitor(from *int, to int) string {
	return model.StaleReasonJSON(model.StaleCodeJanitor, testVersionParams(from, to))
}
func rMaterial(name string) string {
	return model.StaleReasonJSON(model.StaleCodeMaterial, map[string]any{"filename": name})
}
func rSummary() string { return model.StaleReasonJSON(model.StaleCodeSummary, nil) }
func rInput() string   { return model.StaleReasonJSON(model.StaleCodeInput, nil) }

func testVersionParams(from *int, to int) map[string]any {
	p := map[string]any{"to": to}
	if from != nil {
		p["from"] = *from
	}
	return p
}

func ip(n int) *int { return &n }

// ---------- auxiliary tasks: dependencies ----------

func TestAuxDeps(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	for _, stage := range []string{model.StageTitle, model.StageChapters} {
		j := f.lecAux(l.ID, stage, model.StatusPending)
		if ok, w := f.deps(j); ok || w != model.WaitUpstream(model.StageSummary) {
			t.Errorf("%s before summary: ok=%v waiting=%q", stage, ok, w)
		}
	}
	st := f.lecAux(l.ID, model.StageStream, model.StatusPending)
	if ok, w := f.deps(st); ok || w != model.WaitUpstream(model.StagePrep) {
		t.Errorf("stream before prep: ok=%v waiting=%q", ok, w)
	}

	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	if ok, w := f.deps(st); !ok || w != "" {
		t.Errorf("stream after prep done: ok=%v waiting=%q", ok, w)
	}
	// Only summary matters for title/chapters: asr done is not enough.
	f.done(model.ScopeLecture, l.ID, model.StageASR)
	if ok, _ := f.deps(f.lj(l.ID, model.StageTitle)); ok {
		t.Error("title must wait for summary, not asr")
	}
	f.done(model.ScopeLecture, l.ID, model.StageSummary)
	for _, stage := range []string{model.StageTitle, model.StageChapters} {
		if ok, w := f.deps(f.lj(l.ID, stage)); !ok || w != "" {
			t.Errorf("%s after summary done: ok=%v waiting=%q", stage, ok, w)
		}
	}
	// A stale summary still has its output in place.
	f.drive(model.ScopeLecture, l.ID, model.StageSummary, model.StatusStale)
	if ok, _ := f.deps(f.lj(l.ID, model.StageTitle)); !ok {
		t.Error("title after summary stale must run")
	}

	// filename_rule has no dependency.
	fr := f.aux(model.ScopeCourse, f.course.ID, model.StageFilenameRule, model.StatusPending)
	if ok, w := f.deps(fr); !ok || w != "" {
		t.Errorf("filename_rule: ok=%v waiting=%q", ok, w)
	}

	// A draft's auxiliary jobs report not_started like its stages.
	d := f.draft()
	if ok, w := f.deps(f.lecAux(d.ID, model.StageStream, model.StatusPending)); ok || w != model.WaitNotStarted {
		t.Errorf("draft stream: ok=%v waiting=%q", ok, w)
	}
}

// ---------- auxiliary tasks: EnqueueAux ----------

func TestEnqueueAux(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.lecture()

	// Validation.
	for _, c := range []struct{ scope, id, stage string }{
		{model.ScopeLecture, l.ID, model.StageSummary},        // a pipeline stage
		{model.ScopeLecture, l.ID, model.StageGlossaryReview}, // a course task
		{model.ScopeCourse, f.course.ID, model.StageTitle},    // wrong scope
		{model.ScopeLecture, l.ID, model.StageFilenameRule},   // wrong scope
		{model.ScopeMaterial, l.ID, model.StageStream},        // wrong scope
		{model.ScopeLecture, l.ID, "nonsense"},                // unknown
	} {
		if _, err := EnqueueAux(ctx, f.st, c.scope, c.id, c.stage); !errors.Is(err, ErrBadInput) {
			t.Errorf("EnqueueAux(%s, %s) = %v, want ErrBadInput", c.scope, c.stage, err)
		}
	}
	if _, err := EnqueueAux(ctx, f.st, model.ScopeLecture, "no-such-lecture", model.StageTitle); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown lecture: %v", err)
	}
	if _, err := EnqueueAux(ctx, f.st, model.ScopeCourse, "no-such-course", model.StageFilenameRule); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown course: %v", err)
	}

	// None → a new pending job without snapshot.
	queued, err := EnqueueAux(ctx, f.st, model.ScopeLecture, l.ID, model.StageTitle)
	if err != nil || !queued {
		t.Fatalf("first: queued=%v err=%v", queued, err)
	}
	first := f.lj(l.ID, model.StageTitle)
	if first.Status != model.StatusPending || first.Attempt != 0 || !model.IsEmptySnapshot(first.ConfigSnapshot) || first.Scope != model.ScopeLecture {
		t.Fatalf("new title job = %+v", first)
	}

	// pending / queued / running → not again.
	for _, status := range []string{model.StatusPending, model.StatusQueued, model.StatusRunning} {
		f.drive(model.ScopeLecture, l.ID, model.StageTitle, nextStep(f.lj(l.ID, model.StageTitle).Status, status)...)
		queued, err := EnqueueAux(ctx, f.st, model.ScopeLecture, l.ID, model.StageTitle)
		if err != nil || queued {
			t.Fatalf("%s: queued=%v err=%v", status, queued, err)
		}
		if j := f.lj(l.ID, model.StageTitle); j.ID != first.ID || j.Status != status {
			t.Fatalf("%s: job replaced: %+v", status, j)
		}
	}

	// done / failed / cancelled / stale → replaced by a new pending job.
	for _, status := range []string{model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale} {
		old := f.lecAux(l.ID, model.StageChapters, status)
		if old.Status != status {
			t.Fatalf("setup %s: %s", status, old.Status)
		}
		if status == model.StatusFailed {
			msg := "boom"
			if err := f.st.SetJobError(ctx, old.ID, &msg); err != nil {
				t.Fatal(err)
			}
		}
		queued, err := EnqueueAux(ctx, f.st, model.ScopeLecture, l.ID, model.StageChapters)
		if err != nil || !queued {
			t.Fatalf("after %s: queued=%v err=%v", status, queued, err)
		}
		nj := f.lj(l.ID, model.StageChapters)
		if nj.ID == old.ID || nj.Status != model.StatusPending || nj.Attempt != 0 || nj.Error != nil {
			t.Fatalf("after %s: %+v", status, nj)
		}
		if oj, err := f.st.GetJob(ctx, old.ID); err != nil || oj.IsCurrent {
			t.Fatalf("after %s: old job must be retired: %+v %v", status, oj, err)
		}
	}

	// Course scope.
	if queued, err := EnqueueAux(ctx, f.st, model.ScopeCourse, f.course.ID, model.StageFilenameRule); err != nil || !queued {
		t.Fatalf("filename_rule: %v %v", queued, err)
	}
	if !f.hasJob(model.ScopeCourse, f.course.ID, model.StageFilenameRule) {
		t.Fatal("filename_rule job missing")
	}

	// Inside an outer transaction (the scheduler calls it there).
	err = f.st.Tx(ctx, func(tx db.Store) error {
		_, err := EnqueueAux(ctx, tx, model.ScopeLecture, l.ID, model.StageStream)
		return err
	})
	if err != nil || !f.hasJob(model.ScopeLecture, l.ID, model.StageStream) {
		t.Fatalf("in a transaction: %v", err)
	}
}

// nextStep lists the transitions from one status to a later one on the
// pending → queued → running path.
func nextStep(from, to string) []string {
	path := []string{model.StatusPending, model.StatusQueued, model.StatusRunning}
	var out []string
	started := false
	for _, s := range path {
		if started {
			out = append(out, s)
		}
		if s == from {
			started = true
		}
		if s == to {
			break
		}
	}
	if from == to {
		return nil
	}
	return out
}

// ---------- auxiliary tasks never count as pipeline stages ----------

func TestAuxNotInPipeline(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	f.allDone(l.ID)
	f.lecAux(l.ID, model.StageTitle, model.StatusFailed)
	f.lecAux(l.ID, model.StageChapters, model.StatusRunning)
	f.lecAux(l.ID, model.StageStream, model.StatusStale) // cannot happen in practice; must still be ignored
	jobs := mustJobs(t, f, l.ID)
	if len(jobs) != 9 {
		t.Fatalf("jobs = %d", len(jobs))
	}

	pipe, aux := SplitAux(jobs)
	if len(pipe) != 6 || len(aux) != 3 {
		t.Fatalf("SplitAux: %d pipeline, %d aux", len(pipe), len(aux))
	}
	for _, j := range aux {
		if !model.IsAuxStage(j.Stage) {
			t.Fatalf("aux part has %s", j.Stage)
		}
	}

	ds := DisplayStatuses(jobs)
	for _, s := range model.AuxStages {
		if _, ok := ds[s]; ok {
			t.Errorf("DisplayStatuses lists %s", s)
		}
	}
	for _, s := range model.LectureStages {
		if ds[s] != model.StatusDone {
			t.Errorf("display %s = %q", s, ds[s])
		}
	}
	if got := StaleStages(jobs); len(got) != 0 {
		t.Errorf("StaleStages = %v", got)
	}
	if got := EarliestStale(jobs); got != "" {
		t.Errorf("EarliestStale = %q", got)
	}
	if got := StaleText(jobs); got != "" {
		t.Errorf("StaleText = %q", got)
	}
	if !CanDelete(jobs) {
		t.Error("a running auxiliary task must not block deleting the lecture")
	}

	acts := StageActions(jobs, false)
	want := map[string][]string{
		model.StageTitle:    {model.ActionRetry},
		model.StageChapters: {model.ActionCancel},
		model.StageStream:   {},
		model.StageExport:   {model.ActionRerun},
		model.StagePrep:     {model.ActionRerun},
	}
	for s, w := range want {
		if !reflect.DeepEqual(acts[s], w) {
			t.Errorf("StageActions[%s] = %v, want %v", s, acts[s], w)
		}
	}

	// AuxActions: queued/running → cancel; failed/cancelled → retry; never rerun.
	for status, w := range map[string][]string{
		model.StatusPending: {}, model.StatusQueued: {model.ActionCancel}, model.StatusRunning: {model.ActionCancel},
		model.StatusDone: {}, model.StatusStale: {}, model.StatusFailed: {model.ActionRetry}, model.StatusCancelled: {model.ActionRetry},
	} {
		if got := AuxActions(model.Job{Stage: model.StageTitle, Status: status}); !reflect.DeepEqual(got, w) {
			t.Errorf("AuxActions(%s) = %v, want %v", status, got, w)
		}
		if got := AllowedActions(model.Job{Stage: model.StageStream, Status: status}, ActionOpts{}); !reflect.DeepEqual(got, w) {
			t.Errorf("AllowedActions(stream %s) = %v, want %v", status, got, w)
		}
	}

	// A running pipeline stage still blocks deletion.
	l2 := f.lecture()
	f.to(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusRunning)
	if CanDelete(mustJobs(t, f, l2.ID)) {
		t.Error("running prep must block deletion")
	}

	// A stale pipeline stage next to stale-looking aux jobs: only the stage.
	f.drive(model.ScopeLecture, l.ID, model.StageRepair, model.StatusStale)
	jobs = mustJobs(t, f, l.ID)
	if got := StaleStages(jobs); !reflect.DeepEqual(got, []string{model.StageRepair}) {
		t.Errorf("StaleStages = %v", got)
	}
	if got := EarliestStale(jobs); got != model.StageRepair {
		t.Errorf("EarliestStale = %q", got)
	}
	if got, want := StaleText(jobs), "输入已变化，需要更新：修复"; got != want {
		t.Errorf("StaleText = %q, want %q", got, want)
	}
}

// Launch leaves auxiliary jobs without a snapshot; the lecture's stage
// snapshots are written as before.
func TestLaunchSkipsAux(t *testing.T) {
	f := newFixture(t)
	d := f.draft()
	f.lecAux(d.ID, model.StageStream, model.StatusPending)
	snaps := launchSnaps()
	snaps[model.StageStream] = `{"bogus":1}`
	if err := Launch(f.ctx, f.st, d.ID, snaps); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(d.ID, model.StageStream); !model.IsEmptySnapshot(j.ConfigSnapshot) {
		t.Fatalf("stream snapshot = %q", j.ConfigSnapshot)
	}
	if j := f.lj(d.ID, model.StageSummary); j.ConfigSnapshot != snaps[model.StageSummary] {
		t.Fatalf("summary snapshot = %q", j.ConfigSnapshot)
	}
}

// §4.7 for auxiliary tasks: an interrupted one is failed and NOT requeued
// (no automatic retry); a pipeline stage is requeued as before.
func TestRecoverDoesNotRetryAux(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	f.to(model.ScopeLecture, l.ID, model.StagePrep, model.StatusRunning)
	f.lecAux(l.ID, model.StageStream, model.StatusRunning)
	f.aux(model.ScopeCourse, f.course.ID, model.StageFilenameRule, model.StatusRunning)
	if err := Recover(f.ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StagePrep); j.Status != model.StatusQueued || j.Attempt != 1 {
		t.Errorf("prep = %s attempt %d", j.Status, j.Attempt)
	}
	for _, j := range []model.Job{f.lj(l.ID, model.StageStream), f.job(model.ScopeCourse, f.course.ID, model.StageFilenameRule)} {
		if j.Status != model.StatusFailed || j.Attempt != 0 || j.Error == nil || *j.Error != model.ErrInterruptedByRestart || !IsInterrupted(j.Error) {
			t.Errorf("%s = %s attempt %d error %v", j.Stage, j.Status, j.Attempt, j.Error)
		}
	}
}

// Auxiliary tasks never take part in the invalidation (P13.5).
func TestAuxNotInvalidated(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	j := f.lecAux(l.ID, model.StageChapters, model.StatusDone)
	f.setHash(model.ScopeLecture, l.ID, model.StageChapters, "old")
	hashers := map[string]Hasher{model.StageChapters: HasherFunc(func(context.Context, string) (string, error) { return "new", nil })}
	if changed, err := CheckStale(f.ctx, f.st, hashers, l.ID); err != nil || len(changed) != 0 {
		t.Fatalf("CheckStale: %+v %v", changed, err)
	}
	if changed, err := InvalidateCourseWith(f.ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleSummary}); err != nil || len(changed) != 0 {
		t.Fatalf("InvalidateCourseWith: %+v %v", changed, err)
	}
	if got := f.lj(l.ID, model.StageChapters); got.Status != model.StatusDone || got.ID != j.ID {
		t.Fatalf("chapters = %+v", got)
	}
}

// Auxiliary tasks are retried and cancelled like stages, never rerun (a new
// run goes through EnqueueAux).
func TestAuxRetryCancelNoRerun(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.lecture()
	f.lecAux(l.ID, model.StageTitle, model.StatusDone)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTitle); !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun of an auxiliary task: %v", err)
	}
	f.lecAux(l.ID, model.StageChapters, model.StatusFailed)
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StageChapters); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageChapters); j.Status != model.StatusQueued || j.Attempt != 1 {
		t.Fatalf("retried chapters = %s attempt %d", j.Status, j.Attempt)
	}
	if err := CancelQueued(ctx, f.st, f.lj(l.ID, model.StageChapters).ID); err != nil {
		t.Fatal(err)
	}
	// A pipeline rerun leaves the auxiliary jobs alone.
	f.allDone(l.ID)
	title := f.lj(l.ID, model.StageTitle)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageTitle); j.ID != title.ID || j.Status != model.StatusDone {
		t.Fatalf("title after summary rerun = %+v", j)
	}
}

// ---------- RoleConfigured ----------

func TestRoleConfigured(t *testing.T) {
	f := newFixture(t)
	check := func(what string, want bool) {
		t.Helper()
		got, err := RoleConfigured(f.ctx, f.st, model.RoleOrganizer)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: RoleConfigured = %v, want %v", what, got, want)
		}
	}
	check("no row", false)
	p := f.provider("DS", "https://api.deepseek.example.test", "sk-test-not-a-real-key")
	f.setRole(model.RoleOrganizer, p.ID, "")
	check("no model", false)
	f.setRole(model.RoleOrganizer, p.ID, "deepseek-flash")
	check("provider with key", true)
	local := f.provider("Local", "http://127.0.0.1:8080/v1", "")
	f.setRole(model.RoleOrganizer, local.ID, "qwen")
	check("local provider without key", true)
	nokey := f.provider("NoKey", "https://api.example.test/v1", "")
	f.setRole(model.RoleOrganizer, nokey.ID, "m")
	check("remote provider without key", false)
	// Another role does not count.
	f.setRole(model.RoleChapters, p.ID, "deepseek-flash")
	check("only chapters configured", false)
	if ok, err := RoleConfigured(f.ctx, f.st, model.RoleChapters); err != nil || !ok {
		t.Errorf("chapters: %v %v", ok, err)
	}
}

// ---------- P13.3.2 trigger ----------

func TestFilenameRuleWanted(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	check := func(what string, want bool) {
		t.Helper()
		got, err := FilenameRuleWanted(ctx, f.st, f.course.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: FilenameRuleWanted = %v, want %v", what, got, want)
		}
	}
	if _, err := FilenameRuleWanted(ctx, f.st, "no-such-course"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown course: %v", err)
	}
	check("no lectures", false)

	l1 := f.lecture() // original_filename "" (before v2): never a sample
	check("lecture without an original filename", false)
	if err := f.st.SetLectureOriginalFilename(ctx, l1.ID, "CSC1003_2026-09-30_1000.mp4"); err != nil {
		t.Fatal(err)
	}
	check("unknown time, unused name", true)

	samples := func(names ...string) {
		t.Helper()
		b, _ := json.Marshal(names)
		if err := f.st.SetKV(ctx, model.KVFilenameSamples+f.course.ID, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	samples("CSC1003_2026-09-30_1000.mp4")
	check("name already used for a rule", false)

	l2 := f.lecture()
	f.st.SetLectureOriginalFilename(ctx, l2.ID, "CSC1003_2026-10-07_1000.mp4")
	check("a new unused name", true)
	at := "2026-10-07T10:00"
	if err := f.st.SetLectureRecordedAt(ctx, l2.ID, &at, model.RecordedAtFilename); err != nil {
		t.Fatal(err)
	}
	check("its time is known (filename)", false)
	f.st.SetLectureRecordedAt(ctx, l2.ID, &at, model.RecordedAtMetadata)
	check("its time is known (metadata)", false)
	f.st.SetLectureRecordedAt(ctx, l2.ID, nil, model.RecordedAtNone)
	check("unknown again", true)

	// A broken samples value counts as no samples.
	f.st.SetKV(ctx, model.KVFilenameSamples+f.course.ID, "not json")
	check("broken samples", true)

	// A rule the user edited is never regenerated automatically.
	user := `{"regex":"^x","source":"user"}`
	if err := f.st.SetCourseFilenameRule(ctx, f.course.ID, &user); err != nil {
		t.Fatal(err)
	}
	check("user rule", false)
	llm := `{"regex":"^x","source":"llm"}`
	f.st.SetCourseFilenameRule(ctx, f.course.ID, &llm)
	check("llm rule that does not match", true)
}

// ---------- P14.3 stale_reason JSON ----------

func TestStaleReasonJSONFormats(t *testing.T) {
	three, zero := 3, 0
	tr := model.StageTranslate
	j := model.Job{Stage: tr, Stats: model.JobStats{GlossaryVersion: &three}}
	cases := []struct {
		cause StaleCause
		j     model.Job
		want  string
	}{
		{StaleCause{Kind: StaleGlossary}, j, `{"code":"glossary_updated","params":{"from":3,"to":4}}`},
		{StaleCause{Kind: StaleGlossary}, model.Job{Stage: tr}, `{"code":"glossary_updated","params":{"to":4}}`},
		{StaleCause{Kind: StaleGlossary}, model.Job{Stage: tr, Stats: model.JobStats{GlossaryVersion: &zero}}, `{"code":"glossary_updated","params":{"from":0,"to":4}}`},
		{StaleCause{Kind: StaleJanitor}, j, `{"code":"glossary_janitor","params":{"from":3,"to":4}}`},
		{StaleCause{Kind: StaleMaterial, Material: "week3.pdf"}, j, `{"code":"material_changed","params":{"filename":"week3.pdf"}}`},
		{StaleCause{Kind: StaleMaterial}, j, `{"code":"material_changed"}`},
		{StaleCause{Kind: StaleSummary}, j, `{"code":"summary_updated"}`},
		{StaleCause{}, j, `{"code":"input_changed"}`},
	}
	for _, c := range cases {
		if got := StaleReason(c.cause, c.j, 4); got != c.want {
			t.Errorf("StaleReason(%+v) = %s, want %s", c.cause, got, c.want)
		}
	}
}

func TestStaleReasonFromText(t *testing.T) {
	cases := []struct {
		text string
		want model.StaleReasonCode
	}{
		{model.StaleReasonGlossary(ip(3), 4), model.StaleReasonCode{Code: model.StaleCodeGlossary, Params: map[string]any{"from": 3, "to": 4}}},
		{model.StaleReasonGlossary(nil, 12), model.StaleReasonCode{Code: model.StaleCodeGlossary, Params: map[string]any{"to": 12}}},
		{model.StaleReasonJanitor(ip(0), 1), model.StaleReasonCode{Code: model.StaleCodeJanitor, Params: map[string]any{"from": 0, "to": 1}}},
		{model.StaleReasonJanitor(nil, 7), model.StaleReasonCode{Code: model.StaleCodeJanitor, Params: map[string]any{"to": 7}}},
		{model.StaleReasonSummary(), model.StaleReasonCode{Code: model.StaleCodeSummary}},
		{model.StaleReasonMaterial("week 3：intro.pdf"), model.StaleReasonCode{Code: model.StaleCodeMaterial, Params: map[string]any{"filename": "week 3：intro.pdf"}}},
		{"课件已变化", model.StaleReasonCode{Code: model.StaleCodeMaterial}},
		{"输入已变化", model.StaleReasonCode{Code: model.StaleCodeInput}},
		{"something else", model.StaleReasonCode{Code: model.StaleCodeLegacy, Params: map[string]any{"text": "something else"}}},
		{"术语表已更新（v3 → v4）还有别的", model.StaleReasonCode{Code: model.StaleCodeLegacy, Params: map[string]any{"text": "术语表已更新（v3 → v4）还有别的"}}},
	}
	for _, c := range cases {
		got := StaleReasonFromText(c.text)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("StaleReasonFromText(%q) = %#v, want %#v", c.text, got, c.want)
		}
		// The text the transitional renderer gives back is the old one.
		raw := model.StaleReasonJSON(got.Code, got.Params)
		if back := StaleReasonText(raw); back != c.text && got.Code != model.StaleCodeInput {
			t.Errorf("StaleReasonText(%s) = %q, want %q", raw, back, c.text)
		}
	}
}

func TestStaleReasonTextAndOf(t *testing.T) {
	cases := []struct{ raw, want string }{
		{rGlossary(ip(1), 3), "术语表已更新（v1 → v3）"},
		{rGlossary(nil, 3), "术语表已更新（现为 v3）"},
		{rJanitor(ip(2), 5), "术语表已整理（v2 → v5）"},
		{rMaterial("week3.pdf"), "课件已变化：week3.pdf"},
		{model.StaleReasonJSON(model.StaleCodeMaterial, nil), "课件已变化"},
		{rSummary(), "摘要已更新"},
		{rInput(), "输入已变化"},
		{model.StaleReasonJSON(model.StaleCodeLegacy, map[string]any{"text": "旧的说明"}), "旧的说明"},
		{"", "输入已变化"},
		{"  ", "输入已变化"},
		{"术语表已更新（v1 → v2）", "术语表已更新（v1 → v2）"}, // not converted yet
		{`{"code":"something_new"}`, "输入已变化"},
	}
	for _, c := range cases {
		if got := StaleReasonText(c.raw); got != c.want {
			t.Errorf("StaleReasonText(%q) = %q, want %q", c.raw, got, c.want)
		}
	}

	if got := StaleReasonOf(model.Job{}); got.Code != model.StaleCodeInput {
		t.Errorf("no reason: %+v", got)
	}
	if got := StaleReasonOf(model.Job{StaleReason: sp(" ")}); got.Code != model.StaleCodeInput {
		t.Errorf("blank reason: %+v", got)
	}
	got := StaleReasonOf(model.Job{StaleReason: sp("术语表已整理（v3 → v4）")})
	if got.Code != model.StaleCodeJanitor || fmt.Sprint(got.Params["from"]) != "3" || fmt.Sprint(got.Params["to"]) != "4" {
		t.Errorf("old text: %+v", got)
	}
	got = StaleReasonOf(model.Job{StaleReason: sp(rMaterial("w.pdf"))})
	if got.Code != model.StaleCodeMaterial || got.Params["filename"] != "w.pdf" {
		t.Errorf("JSON: %+v", got)
	}

	// StaleText renders JSON reasons exactly as it rendered the texts.
	D, S := model.StatusDone, model.StatusStale
	js := jobsOf(D, S, S, S, S, D)
	js[1].StaleReason = nil
	js[2].StaleReason = sp(rMaterial("week3.pdf"))
	js[3].StaleReason = sp(rGlossary(ip(3), 4))
	js[4].StaleReason = sp(rGlossary(ip(3), 4))
	want := "输入已变化；课件已变化：week3.pdf；术语表已更新（v3 → v4），需要更新：转写、摘要、修复、翻译"
	if got := StaleText(js); got != want {
		t.Errorf("StaleText = %q\nwant        %q", got, want)
	}
}

// The one-time conversion at startup (kv v2.stale_reason_json).
func TestConvertStaleReasons(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	texts := map[string]string{
		model.StageASR:       "输入已变化",
		model.StageSummary:   "课件已变化：week3.pdf",
		model.StageRepair:    "术语表已更新（v1 → v3）",
		model.StageTranslate: "术语表已整理（现为 v3）",
		model.StageExport:    "人工写的说明",
	}
	l := f.lecture()
	f.allDone(l.ID)
	for stage, text := range texts {
		r := text
		if err := Transition(ctx, f.st, model.ScopeLecture, l.ID, stage, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &r}); err != nil {
			t.Fatal(err)
		}
	}
	// A retired job's reason is converted too; an already converted one is kept.
	l2 := f.lecture()
	f.allDone(l2.ID)
	already := rSummary()
	if err := Transition(ctx, f.st, model.ScopeLecture, l2.ID, model.StageSummary, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &already}); err != nil {
		t.Fatal(err)
	}
	old := "摘要已更新"
	if err := Transition(ctx, f.st, model.ScopeLecture, l2.ID, model.StageRepair, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &old}); err != nil {
		t.Fatal(err)
	}
	retired := f.lj(l2.ID, model.StageRepair).ID
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l2.ID, model.StageRepair); err != nil {
		t.Fatal(err)
	}

	n, err := ConvertStaleReasons(ctx, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("converted %d reasons, want 6", n)
	}
	want := map[string]string{
		model.StageASR:       rInput(),
		model.StageSummary:   rMaterial("week3.pdf"),
		model.StageRepair:    rGlossary(ip(1), 3),
		model.StageTranslate: rJanitor(nil, 3),
		model.StageExport:    model.StaleReasonJSON(model.StaleCodeLegacy, map[string]any{"text": "人工写的说明"}),
	}
	for stage, w := range want {
		if got := reasonOf(f.lj(l.ID, stage)); got != w {
			t.Errorf("%s: %s, want %s", stage, got, w)
		}
	}
	if got := reasonOf(f.lj(l2.ID, model.StageSummary)); got != already {
		t.Errorf("already JSON: %s", got)
	}
	if rj, err := f.st.GetJob(ctx, retired); err != nil || rj.StaleReason == nil || *rj.StaleReason != rSummary() {
		t.Errorf("retired job: %+v %v", rj.StaleReason, err)
	}
	if v, ok, _ := f.st.GetKV(ctx, model.KVStaleReasonJSON); !ok || v != "1" {
		t.Errorf("kv marker = %q %v", v, ok)
	}
	// The texts render as before.
	if got, want := StaleText(mustJobs(t, f, l.ID)), "输入已变化；课件已变化：week3.pdf；术语表已更新（v1 → v3）；术语表已整理（现为 v3）；人工写的说明，需要更新：转写、摘要、修复、翻译、导出"; got != want {
		t.Errorf("StaleText = %q\nwant        %q", got, want)
	}

	// Once only.
	r := "摘要已更新"
	l3 := f.lecture()
	f.allDone(l3.ID)
	Transition(ctx, f.st, model.ScopeLecture, l3.ID, model.StageSummary, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &r})
	if n, err := ConvertStaleReasons(ctx, f.st); err != nil || n != 0 {
		t.Errorf("second run: %d %v", n, err)
	}
	if got := reasonOf(f.lj(l3.ID, model.StageSummary)); got != r {
		t.Errorf("second run touched %s", got)
	}
}

// A glossary refresh compares codes and updates params: an unconverted
// text reason is refreshed into JSON, and the janitor code is kept.
func TestGlossaryReasonRefreshFromText(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Source: model.SourceManual, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v1
	hashers := v11Hashers(f.st, f.course.ID)
	l := f.lecture()
	f.allDone(l.ID)
	f.recordHashes(hashers, l.ID)
	f.setGlossaryVersionStat(l.ID, model.StageRepair, 1)
	f.setGlossaryVersionStat(l.ID, model.StageTranslate, 1)
	term.Zh = "缓存"
	f.st.UpdateTerm(ctx, term)
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v2
	for stage, text := range map[string]string{
		model.StageRepair:    "术语表已整理（v1 → v2）",
		model.StageTranslate: "术语表已更新（v1 → v2）",
	} {
		r := text
		if err := Transition(ctx, f.st, model.ScopeLecture, l.ID, stage, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &r}); err != nil {
			t.Fatal(err)
		}
	}
	term.Zh = "高速缓存"
	f.st.UpdateTerm(ctx, term)
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v3
	changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleGlossary})
	if err != nil || len(changed) != 2 {
		t.Fatalf("changed = %+v %v", changed, err)
	}
	if got := reasonOf(f.lj(l.ID, model.StageRepair)); got != rJanitor(ip(1), 3) {
		t.Errorf("repair = %s", got)
	}
	if got := reasonOf(f.lj(l.ID, model.StageTranslate)); got != rGlossary(ip(1), 3) {
		t.Errorf("translate = %s", got)
	}
}

// ---------- P14.3 coded job errors ----------

func TestJobErrorEncoding(t *testing.T) {
	inner := errors.New("ffmpeg: Invalid data found when processing input")
	coded := model.Errorf("media.corrupt", map[string]any{"file": "a.mp4"}, inner)
	for _, err := range []error{coded, fmt.Errorf("prep: %w", coded), &wrapped{coded}} {
		raw := EncodeJobError(err)
		info, text := ParseJobError(raw)
		if info == nil {
			t.Fatalf("%v: not coded: %s", err, raw)
		}
		if info.Code != "media.corrupt" || info.Params["file"] != "a.mp4" || info.Detail != inner.Error() {
			t.Errorf("%v: info = %+v", err, info)
		}
		if text != inner.Error() {
			t.Errorf("%v: text = %q", err, text)
		}
	}
	// A coded error without a wrapped error keeps its own detail; the text
	// falls back to the code.
	bare := &model.CodedError{Info: model.ErrorInfo{Code: "chapters.invalid"}}
	info, text := ParseJobError(EncodeJobError(bare))
	if info == nil || info.Code != "chapters.invalid" || info.Detail != "" || text != "chapters.invalid" {
		t.Errorf("bare: %+v %q", info, text)
	}
	withDetail := &model.CodedError{Info: model.ErrorInfo{Code: "x.y", Detail: "given"}}
	if info, text := ParseJobError(EncodeJobError(withDetail)); info == nil || info.Detail != "given" || text != "given" {
		t.Errorf("detail kept: %+v %q", info, text)
	}

	// Plain errors are stored as their text (legacy).
	plain := errors.New("llm: 500 internal error")
	if raw := EncodeJobError(plain); raw != plain.Error() {
		t.Errorf("plain = %q", raw)
	}
	for _, raw := range []string{
		"llm: 500 internal error",
		"",
		"interrupted by restart (and more)", // only the exact §4.7 text is the code
		`{"error":{"message":"bad key"}}`,   // a provider body quoted as text
		`{"code":"invalid_api_key","message":"x"}`, // unknown fields: not ours
		`{"code":""}`, // no code
		`{"code":"x"`, // broken
	} {
		if info, text := ParseJobError(raw); info != nil || text != raw {
			t.Errorf("ParseJobError(%q) = %+v %q", raw, info, text)
		}
	}

	// §4.7 stores the plain text "interrupted by restart" (the stored content
	// of §4 does not change); reading maps it to job.interrupted with the
	// text as its detail (lead ruling at the phase-2 gate). The coded form
	// written by the phase-2 build before that ruling reads the same.
	for _, raw := range []string{model.ErrInterruptedByRestart, " " + model.ErrInterruptedByRestart + "\n",
		EncodeJobError(model.Errorf(CodeInterrupted, nil, errors.New(model.ErrInterruptedByRestart)))} {
		info, text := ParseJobError(raw)
		if info == nil || info.Code != CodeInterrupted || info.Detail != model.ErrInterruptedByRestart || len(info.Params) != 0 || text != model.ErrInterruptedByRestart {
			t.Errorf("ParseJobError(%q) = %+v %q, want job.interrupted", raw, info, text)
		}
		if r := raw; !IsInterrupted(&r) {
			t.Errorf("IsInterrupted(%q) = false", raw)
		}
	}
	if interruptedError != model.ErrInterruptedByRestart {
		t.Errorf("Recover writes %q, want the §4.7 text", interruptedError)
	}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "outer: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
