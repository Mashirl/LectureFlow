// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---------- fixtures ----------

type fixture struct {
	t       *testing.T
	ctx     context.Context
	st      db.Store
	dataDir string
	course  model.Course
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, ctx: context.Background(), st: st, dataDir: filepath.Join(dir, "data")}
	f.course = model.Course{ID: db.NewID(), Name: "CS"}
	if err := st.CreateCourse(f.ctx, f.course); err != nil {
		t.Fatal(err)
	}
	return f
}

// lecture creates a launched lecture (v1.1 A3: a v1 lecture was started on
// upload) whose media file exists in the lecture dir.
func (f *fixture) lecture() model.Lecture {
	f.t.Helper()
	now := db.Now()
	return f.newLecture(&now)
}

// draft creates a draft lecture (launched_at IS NULL, v1.1 A3.1).
func (f *fixture) draft() model.Lecture {
	f.t.Helper()
	return f.newLecture(nil)
}

func (f *fixture) newLecture(launchedAt *string) model.Lecture {
	f.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: f.course.ID, Title: "L", MediaPath: "media.mp4", LaunchedAt: launchedAt}
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "media.mp4"), []byte("m"), 0o644)
	if err := f.st.CreateLecture(f.ctx, l); err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fixture) job(scope, scopeID, stage string) model.Job {
	f.t.Helper()
	j, err := f.st.GetCurrentJob(f.ctx, scope, scopeID, stage)
	if err != nil {
		f.t.Fatalf("current job %s/%s: %v", scopeID, stage, err)
	}
	return j
}

func (f *fixture) lj(lectureID, stage string) model.Job {
	return f.job(model.ScopeLecture, lectureID, stage)
}

// drive moves the current job through the given statuses with Transition.
func (f *fixture) drive(scope, scopeID, stage string, path ...string) {
	f.t.Helper()
	from := f.job(scope, scopeID, stage).Status
	for _, to := range path {
		if err := Transition(f.ctx, f.st, scope, scopeID, stage, from, to, db.JobFinish{}); err != nil {
			f.t.Fatalf("%s %s→%s: %v", stage, from, to, err)
		}
		from = to
	}
}

func (f *fixture) done(scope, scopeID, stage string) {
	f.drive(scope, scopeID, stage, model.StatusQueued, model.StatusRunning, model.StatusDone)
}

func (f *fixture) setHash(scope, scopeID, stage, h string) {
	f.t.Helper()
	if err := f.st.SetJobInputHash(f.ctx, f.job(scope, scopeID, stage).ID, h); err != nil {
		f.t.Fatal(err)
	}
}

func sp(s string) *string { return &s }

// fullLecture is a lecture with all six stages done, sentences, patches,
// a summary, translations, human_text on sentence 1, audio.wav and exports.
func (f *fixture) fullLecture() model.Lecture {
	f.t.Helper()
	l := f.lecture()
	for _, s := range model.LectureStages {
		f.done(model.ScopeLecture, l.ID, s)
	}
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "the cash is warm", RepairedText: "the cash is warm"},
		{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "a miss penalty", RepairedText: "a miss penalty"},
		{Idx: 2, StartMS: 2000, EndMS: 3000, ASRText: "end", RepairedText: "end"},
	}
	ctx := f.ctx
	must := func(err error) {
		f.t.Helper()
		if err != nil {
			f.t.Fatal(err)
		}
	}
	must(f.st.InsertSentences(ctx, l.ID, ss))
	must(f.st.SetRepairedText(ctx, l.ID, 0, "the cache is warm"))
	must(f.st.SetHumanText(ctx, l.ID, 1, sp("a miss penalty!")))
	must(f.st.RecomputeText(ctx, l.ID, nil))
	must(f.st.InsertPatches(ctx, []model.Patch{
		{LectureID: l.ID, SentenceIdx: 0, Before: "the cash is warm", After: "the cache is warm", Class: model.ClassAuto, Review: model.ReviewNone},
		{LectureID: l.ID, SentenceIdx: 1, Before: "a miss penalty", After: "a mess penalty", Class: model.ClassDecide, Review: model.ReviewRejected},
	}))
	must(f.st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "sum", InputHash: "h"}))
	for i := 0; i < 3; i++ {
		must(f.st.SetTranslation(ctx, l.ID, i, db.SentenceTr{Zh: sp("中文"), ZhStatus: model.ZhOK, ZhHash: sp("zh")}))
	}
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	os.WriteFile(filepath.Join(dir, model.AudioFile), []byte("wav"), 0o644)
	os.MkdirAll(filepath.Join(dir, model.ExportDirName), 0o755)
	os.WriteFile(filepath.Join(dir, model.ExportDirName, "L.en.txt"), []byte("x"), 0o644)
	return l
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---------- step2 required tests ----------

func TestIllegalTransitionsRejected(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	all := []string{model.StatusPending, model.StatusQueued, model.StatusRunning, model.StatusDone,
		model.StatusFailed, model.StatusCancelled, model.StatusStale}
	legal := map[string]bool{
		"pending>queued": true, "queued>running": true, "queued>cancelled": true,
		"running>done": true, "running>failed": true, "running>cancelled": true,
		"failed>queued": true, "cancelled>queued": true, "done>stale": true,
	}
	for _, from := range all {
		for _, to := range all {
			if Legal(from, to) != legal[from+">"+to] {
				t.Errorf("Legal(%s,%s) = %v", from, to, !legal[from+">"+to])
			}
			if legal[from+">"+to] {
				continue
			}
			err := Transition(f.ctx, f.st, model.ScopeLecture, l.ID, model.StagePrep, from, to, db.JobFinish{})
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("%s→%s: want ErrIllegalTransition, got %v", from, to, err)
			}
		}
	}
	if j := f.lj(l.ID, model.StagePrep); j.Status != model.StatusPending {
		t.Fatalf("status changed by illegal transitions: %s", j.Status)
	}
	// A legal pair whose from does not match the row affects 0 rows → error.
	err := Transition(f.ctx, f.st, model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, db.JobFinish{})
	if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("0-row CAS: %v", err)
	}
	// done|stale → pending is not a transition; it goes through Rerun.
	if Legal(model.StatusDone, model.StatusPending) || Legal(model.StatusStale, model.StatusPending) {
		t.Fatal("done/stale → pending must not be a Transition")
	}
}

func TestConcurrentQueuedToRunningOnlyOneWins(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued)
	id := f.lj(l.ID, model.StagePrep).ID
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = TransitionJob(f.ctx, f.st, id, model.StatusQueued, model.StatusRunning, db.JobFinish{})
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d goroutines won queued→running, want 1", ok)
	}
	j := f.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusRunning || j.StartedAt == nil {
		t.Fatalf("job = %+v", j)
	}
}

func TestRerunMakesAllDownstreamPending(t *testing.T) {
	f := newFixture(t)
	l := f.fullLecture()
	old := map[string]model.Job{}
	for _, s := range model.LectureStages {
		old[s] = f.lj(l.ID, s)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageSummary, model.StatusStale)
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.LectureStages {
		j := f.lj(l.ID, s)
		switch s {
		case model.StagePrep, model.StageASR:
			if j.ID != old[s].ID || j.Status != model.StatusDone {
				t.Errorf("%s: upstream must be untouched, got %+v", s, j)
			}
		default:
			if j.ID == old[s].ID || j.Status != model.StatusPending || j.Attempt != 0 || !j.IsCurrent {
				t.Errorf("%s: want new pending job, got %+v", s, j)
			}
			prev, _ := f.st.GetJob(f.ctx, old[s].ID)
			if prev.IsCurrent {
				t.Errorf("%s: old job still current", s)
			}
		}
	}
	// Running jobs cannot be rerun; neither can pending ones.
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageRepair); !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun pending: %v", err)
	}
}

// fakeHashers are arbitrary-input fake hashers that exercise the state
// mechanism (done → stale only for stages whose hash input changed): the
// "repair" hasher covers the glossary (en + zh), the "asr" hasher only the
// enabled en terms. They do NOT model the real input hashes: since v1.1 A1
// the real asr hash ignores the glossary entirely; that real behaviour is
// covered by TestRealHashersInvalidation in internal/stages.
func fakeHashers(st db.Store, courseID string) map[string]Hasher {
	sum := func(parts ...string) string {
		h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return hex.EncodeToString(h[:])
	}
	terms := func(ctx context.Context, withZh bool) string {
		ts, _ := st.ListTerms(ctx, courseID)
		var parts []string
		for _, t := range ts {
			if !t.Enabled {
				continue
			}
			if withZh {
				parts = append(parts, t.En+"=>"+t.Zh)
			} else {
				parts = append(parts, t.En)
			}
		}
		return strings.Join(parts, ", ")
	}
	return map[string]Hasher{
		model.StageASR: HasherFunc(func(ctx context.Context, id string) (string, error) {
			return sum("wav", terms(ctx, false)), nil
		}),
		model.StageRepair: HasherFunc(func(ctx context.Context, id string) (string, error) {
			return sum("asr", "summary", terms(ctx, true)), nil
		}),
		model.StageSummary: HasherFunc(func(ctx context.Context, id string) (string, error) { return "", nil }),
	}
}

func TestGlossaryChangeMarksStale(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	term := model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Source: model.SourceManual, Enabled: true}
	if err := f.st.InsertTerm(ctx, term); err != nil {
		t.Fatal(err)
	}
	hashers := fakeHashers(f.st, f.course.ID)
	l := f.lecture()
	for _, s := range model.LectureStages {
		f.done(model.ScopeLecture, l.ID, s)
	}
	for _, s := range []string{model.StageASR, model.StageRepair} {
		h, _ := hashers[s].InputHash(ctx, l.ID)
		f.setHash(model.ScopeLecture, l.ID, s, h)
	}
	f.setHash(model.ScopeLecture, l.ID, model.StageSummary, "recorded-but-no-hash-now")

	// Nothing changed → nothing stale.
	changed, err := InvalidateCourse(ctx, f.st, hashers, f.course.ID)
	if err != nil || len(changed) != 0 {
		t.Fatalf("no-op invalidation: %v %v", changed, err)
	}

	// 1) Editing zh changes the "repair" hasher's input but not the fake
	//    "asr" hasher's (en terms only).
	term.Zh = "缓存"
	f.st.UpdateTerm(ctx, term)
	changed, err = InvalidateCourse(ctx, f.st, hashers, f.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Stage != model.StageRepair || changed[0].Status != model.StatusStale {
		t.Fatalf("changed = %+v", changed)
	}
	if s := f.lj(l.ID, model.StageASR).Status; s != model.StatusDone {
		t.Fatalf("asr must stay done when its fake hasher's input is unchanged, got %s", s)
	}
	if s := f.lj(l.ID, model.StageSummary).Status; s != model.StatusDone {
		t.Fatalf("stage without hash must be skipped, got %s", s)
	}
	// Not re-run automatically: still stale, nothing pending.
	for _, s := range []string{model.StageTranslate, model.StageExport} {
		if st := f.lj(l.ID, s).Status; st != model.StatusDone {
			t.Fatalf("%s = %s, stale must not cascade", s, st)
		}
	}

	// 2) Adding a term changes the fake "asr" hasher's input → that job
	//    becomes stale too (a property of this fake, not of the real A1 hash).
	f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "TLB", Source: model.SourceManual, Enabled: true})
	changed, err = CheckStale(ctx, f.st, hashers, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Stage != model.StageASR {
		t.Fatalf("changed = %+v", changed)
	}
	if s := f.lj(l.ID, model.StageASR).Status; s != model.StatusStale {
		t.Fatalf("asr = %s", s)
	}

	// Rerun from stale works and resets downstream.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageRepair); err != nil {
		t.Fatal(err)
	}
	if s := f.lj(l.ID, model.StageExport).Status; s != model.StatusPending {
		t.Fatalf("export = %s", s)
	}
}

// ---------- rerun cleanup table (SPEC §4.6) ----------

func TestRerunCleanupTable(t *testing.T) {
	type want struct {
		sentences, patches, summary, audio bool
		repairedReset, zhCleared           bool
		human                              bool // human_text of sentence 1 kept
	}
	cases := map[string]want{
		model.StagePrep:      {sentences: false, patches: false, summary: false, audio: false},
		model.StageASR:       {sentences: false, patches: false, summary: false, audio: true},
		model.StageSummary:   {sentences: true, patches: false, summary: false, audio: true, repairedReset: true, zhCleared: true, human: true},
		model.StageRepair:    {sentences: true, patches: false, summary: true, audio: true, repairedReset: true, zhCleared: true, human: true},
		model.StageTranslate: {sentences: true, patches: true, summary: true, audio: true, zhCleared: true, human: true},
		model.StageExport:    {sentences: true, patches: true, summary: true, audio: true, human: true},
	}
	for stage, w := range cases {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			l := f.fullLecture()
			ctx := f.ctx
			if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, stage); err != nil {
				t.Fatal(err)
			}
			n, _ := f.st.CountSentences(ctx, l.ID)
			if (n > 0) != w.sentences {
				t.Errorf("sentences present = %v", n > 0)
			}
			np, _ := f.st.CountPatches(ctx, l.ID, "")
			if (np > 0) != w.patches {
				t.Errorf("patches present = %v", np > 0)
			}
			_, err := f.st.GetSummary(ctx, l.ID)
			if (err == nil) != w.summary {
				t.Errorf("summary present = %v", err == nil)
			}
			dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
			if exists(filepath.Join(dir, model.AudioFile)) != w.audio {
				t.Errorf("audio.wav present = %v", !w.audio)
			}
			if exists(filepath.Join(dir, model.ExportDirName)) {
				t.Error("export dir must be removed by every lecture rerun")
			}
			if !exists(filepath.Join(dir, "media.mp4")) {
				t.Error("original media must be kept")
			}
			if w.sentences {
				s0, _ := f.st.GetSentence(ctx, l.ID, 0)
				s1, _ := f.st.GetSentence(ctx, l.ID, 1)
				if w.repairedReset != (s0.RepairedText == s0.ASRText) {
					t.Errorf("repaired_text = %q", s0.RepairedText)
				}
				if s0.Text != s0.RepairedText {
					t.Errorf("text not recomputed: %q vs %q", s0.Text, s0.RepairedText)
				}
				cleared := s0.Zh == nil && s0.ZhStatus == model.ZhNone && s0.ZhHash == nil
				if cleared != w.zhCleared {
					t.Errorf("zh cleared = %v", cleared)
				}
				if (s1.HumanText != nil) != w.human || (w.human && s1.Text != "a miss penalty!") {
					t.Errorf("human_text rule violated: %+v", s1)
				}
			}
			// The stage and all downstream are new pending jobs.
			pos := indexOf(model.LectureStages, stage)
			for i, s := range model.LectureStages {
				st := f.lj(l.ID, s).Status
				if i >= pos && st != model.StatusPending {
					t.Errorf("%s = %s, want pending", s, st)
				}
				if i < pos && st != model.StatusDone {
					t.Errorf("%s = %s, want done", s, st)
				}
			}
		})
	}
}

func TestRerunGuards(t *testing.T) {
	f := newFixture(t)
	l := f.fullLecture()
	// Downstream running → conflict, nothing changed.
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning)
	tr := f.lj(l.ID, model.StageTranslate)
	err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageTranslate)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rerun with running downstream: %v", err)
	}
	s0, _ := f.st.GetSentence(f.ctx, l.ID, 0)
	if s0.Zh == nil {
		t.Fatal("rejected rerun must not clean up")
	}
	if j := f.lj(l.ID, model.StageTranslate); j.ID != tr.ID || j.Status != model.StatusDone {
		t.Fatalf("rejected rerun changed translate: %+v", j)
	}
	if j := f.lj(l.ID, model.StageExport); j.Status != model.StatusRunning {
		t.Fatalf("rejected rerun changed the running export: %+v", j)
	}
	// Prep rerun refuses when the original media is gone (SPEC §17.5).
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusDone)
	os.Remove(filepath.Join(model.LectureDir(f.dataDir, l.CourseID, l.ID), "media.mp4"))
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StagePrep); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("prep rerun without media: %v", err)
	}
	if n, _ := f.st.CountSentences(f.ctx, l.ID); n != 3 {
		t.Fatal("refused prep rerun must not delete sentences")
	}
	// Wrong scope / unknown stage.
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeCourse, l.ID, model.StageASR); err == nil {
		t.Fatal("scope mismatch accepted")
	}
}

func TestMaterialAndCourseRerun(t *testing.T) {
	f := newFixture(t)
	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "w1.pdf", Path: "p"}
	f.st.CreateMaterial(f.ctx, m)
	f.done(model.ScopeMaterial, m.ID, model.StageMaterialExtract)
	f.done(model.ScopeMaterial, m.ID, model.StageMaterialTerms)
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeMaterial, m.ID, model.StageMaterialExtract); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.MaterialStages {
		if st := f.job(model.ScopeMaterial, m.ID, s).Status; st != model.StatusPending {
			t.Fatalf("%s = %s", s, st)
		}
	}

	if err := ResetGlossarySeed(f.ctx, f.st, f.course.ID); err != nil {
		t.Fatal(err)
	}
	g := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	if g.Status != model.StatusPending {
		t.Fatal(g.Status)
	}
	f.done(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeCourse, f.course.ID, model.StageGlossarySeed); err != nil {
		t.Fatal(err)
	}
	g2 := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	if g2.ID == g.ID || g2.Status != model.StatusPending {
		t.Fatalf("glossary rerun: %+v", g2)
	}
	// Re-uploading the syllabus while glossary_seed runs is refused.
	f.drive(model.ScopeCourse, f.course.ID, model.StageGlossarySeed, model.StatusQueued, model.StatusRunning)
	if err := ResetGlossarySeed(f.ctx, f.st, f.course.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset while running: %v", err)
	}
}

// ---------- retry / cancel ----------

func TestRetryAndCancel(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	ctx := f.ctx
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	id := f.lj(l.ID, model.StagePrep).ID
	if err := TransitionJob(ctx, f.st, id, model.StatusRunning, model.StatusFailed, db.JobFinish{Error: sp("ffmpeg exploded")}); err != nil {
		t.Fatal(err)
	}
	j := f.lj(l.ID, model.StagePrep)
	if j.FinishedAt == nil || *j.Error != "ffmpeg exploded" {
		t.Fatalf("failed job = %+v", j)
	}

	// Automatic retry keeps the error text and bumps attempt.
	if err := AutoRetry(ctx, f.st, j); err != nil {
		t.Fatal(err)
	}
	j = f.lj(l.ID, model.StagePrep)
	if j.ID != id || j.Status != model.StatusQueued || j.Attempt != 1 || j.Error == nil || j.FinishedAt != nil {
		t.Fatalf("auto retry = %+v", j)
	}
	// Duplicate retry → conflict (409).
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StagePrep); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry queued: %v", err)
	}
	// Cancel queued.
	if err := CancelQueued(ctx, f.st, id); err != nil {
		t.Fatal(err)
	}
	if err := CancelQueued(ctx, f.st, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("double cancel: %v", err)
	}
	j = f.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusCancelled || j.FinishedAt == nil {
		t.Fatalf("cancelled = %+v", j)
	}
	// User retry from cancelled: same job, attempt+1, error cleared.
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StagePrep); err != nil {
		t.Fatal(err)
	}
	j = f.lj(l.ID, model.StagePrep)
	if j.ID != id || j.Status != model.StatusQueued || j.Attempt != 2 || j.Error != nil {
		t.Fatalf("user retry = %+v", j)
	}
	// AutoRetry refuses once attempts are used up.
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusRunning, model.StatusFailed)
	if err := AutoRetry(ctx, f.st, f.lj(l.ID, model.StagePrep)); !errors.Is(err, ErrConflict) {
		t.Fatalf("auto retry after 2 attempts: %v", err)
	}
	// Retry of a pending stage → conflict.
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StageASR); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry pending: %v", err)
	}
	if s := f.lj(l.ID, model.StageASR).Status; s != model.StatusPending {
		t.Fatalf("asr = %s", s)
	}
}

// ---------- allowed actions ----------

func TestAllowedActions(t *testing.T) {
	table := map[string][]string{
		model.StatusPending:   {},
		model.StatusQueued:    {model.ActionCancel},
		model.StatusRunning:   {model.ActionCancel},
		model.StatusDone:      {model.ActionRerun},
		model.StatusStale:     {model.ActionRerun},
		model.StatusFailed:    {model.ActionRetry},
		model.StatusCancelled: {model.ActionRetry},
	}
	for status, want := range table {
		got := AllowedActions(model.Job{Stage: model.StageASR, Status: status}, ActionOpts{})
		if got == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v want %#v", status, got, want)
		}
	}
	prepDone := model.Job{Stage: model.StagePrep, Status: model.StatusDone}
	if got := AllowedActions(prepDone, ActionOpts{MediaDeleted: true}); len(got) != 0 {
		t.Fatalf("prep rerun must vanish when media was deleted: %v", got)
	}
	if got := AllowedActions(model.Job{Stage: model.StageASR, Status: model.StatusDone}, ActionOpts{MediaDeleted: true}); len(got) != 1 {
		t.Fatalf("media deletion only affects prep: %v", got)
	}

	jobs := []model.Job{
		{Stage: model.StagePrep, Status: model.StatusDone},
		{Stage: model.StageASR, Status: model.StatusDone},
		{Stage: model.StageSummary, Status: model.StatusRunning},
		{Stage: model.StageRepair, Status: model.StatusPending},
		{Stage: model.StageTranslate, Status: model.StatusPending},
		{Stage: model.StageExport, Status: model.StatusPending},
	}
	acts := StageActions(jobs, false)
	if len(acts[model.StagePrep]) != 0 || len(acts[model.StageASR]) != 0 {
		t.Fatalf("rerun must be hidden while a downstream stage runs: %v", acts)
	}
	if !reflect.DeepEqual(acts[model.StageSummary], []string{model.ActionCancel}) {
		t.Fatalf("summary actions = %v", acts[model.StageSummary])
	}
	if CanDelete(jobs) || !Reviewable(jobs) {
		t.Fatal("CanDelete/Reviewable with summary running")
	}
	jobs[2].Status = model.StatusDone
	jobs[3].Status = model.StatusRunning
	if CanDelete(jobs) || Reviewable(jobs) {
		t.Fatal("repair running: not deletable, not reviewable")
	}
	jobs[3].Status = model.StatusDone
	jobs[4].Status = model.StatusRunning
	if Reviewable(jobs) {
		t.Fatal("translate running: not reviewable")
	}
	jobs[4].Status = model.StatusDone
	if !CanDelete(jobs) || !Reviewable(jobs) {
		t.Fatal("idle lecture")
	}
}

// ---------- deps / waiting_for ----------

func TestDepsWaitingFor(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.lecture()
	deps := func(stage string) (bool, string) {
		t.Helper()
		ok, w, err := Deps(ctx, f.st, f.lj(l.ID, stage))
		if err != nil {
			t.Fatal(err)
		}
		return ok, w
	}
	if ok, _ := deps(model.StagePrep); !ok {
		t.Fatal("prep has no upstream")
	}
	if ok, w := deps(model.StageASR); ok || w != "upstream:prep" {
		t.Fatalf("asr: %v %q", ok, w)
	}
	if ok, w := deps(model.StageExport); ok || w != "upstream:translate" {
		t.Fatalf("export: %v %q", ok, w)
	}
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	if ok, w := deps(model.StageASR); !ok || w != "" {
		t.Fatalf("asr without syllabus/materials: %v %q", ok, w)
	}

	// Syllabus present → asr waits for glossary_seed.
	f.st.UpsertSyllabus(ctx, model.Syllabus{CourseID: f.course.ID, Filename: "s.md", Path: "p"})
	ResetGlossarySeed(ctx, f.st, f.course.ID)
	if ok, w := deps(model.StageASR); ok || w != model.WaitGlossarySeed {
		t.Fatalf("asr: %v %q", ok, w)
	}
	g := f.job(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)
	if ok, _, _ := Deps(ctx, f.st, g); !ok {
		t.Fatal("glossary_seed has no deps")
	}
	f.done(model.ScopeCourse, f.course.ID, model.StageGlossarySeed)

	// A material bound to this lecture blocks asr; a course-wide one does not.
	wide := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "all.pdf", Path: "p"}
	bound := model.Material{ID: db.NewID(), CourseID: f.course.ID, LectureID: &l.ID, Filename: "week1.pdf", Path: "p"}
	f.st.CreateMaterial(ctx, wide)
	if ok, _ := deps(model.StageASR); !ok {
		t.Fatal("course-wide material must not block asr")
	}
	f.st.CreateMaterial(ctx, bound)
	if ok, w := deps(model.StageASR); ok || w != "material_terms:week1.pdf" {
		t.Fatalf("asr: %v %q", ok, w)
	}
	mt := f.job(model.ScopeMaterial, bound.ID, model.StageMaterialTerms)
	if ok, w, _ := Deps(ctx, f.st, mt); ok || w != "upstream:material_extract" {
		t.Fatalf("material_terms: %v %q", ok, w)
	}
	f.done(model.ScopeMaterial, bound.ID, model.StageMaterialExtract)
	if ok, _, _ := Deps(ctx, f.st, mt); !ok {
		t.Fatal("material_terms after extract")
	}
	f.done(model.ScopeMaterial, bound.ID, model.StageMaterialTerms)
	if ok, w := deps(model.StageASR); !ok {
		t.Fatalf("asr after material_terms: %q", w)
	}

	// A stale upstream still has its outputs: downstream may proceed (NOTES.md).
	f.done(model.ScopeLecture, l.ID, model.StageASR)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusStale)
	if ok, w := deps(model.StageSummary); !ok {
		t.Fatalf("summary with stale asr: %q", w)
	}
	if ok, w := deps(model.StageRepair); ok || w != model.WaitUpstream(model.StageSummary) {
		t.Fatalf("repair: %v %q", ok, w)
	}
}

// ---------- recovery ----------

func TestRecover(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l1 := f.lecture()
	l2 := f.lecture()
	l3 := f.lecture()
	// l1 prep running, attempt 0 → failed → queued attempt 1.
	f.drive(model.ScopeLecture, l1.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	// l2 prep running with attempt 2 → stays failed.
	f.drive(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	AutoRetry(ctx, f.st, f.lj(l2.ID, model.StagePrep))
	f.drive(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusRunning, model.StatusFailed)
	AutoRetry(ctx, f.st, f.lj(l2.ID, model.StagePrep))
	f.drive(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusRunning)
	if a := f.lj(l2.ID, model.StagePrep).Attempt; a != 2 {
		t.Fatalf("setup attempt = %d", a)
	}
	// l3 prep queued → unchanged.
	f.drive(model.ScopeLecture, l3.ID, model.StagePrep, model.StatusQueued)

	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "a.pdf", Path: "p"}
	f.st.CreateMaterial(ctx, m)
	ldir := model.LectureDir(f.dataDir, l1.CourseID, l1.ID)
	mdir := model.MaterialDir(f.dataDir, m.CourseID, m.ID)
	os.MkdirAll(filepath.Join(ldir, model.ExportDirName), 0o755)
	os.MkdirAll(mdir, 0o755)
	tmpFiles := []string{filepath.Join(ldir, "result.json.tmp"), filepath.Join(ldir, model.ExportDirName, "x.vtt.tmp"), filepath.Join(mdir, "result.json.tmp")}
	for _, p := range tmpFiles {
		os.WriteFile(p, []byte("x"), 0o644)
	}
	keep := filepath.Join(ldir, "job.json")
	os.WriteFile(keep, []byte("{}"), 0o644)

	if err := Recover(ctx, f.st, f.dataDir); err != nil {
		t.Fatal(err)
	}
	// §4.7 stores the text "interrupted by restart" verbatim; the API reads
	// it as the code job.interrupted (ParseJobError, lead ruling at the
	// phase-2 gate).
	j1 := f.lj(l1.ID, model.StagePrep)
	if j1.Status != model.StatusQueued || j1.Attempt != 1 || j1.Error == nil || *j1.Error != model.ErrInterruptedByRestart || !IsInterrupted(j1.Error) {
		t.Fatalf("l1 = %+v", j1)
	}
	if info, text := ParseJobError(*j1.Error); info == nil || info.Code != CodeInterrupted || info.Detail != model.ErrInterruptedByRestart || text != model.ErrInterruptedByRestart {
		t.Fatalf("l1 error read as %+v %q", info, text)
	}
	j2 := f.lj(l2.ID, model.StagePrep)
	if j2.Status != model.StatusFailed || j2.Attempt != 2 || j2.Error == nil || *j2.Error != model.ErrInterruptedByRestart || !IsInterrupted(j2.Error) {
		t.Fatalf("l2 = %+v", j2)
	}
	j3 := f.lj(l3.ID, model.StagePrep)
	if j3.Status != model.StatusQueued || j3.Attempt != 0 {
		t.Fatalf("l3 = %+v", j3)
	}
	for _, p := range tmpFiles {
		if exists(p) {
			t.Errorf("%s not deleted", p)
		}
	}
	if !exists(keep) || !exists(filepath.Join(ldir, "media.mp4")) {
		t.Fatal("non-tmp files must be kept")
	}
	// Recover with no data dir at all is fine.
	if err := Recover(ctx, f.st, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

// ---------- incremental translate (SPEC §7.3) ----------

func TestStartIncrementalTranslate(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.fullLecture()
	f.st.SetZhStatus(ctx, l.ID, []int{1}, model.ZhStale)
	oldTr := f.lj(l.ID, model.StageTranslate)

	// export running → busy, nothing changes.
	if err := Rerun(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning)
	started, err := StartIncrementalTranslate(ctx, f.st, l.ID)
	if started || !errors.Is(err, ErrBusy) || !errors.Is(err, ErrConflict) {
		t.Fatalf("export running: %v %v", started, err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusDone)

	started, err = StartIncrementalTranslate(ctx, f.st, l.ID)
	if err != nil || !started {
		t.Fatalf("start: %v %v", started, err)
	}
	tr := f.lj(l.ID, model.StageTranslate)
	if tr.ID == oldTr.ID || tr.Status != model.StatusPending {
		t.Fatalf("translate = %+v", tr)
	}
	if ex := f.lj(l.ID, model.StageExport); ex.Status != model.StatusPending {
		t.Fatalf("export = %+v", ex)
	}
	// Sentence translations are untouched.
	counts, _ := f.st.CountZh(ctx, l.ID)
	if counts[model.ZhOK] != 2 || counts[model.ZhStale] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	// translate not done → no-op.
	started, err = StartIncrementalTranslate(ctx, f.st, l.ID)
	if started || err != nil {
		t.Fatalf("second start: %v %v", started, err)
	}
}

func TestMediaExists(t *testing.T) {
	dir := t.TempDir()
	l := model.Lecture{ID: "L", CourseID: "C", MediaPath: "in-dir.mp4"}
	if MediaExists(dir, l) {
		t.Fatal("missing file reported present")
	}
	ld := model.LectureDir(dir, "C", "L")
	os.MkdirAll(ld, 0o755)
	os.WriteFile(filepath.Join(ld, "in-dir.mp4"), nil, 0o644)
	if !MediaExists(dir, l) {
		t.Fatal("relative to lecture dir")
	}
	abs := filepath.Join(dir, "abs.mp4")
	os.WriteFile(abs, nil, 0o644)
	if !MediaExists(dir, model.Lecture{ID: "X", CourseID: "C", MediaPath: abs}) {
		t.Fatal("absolute path")
	}
}

// FailJob: running → failed → queued happens in one transaction, so
// "failed" is only ever committed as a final state (Gate A item 3).
func TestFailJobRequeuesAtomically(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	l := f.lecture()
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	id := f.lj(l.ID, model.StagePrep).ID

	requeued, err := FailJob(ctx, f.st, id, "HTTP 503", true)
	if err != nil || !requeued {
		t.Fatalf("FailJob: %v %v", requeued, err)
	}
	j := f.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusQueued || j.Attempt != 1 || j.Error == nil || *j.Error != "HTTP 503" || j.FinishedAt != nil {
		t.Fatalf("after retryable failure: %+v", j)
	}
	// Permanent (retry=false): stays failed.
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusRunning)
	requeued, err = FailJob(ctx, f.st, id, "bad input", false)
	if err != nil || requeued {
		t.Fatalf("permanent: %v %v", requeued, err)
	}
	if j := f.lj(l.ID, model.StagePrep); j.Status != model.StatusFailed || j.Attempt != 1 || j.FinishedAt == nil {
		t.Fatalf("after permanent failure: %+v", j)
	}
	// Attempts used up: stays failed even with retry=true.
	Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StagePrep) // attempt 2
	f.drive(model.ScopeLecture, l.ID, model.StagePrep, model.StatusRunning)
	requeued, err = FailJob(ctx, f.st, id, "again", true)
	if err != nil || requeued {
		t.Fatalf("attempts used up: %v %v", requeued, err)
	}
	if j := f.lj(l.ID, model.StagePrep); j.Status != model.StatusFailed || j.Attempt != 2 {
		t.Fatalf("final: %+v", j)
	}
	// Not running → conflict, nothing written.
	if _, err := FailJob(ctx, f.st, id, "x", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("FailJob on failed job: %v", err)
	}
}

func TestCheckStaleJob(t *testing.T) {
	f := newFixture(t)
	l := f.lecture()
	f.done(model.ScopeLecture, l.ID, model.StagePrep)
	h := "h1"
	hasher := HasherFunc(func(ctx context.Context, id string) (string, error) { return h, nil })
	j := f.lj(l.ID, model.StagePrep)
	if stale, err := CheckStaleJob(f.ctx, f.st, hasher, j); stale || err != nil {
		t.Fatalf("no recorded hash: %v %v", stale, err)
	}
	f.setHash(model.ScopeLecture, l.ID, model.StagePrep, "h1")
	j = f.lj(l.ID, model.StagePrep)
	if stale, err := CheckStaleJob(f.ctx, f.st, hasher, j); stale || err != nil {
		t.Fatalf("same hash: %v %v", stale, err)
	}
	h = "h2"
	if stale, err := CheckStaleJob(f.ctx, f.st, hasher, j); !stale || err != nil {
		t.Fatalf("changed hash: %v %v", stale, err)
	}
	if s := f.lj(l.ID, model.StagePrep).Status; s != model.StatusStale {
		t.Fatalf("status = %s", s)
	}
	boom := HasherFunc(func(ctx context.Context, id string) (string, error) { return "", errors.New("wav missing") })
	f.done(model.ScopeLecture, l.ID, model.StageASR)
	f.setHash(model.ScopeLecture, l.ID, model.StageASR, "x")
	var he *HashError
	if _, err := CheckStaleJob(f.ctx, f.st, boom, f.lj(l.ID, model.StageASR)); !errors.As(err, &he) {
		t.Fatalf("hasher error must be a HashError: %v", err)
	}
	// CheckStaleScope logs and skips hasher errors.
	if _, err := CheckStaleScope(f.ctx, f.st, map[string]Hasher{model.StageASR: boom}, model.ScopeLecture, l.ID); err != nil {
		t.Fatalf("scope check must skip hasher errors: %v", err)
	}
}
