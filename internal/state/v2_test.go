// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// Tests of the v2 state rules (SPEC-v2 G3.1, appendix D2; lead rulings 2,
// 3, 4 and 11 in .claude/v2-brief.md). Written before the implementation.

import (
	"errors"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---------- helpers ----------

// pathTo lists the transitions that take a fresh pending job to status.
var pathTo = map[string][]string{
	model.StatusPending:   nil,
	model.StatusQueued:    {model.StatusQueued},
	model.StatusRunning:   {model.StatusQueued, model.StatusRunning},
	model.StatusDone:      {model.StatusQueued, model.StatusRunning, model.StatusDone},
	model.StatusFailed:    {model.StatusQueued, model.StatusRunning, model.StatusFailed},
	model.StatusCancelled: {model.StatusQueued, model.StatusCancelled},
	model.StatusStale:     {model.StatusQueued, model.StatusRunning, model.StatusDone, model.StatusStale},
}

// to drives the current (pending) job of a stage to status.
func (f *fixture) to(scope, scopeID, stage, status string) {
	f.t.Helper()
	f.drive(scope, scopeID, stage, pathTo[status]...)
}

// provider inserts a provider with a fake key (never a real one).
func (f *fixture) provider(name, baseURL, key string) model.Provider {
	f.t.Helper()
	p := model.Provider{ID: db.NewID(), Name: name, Type: model.ProviderAnthropic, BaseURL: baseURL, APIKey: key,
		MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone}
	if err := f.st.InsertProvider(f.ctx, p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) setJanitorRole(providerID, mdl string) {
	f.t.Helper()
	r := model.RoleSetting{Role: model.RoleGlossaryJanitor, Params: "{}"}
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

// configureJanitor makes the glossary_janitor role usable.
func (f *fixture) configureJanitor() {
	f.t.Helper()
	p := f.provider("Opus", "https://api.anthropic.example.test", "sk-test-not-a-real-key")
	f.setJanitorRole(p.ID, "claude-opus-5-5")
}

func (f *fixture) review() model.Job {
	f.t.Helper()
	return f.job(model.ScopeCourse, f.course.ID, model.StageGlossaryReview)
}

func (f *fixture) hasReview() bool {
	f.t.Helper()
	_, err := f.st.GetCurrentJob(f.ctx, model.ScopeCourse, f.course.ID, model.StageGlossaryReview)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

// newReview creates a pending glossary_review job of the course.
func (f *fixture) newReview() model.Job {
	f.t.Helper()
	if _, err := EnqueueGlossaryReview(f.ctx, f.st, f.course.ID); err != nil {
		f.t.Fatal(err)
	}
	return f.review()
}

func (f *fixture) dirty() string {
	f.t.Helper()
	v, _, err := f.st.GetKV(f.ctx, model.KVGlossaryReviewDirtyPrefix+f.course.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) syllabus() {
	f.t.Helper()
	if err := f.st.UpsertSyllabus(f.ctx, model.Syllabus{CourseID: f.course.ID, Filename: "s.md", Path: "s.md", Text: sp("x")}); err != nil {
		f.t.Fatal(err)
	}
	if err := ResetGlossarySeed(f.ctx, f.st, f.course.ID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) material(name string, lectureID *string) model.Material {
	f.t.Helper()
	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, LectureID: lectureID, Filename: name, Path: name}
	if err := f.st.CreateMaterial(f.ctx, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) deps(j model.Job) (bool, string) {
	f.t.Helper()
	ok, w, err := Deps(f.ctx, f.st, j)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok, w
}

// ---------- JanitorConfigured ----------

func TestJanitorConfigured(t *testing.T) {
	f := newFixture(t)
	check := func(what string, want bool) {
		t.Helper()
		got, err := JanitorConfigured(f.ctx, f.st)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: JanitorConfigured = %v, want %v", what, got, want)
		}
	}
	check("no role row", false)
	withKey := f.provider("Opus", "https://api.anthropic.example.test", "sk-test-not-a-real-key")
	noKey := f.provider("NoKey", "https://api.example.test", "")
	local := f.provider("Local", "http://127.0.0.1:11434/v1", "")
	f.setJanitorRole("", "")
	check("role without provider and model", false)
	f.setJanitorRole(withKey.ID, "")
	check("role without model", false)
	f.setJanitorRole(withKey.ID, "   ")
	check("blank model", false)
	f.setJanitorRole(noKey.ID, "m")
	check("provider without key", false)
	f.setJanitorRole(local.ID, "m")
	check("local provider without key (like settings.SnapshotUsable)", true)
	f.setJanitorRole(withKey.ID, "claude-opus-5-5")
	check("provider with key", true)
	// Inside a transaction too.
	if err := f.st.Tx(f.ctx, func(tx db.Store) error {
		ok, err := JanitorConfigured(f.ctx, tx)
		if err == nil && !ok {
			t.Error("inside a transaction: false")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------- EnqueueGlossaryReview ----------

func TestEnqueueGlossaryReviewNotConfigured(t *testing.T) {
	f := newFixture(t)
	queued, err := EnqueueGlossaryReview(f.ctx, f.st, f.course.ID)
	if err != nil || queued {
		t.Fatalf("not configured: queued=%v err=%v", queued, err)
	}
	if f.hasReview() {
		t.Fatal("a glossary_review job was created although the janitor is not configured")
	}
	if f.dirty() != "" {
		t.Fatal("dirty set although the janitor is not configured")
	}
}

func TestEnqueueGlossaryReview(t *testing.T) {
	f := newFixture(t)
	f.configureJanitor()
	ctx := f.ctx

	// none → a pending job.
	queued, err := EnqueueGlossaryReview(ctx, f.st, f.course.ID)
	if err != nil || !queued {
		t.Fatalf("none: queued=%v err=%v", queued, err)
	}
	j := f.review()
	if j.Status != model.StatusPending || j.Scope != model.ScopeCourse || j.Attempt != 0 || !model.IsEmptySnapshot(j.ConfigSnapshot) {
		t.Fatalf("new job = %+v", j)
	}

	// pending / queued → nothing (several triggers merge into one run).
	for _, status := range []string{model.StatusPending, model.StatusQueued} {
		if status == model.StatusQueued {
			f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusQueued)
		}
		before := f.review()
		queued, err := EnqueueGlossaryReview(ctx, f.st, f.course.ID)
		if err != nil || !queued {
			t.Fatalf("%s: queued=%v err=%v", status, queued, err)
		}
		if after := f.review(); after.ID != before.ID || after.Status != status {
			t.Fatalf("%s: job replaced or changed: %+v → %+v", status, before, after)
		}
		if f.dirty() != "" {
			t.Fatalf("%s: dirty set", status)
		}
	}

	// running → dirty, the job is untouched.
	f.drive(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusRunning)
	running := f.review()
	queued, err = EnqueueGlossaryReview(ctx, f.st, f.course.ID)
	if err != nil || !queued {
		t.Fatalf("running: queued=%v err=%v", queued, err)
	}
	if f.dirty() != "1" {
		t.Fatalf("running: dirty = %q", f.dirty())
	}
	if after := f.review(); after.ID != running.ID || after.Status != model.StatusRunning {
		t.Fatalf("running job changed: %+v", after)
	}

	// done, stale, failed, cancelled → retired and replaced by a new pending job.
	for _, status := range []string{model.StatusDone, model.StatusStale, model.StatusFailed, model.StatusCancelled} {
		old := f.newReviewIn(status)
		queued, err := EnqueueGlossaryReview(ctx, f.st, f.course.ID)
		if err != nil || !queued {
			t.Fatalf("%s: queued=%v err=%v", status, queued, err)
		}
		nj := f.review()
		if nj.ID == old.ID || nj.Status != model.StatusPending || nj.Attempt != 0 || nj.Error != nil || nj.StaleReason != nil {
			t.Fatalf("%s: new job = %+v (old %s)", status, nj, old.ID)
		}
		if o, err := f.st.GetJob(ctx, old.ID); err != nil || o.IsCurrent || o.Status != status {
			t.Fatalf("%s: old job = %+v %v", status, o, err)
		}
	}

	// Inside a transaction (MergeTerms calls it in its own transaction).
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusDone)
	old := f.review()
	if err := f.st.Tx(ctx, func(tx db.Store) error {
		_, err := EnqueueGlossaryReview(ctx, tx, f.course.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if nj := f.review(); nj.ID == old.ID || nj.Status != model.StatusPending {
		t.Fatalf("in a transaction: %+v", nj)
	}

	// Unknown course.
	if _, err := EnqueueGlossaryReview(ctx, f.st, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown course: %v", err)
	}
}

// newReviewIn makes sure the current glossary_review is a fresh job driven
// to status and returns it.
func (f *fixture) newReviewIn(status string) model.Job {
	f.t.Helper()
	j := f.review()
	if j.Status != model.StatusPending {
		if j.Status == model.StatusRunning {
			f.drive(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusDone)
		}
		if _, err := EnqueueGlossaryReview(f.ctx, f.st, f.course.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, status)
	return f.review()
}

// ---------- Deps ----------

// asr waits for the course's glossary_review only while it is pending,
// queued or running (ruling 3, appendix D2: a failed janitor never blocks).
func TestDepsASRWaitsForGlossaryReview(t *testing.T) {
	for status, wait := range map[string]bool{
		"":                  false, // no glossary_review job
		model.StatusPending: true, model.StatusQueued: true, model.StatusRunning: true,
		model.StatusDone: false, model.StatusFailed: false, model.StatusCancelled: false, model.StatusStale: false,
	} {
		t.Run("review="+status, func(t *testing.T) {
			f := newFixture(t)
			f.configureJanitor()
			l := f.lecture()
			f.done(model.ScopeLecture, l.ID, model.StagePrep)
			if status != "" {
				f.newReview()
				f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, status)
			}
			ok, w := f.deps(f.lj(l.ID, model.StageASR))
			if wait && (ok || w != model.WaitGlossaryReview) {
				t.Fatalf("want waiting for glossary_review, got ok=%v %q", ok, w)
			}
			if !wait && (!ok || w != "") {
				t.Fatalf("want no wait, got ok=%v %q", ok, w)
			}
		})
	}
}

func TestDepsASRGlossaryReviewOrder(t *testing.T) {
	f := newFixture(t)
	f.configureJanitor()
	f.newReview() // pending
	// A draft still reports not_started first.
	d := f.draft()
	if ok, w := f.deps(f.lj(d.ID, model.StageASR)); ok || w != model.WaitNotStarted {
		t.Fatalf("draft: ok=%v %q", ok, w)
	}
	// The immediate upstream comes first.
	l := f.lecture()
	if ok, w := f.deps(f.lj(l.ID, model.StageASR)); ok || w != model.WaitUpstream(model.StagePrep) {
		t.Fatalf("prep pending: ok=%v %q", ok, w)
	}
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	// The syllabus terms (the root of the wait) come before the review.
	f.syllabus()
	if ok, w := f.deps(f.lj(l.ID, model.StageASR)); ok || w != model.WaitGlossarySeed {
		t.Fatalf("seed pending: ok=%v %q", ok, w)
	}
	f.done(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	// A bound material's terms come before the review.
	m := f.material("w1.pdf", &l.ID)
	if ok, w := f.deps(f.lj(l.ID, model.StageASR)); ok || w != model.WaitMaterialTerms("w1.pdf") {
		t.Fatalf("bound material: ok=%v %q", ok, w)
	}
	f.done(model.ScopeMaterial, m.ID, model.StageMaterialExtract)
	f.done(model.ScopeMaterial, m.ID, model.StageMaterialTerms)
	if ok, w := f.deps(f.lj(l.ID, model.StageASR)); ok || w != model.WaitGlossaryReview {
		t.Fatalf("review pending: ok=%v %q", ok, w)
	}
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusDone)
	if ok, w := f.deps(f.lj(l.ID, model.StageASR)); !ok || w != "" {
		t.Fatalf("review done: ok=%v %q", ok, w)
	}
}

// glossary_review waits for the syllabus terms (when the course has a
// syllabus) and for the term extraction of every material of the course
// that is in progress; an upstream that failed or was cancelled never makes
// it wait forever (ruling 2).
func TestDepsGlossaryReviewSeed(t *testing.T) {
	// No syllabus: glossary_seed does not matter.
	f := newFixture(t)
	f.configureJanitor()
	if ok, w := f.deps(f.newReview()); !ok || w != "" {
		t.Fatalf("no syllabus, no materials: ok=%v %q", ok, w)
	}
	// A syllabus without a glossary_seed job (should not happen): no wait.
	if err := f.st.UpsertSyllabus(f.ctx, model.Syllabus{CourseID: f.course.ID, Filename: "s.md", Path: "s.md"}); err != nil {
		t.Fatal(err)
	}
	if ok, w := f.deps(f.review()); !ok || w != "" {
		t.Fatalf("syllabus without seed job: ok=%v %q", ok, w)
	}
	for status, wait := range map[string]bool{
		model.StatusPending: true, model.StatusQueued: true, model.StatusRunning: true,
		model.StatusDone: false, model.StatusStale: false, model.StatusFailed: false, model.StatusCancelled: false,
	} {
		t.Run("seed="+status, func(t *testing.T) {
			f := newFixture(t)
			f.configureJanitor()
			f.syllabus()
			f.to(model.ScopeCourse, f.course.ID, model.StageGlossarySeed, status)
			ok, w := f.deps(f.newReview())
			if wait && (ok || w != model.WaitUpstream(model.StageGlossarySeed)) {
				t.Fatalf("want upstream:glossary_seed, got ok=%v %q", ok, w)
			}
			if !wait && (!ok || w != "") {
				t.Fatalf("want no wait, got ok=%v %q", ok, w)
			}
		})
	}
}

func TestDepsGlossaryReviewMaterials(t *testing.T) {
	cases := []struct {
		extract, terms string
		wait           bool
	}{
		{model.StatusPending, model.StatusPending, true},
		{model.StatusQueued, model.StatusPending, true},
		{model.StatusRunning, model.StatusPending, true},
		{model.StatusDone, model.StatusPending, true},
		{model.StatusStale, model.StatusPending, true},
		{model.StatusDone, model.StatusQueued, true},
		{model.StatusDone, model.StatusRunning, true},
		{model.StatusDone, model.StatusDone, false},
		{model.StatusDone, model.StatusStale, false},
		{model.StatusDone, model.StatusFailed, false},
		{model.StatusDone, model.StatusCancelled, false},
		// the extraction failed or was cancelled: material_terms stays
		// pending forever, the review must not wait for it.
		{model.StatusFailed, model.StatusPending, false},
		{model.StatusCancelled, model.StatusPending, false},
	}
	for _, c := range cases {
		t.Run(c.extract+"/"+c.terms, func(t *testing.T) {
			f := newFixture(t)
			f.configureJanitor()
			done := f.material("done.pdf", nil)
			f.done(model.ScopeMaterial, done.ID, model.StageMaterialExtract)
			f.done(model.ScopeMaterial, done.ID, model.StageMaterialTerms)
			// Course-wide and bound materials both count: the review covers
			// the whole glossary.
			l := f.lecture()
			m := f.material("week3.pdf", &l.ID)
			f.to(model.ScopeMaterial, m.ID, model.StageMaterialExtract, c.extract)
			f.to(model.ScopeMaterial, m.ID, model.StageMaterialTerms, c.terms)
			ok, w := f.deps(f.newReview())
			if c.wait && (ok || w != model.WaitMaterialTerms("week3.pdf")) {
				t.Fatalf("want material_terms:week3.pdf, got ok=%v %q", ok, w)
			}
			if !c.wait && (!ok || w != "") {
				t.Fatalf("want no wait, got ok=%v %q", ok, w)
			}
		})
	}
	// A course-wide material in progress also holds the review.
	f := newFixture(t)
	f.configureJanitor()
	f.material("all.pdf", nil)
	if ok, w := f.deps(f.newReview()); ok || w != model.WaitMaterialTerms("all.pdf") {
		t.Fatalf("course-wide material: ok=%v %q", ok, w)
	}
}

// ---------- chain, actions, rerun, retry, cancel, recovery ----------

func TestGlossaryReviewOutsideTheCourseChain(t *testing.T) {
	if got := StagesOf(model.ScopeCourse); len(got) != 1 || got[0] != model.StageGlossarySeed {
		t.Fatalf("StagesOf(course) = %v", got)
	}
	if u, d := Upstream(model.StageGlossaryReview), Downstream(model.StageGlossaryReview); u != "" || len(d) != 0 {
		t.Fatalf("glossary_review upstream %q downstream %v", u, d)
	}
	if d := Downstream(model.StageGlossarySeed); len(d) != 0 {
		t.Fatalf("glossary_seed downstream %v", d)
	}
	course := func(seed, review string) []model.Job {
		return []model.Job{
			{Scope: model.ScopeCourse, Stage: model.StageGlossarySeed, Status: seed},
			{Scope: model.ScopeCourse, Stage: model.StageGlossaryReview, Status: review},
		}
	}
	acts := StageActions(course(model.StatusDone, model.StatusRunning), false)
	if got := acts[model.StageGlossarySeed]; len(got) != 1 || got[0] != model.ActionRerun {
		t.Errorf("seed done while the review runs: %v", got)
	}
	if got := acts[model.StageGlossaryReview]; len(got) != 1 || got[0] != model.ActionCancel {
		t.Errorf("review running: %v", got)
	}
	acts = StageActions(course(model.StatusRunning, model.StatusDone), false)
	if got := acts[model.StageGlossaryReview]; len(got) != 1 || got[0] != model.ActionRerun {
		t.Errorf("review done while the seed runs: %v", got)
	}
	for status, want := range map[string]string{
		model.StatusStale: model.ActionRerun, model.StatusFailed: model.ActionRetry,
		model.StatusCancelled: model.ActionRetry, model.StatusQueued: model.ActionCancel,
	} {
		got := StageActions(course(model.StatusDone, status), false)[model.StageGlossaryReview]
		if len(got) != 1 || got[0] != want {
			t.Errorf("review %s: %v, want %s", status, got, want)
		}
	}
	if got := StageActions(course(model.StatusDone, model.StatusPending), false)[model.StageGlossaryReview]; len(got) != 0 {
		t.Errorf("review pending: %v", got)
	}
	// Display statuses: the review is never "outdated" by the seed.
	if ds := DisplayStatuses(course(model.StatusRunning, model.StatusDone)); ds[model.StageGlossaryReview] != model.StatusDone {
		t.Errorf("display = %v", ds)
	}
}

func TestGlossaryReviewRerunRetryCancel(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	f.configureJanitor()
	f.syllabus()
	f.done(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	f.newReview()
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusDone)
	seed := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	old := f.review()

	// rerun of the review: a new pending review; glossary_seed untouched.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeCourse, f.course.ID, model.StageGlossaryReview); err != nil {
		t.Fatal(err)
	}
	nj := f.review()
	if nj.ID == old.ID || nj.Status != model.StatusPending || !model.IsEmptySnapshot(nj.ConfigSnapshot) {
		t.Fatalf("rerun review: %+v", nj)
	}
	if s := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed); s.ID != seed.ID || s.Status != model.StatusDone {
		t.Fatalf("rerun review touched glossary_seed: %+v", s)
	}

	// rerun of glossary_seed does not touch the review.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeCourse, f.course.ID, model.StageGlossarySeed); err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.ID != nj.ID || r.Status != model.StatusPending {
		t.Fatalf("rerun seed touched the review: %+v", r)
	}

	// a running review refuses rerun (not done/stale) with a conflict.
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusRunning)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeCourse, f.course.ID, model.StageGlossaryReview); !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun running review: %v", err)
	}

	// retry of a failed review: same job, attempt+1, error cleared.
	failed, err := FailJob(ctx, f.st, f.review().ID, "boom", false)
	if err != nil || failed {
		t.Fatalf("fail: requeued=%v %v", failed, err)
	}
	id := f.review().ID
	if err := Retry(ctx, f.st, model.ScopeCourse, f.course.ID, model.StageGlossaryReview); err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.ID != id || r.Status != model.StatusQueued || r.Attempt != 1 || r.Error != nil {
		t.Fatalf("retry: %+v", r)
	}

	// cancel of a queued review.
	if err := CancelQueued(ctx, f.st, id); err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.Status != model.StatusCancelled {
		t.Fatalf("cancel: %+v", r)
	}
	// and retry after cancel.
	if err := RetryWith(ctx, f.st, f.dataDir, model.ScopeCourse, f.course.ID, model.StageGlossaryReview, nil); err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.Status != model.StatusQueued || r.Attempt != 2 {
		t.Fatalf("retry after cancel: %+v", r)
	}
}

// SPEC §4.7 applies to the review like to any job: running → failed
// "interrupted by restart" → queued while attempt < 2.
func TestRecoverGlossaryReview(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	f.configureJanitor()
	f.newReview()
	f.to(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusRunning)
	if err := Recover(ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	r := f.review()
	if r.Status != model.StatusQueued || r.Attempt != 1 || r.Error == nil || !IsInterrupted(r.Error) {
		t.Fatalf("after recovery: %+v", r)
	}
	// attempt 2 interrupted: stays failed.
	f.drive(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusRunning)
	if _, err := FailJob(ctx, f.st, r.ID, "x", true); err != nil { // → queued, attempt 2
		t.Fatal(err)
	}
	f.drive(model.ScopeCourse, f.course.ID, model.StageGlossaryReview, model.StatusRunning)
	if err := Recover(ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.Status != model.StatusFailed || r.Attempt != 2 || r.Error == nil || !IsInterrupted(r.Error) {
		t.Fatalf("attempt 2 after recovery: %+v", r)
	}
}

// ---------- StaleJanitor ----------

func TestStaleJanitorReason(t *testing.T) {
	three := 3
	at := func(stage string, v *int) model.Job {
		return model.Job{Stage: stage, Stats: model.JobStats{GlossaryVersion: v}}
	}
	cause := StaleCause{Kind: StaleJanitor}
	cases := []struct {
		j    model.Job
		want string
	}{
		{at(model.StageRepair, &three), rJanitor(ip(3), 4)},
		{at(model.StageTranslate, &three), rJanitor(ip(3), 4)},
		{at(model.StageTranslate, nil), rJanitor(nil, 4)},
		// same stages as the glossary cause
		{at(model.StageSummary, &three), rInput()},
		{at(model.StageASR, &three), rInput()},
	}
	for _, c := range cases {
		if got := StaleReason(cause, c.j, 4); got != c.want {
			t.Errorf("StaleReason(janitor, %s) = %q, want %q", c.j.Stage, got, c.want)
		}
	}
}

// A janitor run marks repair/translate stale with "术语表已整理（vN → vM）";
// like the glossary reason it follows later glossary changes, keeping its
// own prefix and only extending the version range (ruling 11).
func TestJanitorReasonFollowsLaterChanges(t *testing.T) {
	type step struct {
		kind string
		want string
	}
	for name, steps := range map[string][]step{
		"janitor then edit": {
			{StaleJanitor, rJanitor(ip(3), 4)},
			{StaleGlossary, rJanitor(ip(3), 5)},
		},
		"edit then janitor": {
			{StaleGlossary, rGlossary(ip(3), 4)},
			{StaleJanitor, rGlossary(ip(3), 5)},
		},
		"janitor twice": {
			{StaleJanitor, rJanitor(ip(3), 4)},
			{StaleJanitor, rJanitor(ip(3), 5)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := f.ctx
			term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "public", Zh: "公钥", Source: model.SourceMaterial, Enabled: true}
			if err := f.st.InsertTerm(ctx, term); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.st.BumpGlossaryVersion(ctx, f.course.ID) // v3
			}
			hashers := v11Hashers(f.st, f.course.ID)
			l := f.lecture()
			f.allDone(l.ID)
			f.recordHashes(hashers, l.ID)
			f.setGlossaryVersionStat(l.ID, model.StageRepair, 3)
			f.setGlossaryVersionStat(l.ID, model.StageTranslate, 3)
			zh := []string{"公开", "公共"}
			for i, s := range steps {
				term.Zh = zh[i]
				if err := f.st.UpdateTerm(ctx, term); err != nil {
					t.Fatal(err)
				}
				if _, err := f.st.BumpGlossaryVersion(ctx, f.course.ID); err != nil {
					t.Fatal(err)
				}
				changed, err := InvalidateCourseWith(ctx, f.st, hashers, f.course.ID, StaleCause{Kind: s.kind})
				if err != nil {
					t.Fatal(err)
				}
				if len(changed) != 2 {
					t.Errorf("step %d: %d jobs changed, want 2 (repair, translate)", i, len(changed))
				}
				for _, stage := range []string{model.StageRepair, model.StageTranslate} {
					if j := f.lj(l.ID, stage); j.Status != model.StatusStale || reasonOf(j) != s.want {
						t.Errorf("step %d %s: %s %q, want %q", i, stage, j.Status, reasonOf(j), s.want)
					}
				}
				if j := f.lj(l.ID, model.StageSummary); j.Status != model.StatusDone {
					t.Errorf("step %d: summary %s", i, j.Status)
				}
			}
		})
	}
}

// CheckStaleWith (one lecture) accepts the janitor cause too.
func TestCheckStaleWithJanitor(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "block", Zh: "分组", Source: model.SourceMaterial, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v1
	hashers := v11Hashers(f.st, f.course.ID)
	l := f.lecture()
	f.allDone(l.ID)
	f.recordHashes(hashers, l.ID)
	f.setGlossaryVersionStat(l.ID, model.StageRepair, 1)
	term.Enabled = false
	if err := f.st.UpdateTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	f.st.BumpGlossaryVersion(ctx, f.course.ID) // v2
	if _, err := CheckStaleWith(ctx, f.st, hashers, l.ID, StaleCause{Kind: StaleJanitor}); err != nil {
		t.Fatal(err)
	}
	if got := reasonOf(f.lj(l.ID, model.StageRepair)); got != rJanitor(ip(1), 2) {
		t.Fatalf("repair reason = %q", got)
	}
	if got := reasonOf(f.lj(l.ID, model.StageTranslate)); got != rJanitor(nil, 2) {
		t.Fatalf("translate reason = %q", got)
	}
}

// ---------- prompt versions in kept snapshots (lead, Gate 1) ----------

// v1Snaps are launch snapshots written before v2 (no prompt_version = v1).
func v1Snaps() map[string]string { return launchSnaps() }

func promptVersion(t *testing.T, raw string) int {
	t.Helper()
	s, ok := model.ParseLLMSnapshot(raw)
	if !ok {
		t.Fatalf("not an LLM snapshot: %q", raw)
	}
	return model.SnapshotPromptVersion(s)
}

// sameModel: provider, model and params unchanged (only the prompt version may differ).
func sameModel(a, b string) bool {
	sa, okA := model.ParseLLMSnapshot(a)
	sb, okB := model.ParseLLMSnapshot(b)
	return okA && okB && sa.ProviderID == sb.ProviderID && sa.ProviderName == sb.ProviderName && sa.Model == sb.Model &&
		model.MarshalSnapshot(sa.Params) == model.MarshalSnapshot(sb.Params)
}

// A rerun that keeps the snapshots ("沿用原模型") cannot keep the prompt:
// stages always run the current prompts, so every kept LLM snapshot of the
// new jobs records the current prompt version; provider, model and params
// stay. Snapshots given in NewSnapshots are used as given.
func TestRerunKeptSnapshotsGetCurrentPromptVersion(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := v1Snaps()
	l := f.launched(snaps)
	f.allDone(l.ID)
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		j := f.lj(l.ID, s)
		if v := promptVersion(t, j.ConfigSnapshot); v != model.PromptVersionOf(s) {
			t.Errorf("%s prompt version %d, want %d", s, v, model.PromptVersionOf(s))
		}
		if !sameModel(j.ConfigSnapshot, snaps[s]) {
			t.Errorf("%s: provider/model/params changed: %q → %q", s, snaps[s], j.ConfigSnapshot)
		}
	}
	for _, s := range []string{model.StagePrep, model.StageASR, model.StageExport} {
		if got := f.lj(l.ID, s).ConfigSnapshot; got != mergeSnaps(snaps)[s] {
			t.Errorf("%s snapshot changed: %q", s, got)
		}
	}

	// NewSnapshots ("使用当前设置") are written as given.
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		f.done(model.ScopeLecture, l.ID, s)
	}
	given := llmSnap("anthropic", "claude-x", 0.1)
	if err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageRepair,
		RerunOpts{NewSnapshots: map[string]string{model.StageRepair: given}}); err != nil {
		t.Fatal(err)
	}
	if got := f.lj(l.ID, model.StageRepair).ConfigSnapshot; got != given {
		t.Errorf("NewSnapshots not written as given: %q", got)
	}
}

// retry keeps the snapshot (A4.2) but the run uses the current prompt: the
// retried job's prompt_version becomes current, nothing else changes.
func TestRetryUpdatesPromptVersionOnly(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	snaps := v1Snaps()
	l := f.launched(snaps)
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	f.done(model.ScopeLecture, l.ID, model.StageASR)
	f.to(model.ScopeLecture, l.ID, model.StageSummary, model.StatusFailed)
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	j := f.lj(l.ID, model.StageSummary)
	if v := promptVersion(t, j.ConfigSnapshot); v != model.PromptVersionSummary {
		t.Fatalf("summary prompt version after retry = %d", v)
	}
	if !sameModel(j.ConfigSnapshot, snaps[model.StageSummary]) {
		t.Fatalf("retry changed provider/model/params: %q → %q", snaps[model.StageSummary], j.ConfigSnapshot)
	}
	// A non-LLM retried stage keeps its snapshot byte for byte.
	f2 := newFixture(t)
	l2 := f2.launched(snaps)
	f2.done(model.ScopeLecture, l2.ID, model.StagePrep)
	f2.to(model.ScopeLecture, l2.ID, model.StageASR, model.StatusCancelled)
	if err := Retry(f2.ctx, f2.st, model.ScopeLecture, l2.ID, model.StageASR); err != nil {
		t.Fatal(err)
	}
	if got := f2.lj(l2.ID, model.StageASR).ConfigSnapshot; got != snaps[model.StageASR] {
		t.Fatalf("asr snapshot changed: %q", got)
	}
}

// The v1.1 backfill writes the current settings into lectures started by
// v1: those ran with the v1 prompts, so the LLM snapshots get no
// prompt_version (version 1) even when the current settings carry 2.
func TestBackfillSnapshotsArePromptVersion1(t *testing.T) {
	f := newFixture(t)
	cur := map[string]string{model.StageASR: asrSnap("large-v2")}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		snap, _ := model.ParseLLMSnapshot(launchSnaps()[s])
		snap.PromptVersion = model.PromptVersionOf(s)
		cur[s] = model.MarshalSnapshot(snap)
	}
	l := f.lecture()
	if _, err := BackfillSnapshots(f.ctx, f.st, cur); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		got := f.lj(l.ID, s).ConfigSnapshot
		if v := promptVersion(t, got); v != 1 {
			t.Errorf("%s backfilled with prompt version %d: %q", s, v, got)
		}
		if !sameModel(got, cur[s]) {
			t.Errorf("%s: backfill changed provider/model/params: %q", s, got)
		}
		if got != launchSnaps()[s] {
			t.Errorf("%s: %q, want the v1.1 form %q", s, got, launchSnaps()[s])
		}
	}
	if got := f.lj(l.ID, model.StageASR).ConfigSnapshot; got != cur[model.StageASR] {
		t.Errorf("asr: %q", got)
	}
}
