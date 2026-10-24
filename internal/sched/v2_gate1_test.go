// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Scheduler tests of the lead's Gate 1 rulings for core-builder: the G2
// fallback after a glossary_review that did not complete, and the prompt
// version of kept snapshots. Fake stages only.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// ---------- fallback after a review that did not complete ----------

// term inserts an unlocked, enabled enforce term of the course.
func (e *env) term(en string) model.Term {
	e.t.Helper()
	t := model.Term{ID: db.NewID(), CourseID: e.course.ID, En: en, Zh: "译" + en, Source: model.SourceMaterial, Enabled: true}
	if err := e.st.InsertTerm(e.ctx, t); err != nil {
		e.t.Fatal(err)
	}
	return t
}

func (e *env) tier(id string) string {
	e.t.Helper()
	t, err := e.st.GetTerm(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return t.Tier
}

// drive moves a job along consecutive transitions from its current status.
func (e *env) drive(jobID string, f db.JobFinish, path ...string) {
	e.t.Helper()
	j, err := e.st.GetJob(e.ctx, jobID)
	if err != nil {
		e.t.Fatal(err)
	}
	from := j.Status
	for _, to := range path {
		if err := state.TransitionJob(e.ctx, e.st, jobID, from, to, f); err != nil {
			e.t.Fatalf("%s→%s: %v", from, to, err)
		}
		from = to
	}
}

func (e *env) stagesDone(lectureID string, stages ...string) {
	e.t.Helper()
	for _, s := range stages {
		e.drive(e.lj(lectureID, s).ID, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusDone)
	}
}

// tierProbe is a fake asr stage that records the tier of one term when it
// runs: the glossary asr ran with.
type tierProbe struct {
	id    atomic.Value
	seen  atomic.Value
	stage *fakeStage
}

func (p *tierProbe) tierAtASR() string { s, _ := p.seen.Load().(string); return s }

// registerReviewPipeline registers prep, an asr probe and the review stage.
func (e *env) registerReviewPipeline(rs *reviewStage) *tierProbe {
	p := &tierProbe{}
	p.stage = &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, _ string, _ func(float64)) error {
		id, _ := p.id.Load().(string)
		t, err := e.st.GetTerm(ctx, id)
		if err != nil {
			return err
		}
		p.seen.Store(t.Tier)
		return nil
	}}
	e.s.Register(instant(model.StagePrep, ResourceCPU))
	e.s.Register(p.stage)
	e.s.Register(rs)
	return p
}

func latestReport(t *testing.T, e *env) model.JanitorReport {
	t.Helper()
	rep, err := glossary.LastRun(e.ctx, e.st, e.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// incomplete: rep is the fallback report of a review that ended with
// status (coded note, v2 P14.3).
func incomplete(rep model.JanitorReport, status string) bool {
	n := rep.NoteInfo
	return n != nil && n.Code == CodeNoteReviewIncomplete && n.Params["status"] == status && rep.Note != ""
}

// Retryable failures until no automatic retry is left: the review fails for
// good, the G2 rule sorts the glossary before asr starts, asr is not held,
// and the report says why.
func TestReviewFallbackAfterRetriesExhausted(t *testing.T) {
	rs := newReviewStage()
	e := newEnvOpts(t, nil, func(o *Options) { o.RetryBackoff = []time.Duration{5 * time.Millisecond} })
	probe := e.registerReviewPipeline(rs)
	e.configureJanitor()
	pub := e.term("public")
	vig := e.term("Vigenère cipher")
	probe.id.Store(pub.ID)
	l := e.lecture()
	e.prepDone(l)
	e.enqueueReview()
	e.start()
	for range model.MaxAutoRetries + 1 {
		rs.waitStarted(t)
		rs.finish(t, errors.New("HTTP 503"))
	}
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if j := e.review(); j.Status != model.StatusFailed || j.Attempt != model.MaxAutoRetries {
		t.Fatalf("review: %+v", j)
	}
	if got := probe.tierAtASR(); got != model.TierReference {
		t.Fatalf("asr ran with public in tier %q, want reference (fallback before asr)", got)
	}
	if e.tier(pub.ID) != model.TierReference || e.tier(vig.ID) != model.TierEnforce {
		t.Fatalf("tiers: public %s, Vigenère cipher %s", e.tier(pub.ID), e.tier(vig.ID))
	}
	rep := latestReport(t, e)
	if !rep.Fallback || !incomplete(rep, model.StatusFailed) || len(rep.Items) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if !e.hasChanged("terms", e.course.ID) {
		t.Fatal("no terms event")
	}
	if n := rs.runs.Load(); n != model.MaxAutoRetries+1 {
		t.Fatalf("runs = %d", n)
	}
}

// A user cancel (running, then queued in a retry backoff) also leaves the
// glossary sorted by the G2 rule; repair of a done lecture becomes stale
// with the janitor reason.
func TestReviewFallbackAfterUserCancel(t *testing.T) {
	rs := newReviewStage()
	e := newEnvOpts(t, nil, func(o *Options) { o.RetryBackoff = []time.Duration{time.Hour} })
	probe := e.registerReviewPipeline(rs)
	// repair's input contains the tiers (like the real hash, ruling 9).
	tiers := func(ctx context.Context, _ string) (string, error) {
		ts, err := e.st.ListTerms(ctx, e.course.ID)
		var b strings.Builder
		for _, t := range ts {
			b.WriteString(t.En + "=" + t.Tier + ";")
		}
		return b.String(), err
	}
	e.s.Register(&fakeStage{name: model.StageRepair, res: ResourceAPI, hash: tiers})
	e.configureJanitor()
	pub := e.term("public")
	probe.id.Store(pub.ID)
	done := e.lecture()
	e.stagesDone(done.ID, model.LectureStages...)
	h, _ := tiers(e.ctx, done.ID)
	if err := e.st.SetJobInputHash(e.ctx, e.lj(done.ID, model.StageRepair).ID, h); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	e.prepDone(l)
	e.enqueueReview()
	e.start()

	// running → cancelled
	rs.waitStarted(t)
	if err := e.s.Cancel(e.review().ID); err != nil {
		t.Fatal(err)
	}
	if e.tier(pub.ID) != model.TierReference {
		t.Fatalf("public after cancel: %s", e.tier(pub.ID))
	}
	if rep := latestReport(t, e); !rep.Fallback || !incomplete(rep, model.StatusCancelled) {
		t.Fatalf("report: %+v", rep)
	}
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if got := probe.tierAtASR(); got != model.TierReference {
		t.Fatalf("asr ran with public in tier %q", got)
	}
	if j := e.lj(done.ID, model.StageRepair); j.Status != model.StatusStale || j.StaleReason == nil ||
		state.StaleReasonOf(j).Code != model.StaleCodeJanitor { // P14.3 JSON (was the text "术语表已整理…")
		t.Fatalf("done lecture's repair: %s %v", j.Status, j.StaleReason)
	}

	// queued (retry backoff) → cancelled
	world := e.term("world")
	if err := e.s.DoAction(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	rs.waitStarted(t)
	rs.finish(t, errors.New("HTTP 503"))
	j := e.waitReview("in backoff", func(j model.Job) bool { return j.Status == model.StatusQueued })
	if e.tier(world.ID) != model.TierEnforce {
		t.Fatal("the fallback ran during the backoff")
	}
	if err := e.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if e.tier(world.ID) != model.TierReference {
		t.Fatalf("world after cancelling the queued review: %s", e.tier(world.ID))
	}
}

// A stage that already ran the fallback for a permanent error (appendix D2)
// keeps its report, even one without changes (whose note carries the
// reason): the scheduler does not run the fallback again.
func TestReviewFallbackSkippedWhenStageFellBack(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.term("Vigenère cipher") // nothing for the G2 rule to change
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	if _, _, err := glossary.RunFallbackNote(e.ctx, e.st, e.course.ID, time.Now(), "stage note: HTTP 401", nil); err != nil {
		t.Fatal(err)
	}
	rs.finish(t, Permanent(errors.New("HTTP 401")))
	e.waitReview("failed", func(j model.Job) bool { return j.Status == model.StatusFailed })
	time.Sleep(30 * time.Millisecond)
	if rep := latestReport(t, e); rep.Note != "stage note: HTTP 401" {
		t.Fatalf("the stage's report was replaced: %+v", rep)
	}
}

// A review failed for good by the startup recovery gets its fallback when
// the scheduler starts.
func TestReviewFallbackAtStartAfterRecovery(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	pub := e.term("public")
	e.enqueueReview()
	interrupted := model.ErrInterruptedByRestart
	e.drive(e.review().ID, db.JobFinish{Error: &interrupted}, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	e.start()
	if e.tier(pub.ID) != model.TierReference {
		t.Fatalf("public after start: %s", e.tier(pub.ID))
	}
	if rep := latestReport(t, e); !rep.Fallback || !incomplete(rep, model.StatusFailed) {
		t.Fatalf("report: %+v", rep)
	}
	if !e.hasChanged("terms", e.course.ID) {
		t.Fatal("no terms event")
	}
}

// A review cancelled while queued whose fallback did not run (the process
// stopped in between) gets it at the next start, and only once: a later
// start leaves the user's later change alone.
func TestReviewFallbackAtStartOnce(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	pub := e.term("public")
	e.enqueueReview()
	cancelled := e.review()
	e.drive(cancelled.ID, db.JobFinish{}, model.StatusQueued, model.StatusCancelled)
	e.start()
	if e.tier(pub.ID) != model.TierReference {
		t.Fatalf("public after start: %s", e.tier(pub.ID))
	}
	if rep := latestReport(t, e); !incomplete(rep, model.StatusCancelled) {
		t.Fatalf("report: %+v", rep)
	}
	e.s.Stop()
	p, err := e.st.GetTerm(e.ctx, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.Tier = model.TierEnforce
	if err := e.st.UpdateTerm(e.ctx, p); err != nil {
		t.Fatal(err)
	}
	e.s = e.newScheduler(rs)
	e.start()
	time.Sleep(30 * time.Millisecond)
	if e.tier(pub.ID) != model.TierEnforce {
		t.Fatal("the fallback ran again on the second start")
	}
	if j := e.review(); j.ID != cancelled.ID || j.Status != model.StatusCancelled {
		t.Fatalf("review: %+v", j)
	}
}

// ---------- prompt versions of kept snapshots ----------

// v1Lecture launches a lecture with the current settings as v1 wrote them
// (LLM snapshots without prompt_version).
func (e *env) v1Lecture() model.Lecture {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: "V1", MediaPath: "m.mp4"}
	if err := e.st.CreateLecture(e.ctx, l); err != nil {
		e.t.Fatal(err)
	}
	snaps := e.current()
	for _, s := range llmStages {
		snap, _ := model.ParseLLMSnapshot(snaps[s])
		snap.PromptVersion = 0
		snaps[s] = model.MarshalSnapshot(snap)
	}
	if err := state.Launch(e.ctx, e.st, l.ID, snaps); err != nil {
		e.t.Fatal(err)
	}
	return l
}

// "沿用原模型" after "提示词 v1 → v2": the kept snapshots record the current
// prompt version, so the next rerun does not ask again.
func TestRerunSnapshotChoiceRecordsCurrentPromptVersion(t *testing.T) {
	e := newEnv(t, nil)
	l := e.v1Lecture()
	e.stagesDone(l.ID, model.LectureStages...)
	var nm *state.NeedsModelChoiceError
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); !errors.As(err, &nm) || len(nm.Diffs) != 3 {
		t.Fatalf("v1 snapshots: want needs_model_choice for 3 stages, got %v", err)
	}
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	for _, s := range llmStages {
		snap, _ := model.ParseLLMSnapshot(e.lj(l.ID, s).ConfigSnapshot)
		if model.SnapshotPromptVersion(snap) != model.PromptVersionOf(s) {
			t.Fatalf("%s prompt version %d", s, model.SnapshotPromptVersion(snap))
		}
	}
	e.stagesDone(l.ID, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport)
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatalf("second rerun asked again: %v", err)
	}
}

// retry keeps provider, model and params but records the current prompt version.
func TestRetryRecordsCurrentPromptVersion(t *testing.T) {
	e := newEnv(t, nil)
	l := e.v1Lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR)
	before := e.lj(l.ID, model.StageSummary)
	e.drive(before.ID, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatal(err)
	}
	a, _ := model.ParseLLMSnapshot(before.ConfigSnapshot)
	b, _ := model.ParseLLMSnapshot(e.lj(l.ID, model.StageSummary).ConfigSnapshot)
	if model.SnapshotPromptVersion(b) != model.PromptVersionSummary {
		t.Fatalf("prompt version after retry = %d", model.SnapshotPromptVersion(b))
	}
	b.PromptVersion = a.PromptVersion
	if model.MarshalSnapshot(a) != model.MarshalSnapshot(b) {
		t.Fatalf("retry changed more than the prompt version: %+v → %+v", a, b)
	}
}
