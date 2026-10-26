// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// "只转写" (SPEC-v3 V4.2 step 4, 3.1.0; transcribe_only.go): with no
// provider for any lecture role a lecture launches, prep and asr run, the
// API stages wait with waiting_for no_provider and continue by themselves
// once the roles are configured. Fake stages only.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// noRoles: settings without the legacy import (no provider, no role).
func noRoles(t *testing.T) func(*Options) {
	return func(o *Options) {
		s := settings.New(o.Store, o.Config)
		if err := s.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.Settings = s
	}
}

// snapStage is an instant stage that records the config_snapshot of the
// job it ran for (sched.JobFrom: what the stage reads, A4.1).
func snapStage(name string, res ResourceKind, runs *atomic.Int32, seen *atomic.Value) *fakeStage {
	return &fakeStage{name: name, res: res, run: func(ctx context.Context, id string, p func(float64)) error {
		runs.Add(1)
		if j, ok := JobFrom(ctx); ok {
			seen.Store(j.ConfigSnapshot)
		}
		return nil
	}}
}

func (e *env) settle(scans int) {
	e.t.Helper()
	for range scans {
		e.s.Kick()
		time.Sleep(6 * time.Millisecond)
	}
}

func TestTranscribeOnlyLaunch(t *testing.T) {
	var sumRuns atomic.Int32
	var sumSeen atomic.Value
	e := newEnvOpts(t, nil, noRoles(t), allInstant(snapStage(model.StageSummary, ResourceAPI, &sumRuns, &sumSeen))...)
	if !e.s.TranscribeOnly() {
		t.Fatal("no lecture role configured: TranscribeOnly must be true")
	}
	d := e.draft()
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatalf("transcribe-only launch: %v", err)
	}
	if l, _ := e.st.GetLecture(e.ctx, d.ID); l.LaunchedAt == nil {
		t.Fatal("launched_at not set")
	}
	snaps := e.snaps(d.ID)
	if snaps[model.StageASR] != e.s.set.CurrentASR() || model.IsEmptySnapshot(snaps[model.StageASR]) {
		t.Fatalf("asr snapshot %q, want the current ASR settings %q", snaps[model.StageASR], e.s.set.CurrentASR())
	}
	for _, s := range []string{model.StagePrep, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		if snaps[s] != model.EmptySnapshot {
			t.Fatalf("%s snapshot %q, want empty (recorded when it starts)", s, snaps[s])
		}
	}

	e.start()
	e.waitStatus(d.ID, model.StageASR, model.StatusDone)
	e.waitFor("summary waits for a provider", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(d.ID, model.StageSummary)) == WaitNoProvider
	})
	e.settle(10)
	if j := e.lj(d.ID, model.StageSummary); j.Status != model.StatusPending || j.ConfigSnapshot != model.EmptySnapshot || sumRuns.Load() != 0 {
		t.Fatalf("summary %s snapshot %q runs %d", j.Status, j.ConfigSnapshot, sumRuns.Load())
	}
	if w := e.s.WaitingFor(e.lj(d.ID, model.StageRepair)); w != model.WaitUpstream(model.StageSummary) {
		t.Fatalf("repair waiting_for %q", w)
	}
	if s := e.lj(d.ID, model.StageExport).Status; s != model.StatusPending {
		t.Fatalf("export %s before translate", s)
	}

	// Some roles configured: still waiting, nothing recorded.
	p := e.provider("p1", 4)
	e.assign(p.ID, "model-a", model.StageSummary, model.StageRepair)
	e.settle(10)
	if j := e.lj(d.ID, model.StageSummary); j.Status != model.StatusPending || j.ConfigSnapshot != model.EmptySnapshot ||
		e.s.WaitingFor(j) != WaitNoProvider {
		t.Fatalf("partial roles: summary %s snapshot %q waiting %q", j.Status, j.ConfigSnapshot, e.s.WaitingFor(j))
	}

	// Every lecture role configured: the lecture continues by itself, with
	// the snapshots of the settings now (the format Launch writes).
	e.assign(p.ID, "model-a", model.StageTranslate)
	e.waitStatus(d.ID, model.StageExport, model.StatusDone)
	cur := e.current()
	for _, s := range llmStages {
		if got := e.lj(d.ID, s).ConfigSnapshot; got != cur[s] {
			t.Errorf("%s snapshot %q, want %q", s, got, cur[s])
		}
	}
	if got, _ := sumSeen.Load().(string); got != e.lj(d.ID, model.StageSummary).ConfigSnapshot || providerOfSnap(got) != p.ID {
		t.Errorf("summary ran with %q", got)
	}
	if sumRuns.Load() != 1 {
		t.Errorf("summary runs %d", sumRuns.Load())
	}
}

// A queued job of a deferred lecture (here: summary put in queued by
// hand, like a retry) waits in queued for a provider and gets its
// snapshot right before it starts.
func TestTranscribeOnlyQueuedJob(t *testing.T) {
	var runs atomic.Int32
	var seen atomic.Value
	e := newEnvOpts(t, nil, noRoles(t), allInstant(snapStage(model.StageSummary, ResourceAPI, &runs, &seen))...)
	d := e.draft()
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.stagesDone(d.ID, model.StagePrep, model.StageASR)
	e.drive(e.lj(d.ID, model.StageSummary).ID, db.JobFinish{}, model.StatusQueued)
	e.start()
	e.waitFor("queued summary waits for a provider", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(d.ID, model.StageSummary)) == WaitNoProvider
	})
	if runs.Load() != 0 {
		t.Fatal("summary ran without a provider")
	}
	e.assign(e.provider("p1", 4).ID, "model-a", llmStages...)
	e.waitStatus(d.ID, model.StageExport, model.StatusDone)
	got, _ := seen.Load().(string)
	if model.IsEmptySnapshot(got) || got != e.lj(d.ID, model.StageSummary).ConfigSnapshot {
		t.Fatalf("summary ran with %q", got)
	}
}

// Rerun and retry of a deferred lecture's asr ask nothing about the LLM
// stages (no recorded model) and keep their snapshots empty.
func TestTranscribeOnlyRerunAndRetry(t *testing.T) {
	var fail atomic.Bool
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(context.Context, string, func(float64)) error {
		if fail.Load() {
			return Permanent(errors.New("worker exited with code 1"))
		}
		return nil
	}}
	e := newEnvOpts(t, nil, noRoles(t), allInstant(asr)...)
	d := e.draft()
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(d.ID, model.StageASR, model.StatusDone)
	before := e.lj(d.ID, model.StageASR).ID

	for _, choice := range []string{"", model.ModelChoiceSnapshot, model.ModelChoiceCurrent} {
		fail.Store(true)
		if err := e.s.Rerun(e.ctx, model.ScopeLecture, d.ID, model.StageASR, choice); err != nil {
			t.Fatalf("rerun asr (%q): %v", choice, err)
		}
		e.waitStatus(d.ID, model.StageASR, model.StatusFailed)
		if e.lj(d.ID, model.StageASR).ID == before {
			t.Fatal("rerun made no new asr job")
		}
		fail.Store(false)
		if err := e.s.Retry(e.ctx, model.ScopeLecture, d.ID, model.StageASR, choice); err != nil {
			t.Fatalf("retry asr (%q): %v", choice, err)
		}
		e.waitStatus(d.ID, model.StageASR, model.StatusDone)
		for _, s := range llmStages {
			if j := e.lj(d.ID, s); j.ConfigSnapshot != model.EmptySnapshot || j.Status != model.StatusPending {
				t.Fatalf("after %q: %s %s snapshot %q", choice, s, j.Status, j.ConfigSnapshot)
			}
		}
		e.waitFor("summary waits for a provider again", 5*time.Second, func() bool {
			return e.s.WaitingFor(e.lj(d.ID, model.StageSummary)) == WaitNoProvider
		})
		before = e.lj(d.ID, model.StageASR).ID
	}
}

// Without a term extraction role, glossary_seed and material_terms wait
// for a provider and asr does not wait for them; once the role is
// configured they run.
func TestTranscribeOnlyTermJobs(t *testing.T) {
	var seedRuns, termRuns atomic.Int32
	var seedSeen, termSeen atomic.Value
	stages := append(allInstant(),
		snapStage(model.StageGlossarySeed, ResourceAPI, &seedRuns, &seedSeen),
		instant(model.StageMaterialExtract, ResourceCPU),
		snapStage(model.StageMaterialTerms, ResourceAPI, &termRuns, &termSeen))
	e := newEnvOpts(t, nil, noRoles(t), stages...)
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.md", Path: "p"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	d := e.draft()
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: &d.ID, Filename: "week1.pdf", Path: "p"}
	e.must(e.st.CreateMaterial(e.ctx, m))
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(d.ID, model.StageASR, model.StatusDone)
	e.waitStatus(d.ID, model.StagePrep, model.StatusDone)
	e.waitFor("material_extract", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialExtract).Status == model.StatusDone
	})
	e.settle(5)
	for _, j := range []model.Job{e.job(model.ScopeCourse, e.course.ID, model.StageGlossarySeed), e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)} {
		if j.Status != model.StatusPending || e.s.WaitingFor(j) != WaitNoProvider {
			t.Fatalf("%s: %s waiting %q", j.Stage, j.Status, e.s.WaitingFor(j))
		}
	}
	if seedRuns.Load()+termRuns.Load() != 0 {
		t.Fatal("term extraction ran without a provider")
	}

	// The glossary role configured: both run (the role's current setting).
	e.assign(e.provider("p1", 4).ID, "model-g", model.StageGlossarySeed)
	e.waitFor("term jobs done", 5*time.Second, func() bool {
		return e.job(model.ScopeCourse, e.course.ID, model.StageGlossarySeed).Status == model.StatusDone &&
			e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusDone
	})
	if seedRuns.Load() != 1 || termRuns.Load() != 1 {
		t.Fatalf("runs %d %d", seedRuns.Load(), termRuns.Load())
	}
}

// With the term extraction role configured asr still waits for the term
// jobs (state.Deps unchanged), also when the lecture roles are not.
func TestTermJobsStillHoldASRWithGlossaryRole(t *testing.T) {
	gs := newGate()
	stages := append(allInstant(), &fakeStage{name: model.StageGlossarySeed, res: ResourceAPI, run: gs.run})
	e := newEnvOpts(t, nil, noRoles(t), stages...)
	e.assign(e.provider("p1", 4).ID, "model-g", model.StageGlossarySeed)
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.md", Path: "p"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	d := e.draft()
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(d.ID, model.StagePrep, model.StatusDone)
	e.waitFor("asr waits for glossary_seed", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(d.ID, model.StageASR)) == model.WaitGlossarySeed
	})
	close(gs.release)
	e.waitStatus(d.ID, model.StageASR, model.StatusDone)
}

// A lecture with recorded LLM snapshots is not deferred: an empty
// snapshot of only one of its LLM stages is left alone (lead ruling 4:
// that stage fails, see TestRunningLeavesOtherSnapshotsAlone).
func TestDeferredNeedsAllThreeEmpty(t *testing.T) {
	jobs := func(snaps ...string) []model.Job {
		var out []model.Job
		for i, s := range llmStages {
			out = append(out, model.Job{Stage: s, ConfigSnapshot: snaps[i]})
		}
		return append(out, model.Job{Stage: model.StageASR, ConfigSnapshot: `{"model":"m"}`}, model.Job{Stage: model.StageTitle, ConfigSnapshot: `{"prompt_version":1}`})
	}
	if !deferredLLM(jobs("{}", "", "{}")) {
		t.Error("three empty: deferred")
	}
	if deferredLLM(jobs("{}", `{"provider_id":"p","model":"m"}`, "{}")) {
		t.Error("one recorded: not deferred")
	}
	if deferredLLM(jobs("{}", "{}", "{}")[1:]) {
		t.Error("a missing LLM job: not deferred")
	}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}
