// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// TestPipelineThroughScheduler wires stages.All into the real scheduler with
// the fake LLM, fake ASR and a fake extract_pdf worker: syllabus, a material
// bound to the lecture and a 3-second lecture go from upload to export with
// no manual step (SPEC §11, §14). It then rejects the decide patch and checks
// that the incremental run retranslates exactly [i-x, i+x].
func TestPipelineThroughScheduler(t *testing.T) {
	e := newEnv(t)
	ff := ffmpegOrSkip(t, e.cfg)
	absFake, _ := filepath.Abs(fakeASR)
	e.cfg.ASR.FakeResult = absFake
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["Cache hierarchy", "Direct mapped caches and the miss rate"]}`), 0o644))
	e.useFakeWorker("ok", pages) // extract_pdf only: asr is in fake_result mode

	var s *sched.Scheduler
	d := e.deps(nil)
	d.Invalidate = func(ctx context.Context, courseID string, cause state.StaleCause) error {
		return s.InvalidateCourseWith(ctx, courseID, cause)
	}
	d.GPUTotalMB = func(ctx context.Context) int { return s.GPUStatus(ctx).TotalMB }
	s, err := sched.New(sched.Options{
		Store: e.st, Config: e.cfg, Events: e.ev, DataDir: e.cfg.DataDir,
		GPUQuery: func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 20000, TotalMB: 24000, UsedMB: 1000}, nil
		},
		ScanInterval: 5 * time.Millisecond, ProgressMinInterval: 5 * time.Millisecond, IncrementalDebounce: 20 * time.Millisecond,
		RetryBackoff: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := llm.New(e.cfg, s.Pool(), "", nil)
	client.Backoff = []time.Duration{time.Millisecond}
	d.LLM = client
	for _, stg := range All(d) {
		s.Register(stg)
	}

	// course: syllabus (text file) + a material bound to the lecture
	sdir := model.SyllabusDir(e.cfg.DataDir, e.course.ID)
	e.must(os.MkdirAll(sdir, 0o755))
	e.must(os.WriteFile(filepath.Join(sdir, "syllabus.txt"), []byte("Computer architecture: caches, write-back, LRU."), 0o644))
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "syllabus.txt", Path: "syllabus.txt"}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	l := e.lecture("Lecture 1: Caches", "upload.wav")
	silentMedia(t, ff, filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "upload.wav"), 3)
	lid := l.ID
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: &lid, Filename: "caches.pdf", Path: "caches.pdf", CreatedAt: db.Now()}
	e.must(e.st.CreateMaterial(e.ctx, m))
	mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
	e.must(os.MkdirAll(mdir, 0o755))
	e.must(os.WriteFile(filepath.Join(mdir, "caches.pdf"), []byte("%PDF-1.4"), 0o644))

	e.must(state.Recover(e.ctx, e.st, e.cfg.DataDir))
	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)

	allDone := func() bool {
		for _, st := range model.LectureStages {
			if e.job(model.ScopeLecture, l.ID, st).Status != model.StatusDone {
				return false
			}
		}
		return true
	}
	waitUntil(t, e, l.ID, 60*time.Second, allDone)

	for _, st := range []struct{ scope, id, stage string }{
		{model.ScopeCourse, e.course.ID, model.StageGlossarySeed},
		{model.ScopeMaterial, m.ID, model.StageMaterialExtract},
		{model.ScopeMaterial, m.ID, model.StageMaterialTerms},
	} {
		if j := e.job(st.scope, st.id, st.stage); j.Status != model.StatusDone {
			t.Errorf("%s: %s (%v)", st.stage, j.Status, j.Error)
		}
	}
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	for _, n := range e.exportNames(l) {
		if fi, err := os.Stat(filepath.Join(dir, n)); err != nil || fi.Size() == 0 {
			t.Errorf("export %s: %v", n, err)
		}
	}
	for _, sn := range e.sentences(l.ID) {
		if sn.ZhStatus != model.ZhOK {
			t.Errorf("sentence %d: %s %v", sn.Idx, sn.ZhStatus, sn.QAError)
		}
	}
	if n, _ := e.st.CountPatches(e.ctx, l.ID, model.ReviewPending); n != 2 {
		t.Errorf("%d pending patches, want 2 (decide 9, downgraded 11)", n)
	}
	for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		if j := e.job(model.ScopeLecture, l.ID, st); j.Stats.PromptTokens == 0 || j.Stats.CacheHitTokens == 0 && st == model.StageTranslate {
			t.Errorf("%s stats %+v: the scheduler sink must persist token usage", st, j.Stats)
		}
	}
	if j := e.job(model.ScopeLecture, l.ID, model.StageASR); j.Stats.PromptTermsUsed != 3 {
		t.Errorf("asr stats %+v", j.Stats)
	}
	if len(e.terms()) == 0 {
		t.Error("no terms merged")
	}

	// review: reject the decide patch of sentence 9 → only [9-x, 9+x] retranslated
	x := e.cfg.LLM.Roles["translate"].Window
	exportBefore, _ := os.ReadFile(filepath.Join(dir, e.exportNames(l)[0]))
	p, err := e.st.PatchForSentence(e.ctx, l.ID, 9)
	e.must(err)
	e.fake.Reset()
	_, n, err := state.ReviewPatch(e.ctx, e.st, x, p.ID, model.ReviewActionReject, "")
	e.must(err)
	if n == 0 {
		t.Fatal("rejecting a patch marked nothing stale")
	}
	trBefore := e.job(model.ScopeLecture, l.ID, model.StageTranslate).ID
	s.RequestIncrementalTranslate(l.ID)
	waitUntil(t, e, l.ID, 30*time.Second, func() bool {
		return e.job(model.ScopeLecture, l.ID, model.StageTranslate).ID != trBefore && allDone()
	})
	idxs := e.fake.TranslateIdxs()
	sort.Ints(idxs)
	idxs = slices.Compact(idxs)
	for _, i := range idxs {
		if i < 9-x || i > 9+x {
			t.Errorf("sentence %d retranslated outside [%d, %d]", i, 9-x, 9+x)
		}
	}
	if len(idxs) != n {
		t.Errorf("retranslated %v, review reported %d stale", idxs, n)
	}
	if !slices.Contains(idxs, 9) {
		t.Errorf("the rejected sentence was not retranslated: %v", idxs)
	}
	exportAfter, _ := os.ReadFile(filepath.Join(dir, e.exportNames(l)[0]))
	if string(exportAfter) == string(exportBefore) {
		t.Error("export not updated after the incremental translation")
	}
	if e.fake.Count(llmtest.RoleRepair) != 0 || e.fake.Count(llmtest.RoleSummary) != 0 {
		t.Error("review must not rerun repair or summary")
	}
}

func waitUntil(t *testing.T, e *env, lectureID string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			for _, st := range model.LectureStages {
				j := e.job(model.ScopeLecture, lectureID, st)
				t.Logf("%s: %s attempt=%d err=%v", st, j.Status, j.Attempt, j.Error)
			}
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
