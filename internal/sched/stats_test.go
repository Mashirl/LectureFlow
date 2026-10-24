// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The scheduler is the only writer of job.stats during a run (StatsSink).

func TestStatsSinkPersistedOnFailureAndRetry(t *testing.T) {
	var runs int
	var e *env
	// What the llm client does for every response: job.stats and usage_daily.
	report := func(ctx context.Context, prompt, completion, hit int64) {
		ReportUsage(ctx, prompt, completion, hit)
		e.s.Pool().RecordUsage(context.WithoutCancel(ctx), "deepseek", prompt, hit, completion)
	}
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		runs++
		if runs == 1 {
			report(ctx, 1000, 100, 50)
			report(ctx, 1000, 100, 50)
			UpdateStats(ctx, func(st *model.JobStats) {
				st.Note = "chunk 2 failed"
				st.PromptTermsUsed = 37
				st.Status = "downloading_model"
				st.PromptTokens = 999999 // token fields belong to AddUsage: ignored
			})
			return errors.New("HTTP 503")
		}
		j, _ := JobFrom(ctx)
		if j.Stats.PromptTokens != 2000 || j.Stats.Note != "chunk 2 failed" {
			return Permanent(errors.New("retry did not start from the stored stats"))
		}
		report(ctx, 500, 50, 0)
		return Permanent(errors.New("HTTP 401"))
	}}
	e = newEnv(t, func(c *config.Config) {
		c.LLM.Prices = map[string]map[string]config.Price{"deepseek": {"deepseek-chat": {Input: 1, CacheHit: 0.5, Output: 2}}}
	}, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()
	var j model.Job
	e.waitFor("final failure", 5*time.Second, func() bool {
		j = e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)
		return j.Status == model.StatusFailed
	})
	st := j.Stats
	if st.PromptTokens != 2500 || st.CompletionTokens != 250 || st.CacheHitTokens != 100 || st.Requests != 3 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Note != "chunk 2 failed" || st.PromptTermsUsed != 37 || st.Status != "" || st.DurationMS < 0 {
		t.Fatalf("non-token fields = %+v", st)
	}
	if *j.Error != "HTTP 401" {
		t.Fatalf("error = %q (the retry did not see the stored stats?)", *j.Error)
	}
	// cost = ((2500-100)*1 + 100*0.5 + 250*2) / 1e6
	if want := 2950.0 / 1e6; math.Abs(st.EstimatedCost-want) > 1e-12 {
		t.Fatalf("estimated_cost = %v, want %v", st.EstimatedCost, want)
	}
	u, _ := e.s.UsageToday(e.ctx)
	if u.PromptTokens != 2500 || u.CacheHitTokens != 100 || u.CompletionTokens != 250 || u.EstimatedCost == nil || math.Abs(*u.EstimatedCost-2950.0/1e6) > 1e-12 {
		t.Fatalf("usage today = %+v", u)
	}
}

func TestStatsSinkFlushedWhileRunningAndOnCancel(t *testing.T) {
	reported := make(chan struct{})
	var e *env
	report := func(ctx context.Context, prompt, completion, hit int64) {
		ReportUsage(ctx, prompt, completion, hit)
		e.s.Pool().RecordUsage(context.WithoutCancel(ctx), "deepseek", prompt, hit, completion)
	}
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		report(ctx, 700, 70, 0)
		UpdateStats(ctx, func(st *model.JobStats) { st.Status = "downloading_model" })
		close(reported)
		<-ctx.Done()
		report(ctx, 300, 30, 0) // an in-flight response that completed during cancel
		return ctx.Err()
	}}
	e = newEnv(t, nil, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	tj := func() model.Job { return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms) }
	e.start()
	<-reported
	// Stats are flushed while running; usage_daily was written with each response.
	e.waitFor("stats flushed and counted while running", 5*time.Second, func() bool {
		j := tj()
		u, _ := e.s.UsageToday(e.ctx)
		return j.Status == model.StatusRunning && j.Stats.PromptTokens == 700 && j.Stats.Status == "downloading_model" &&
			u.PromptTokens == 700
	})
	if err := e.s.Cancel(tj().ID); err != nil {
		t.Fatal(err)
	}
	j := tj()
	if j.Status != model.StatusCancelled || j.Stats.PromptTokens != 1000 || j.Stats.CompletionTokens != 100 || j.Stats.Requests != 2 || j.Stats.Status != "" {
		t.Fatalf("after cancel: %+v stats=%+v", j.Status, j.Stats)
	}
	if u, _ := e.s.UsageToday(e.ctx); u.PromptTokens != 1000 {
		t.Fatalf("usage after cancel = %+v", u)
	}
}

func TestStatsAndProgressNeverBlock(t *testing.T) {
	ready := make(chan struct{})
	locked := make(chan struct{})
	took := make(chan time.Duration, 1)
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		close(ready)
		<-locked
		t0 := time.Now()
		for i := 0; i < 5000; i++ {
			p(float64(i) / 5000)
			ReportUsage(ctx, 1, 1, 0)
			UpdateStats(ctx, func(st *model.JobStats) { st.Note = "busy" })
		}
		took <- time.Since(t0)
		return nil
	}}
	e := newEnv(t, nil, prep)
	l := e.lecture()
	e.start()
	<-ready
	release := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- e.st.Tx(context.Background(), func(tx db.Store) error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case d := <-took:
		if d > 500*time.Millisecond {
			t.Fatalf("5000 progress/stats calls took %v while the database was busy", d)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("progress or stats callback blocked on the database")
	}
	close(release)
	if err := <-txDone; err != nil {
		t.Fatal(err)
	}
	j := e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	if j.Stats.PromptTokens != 5000 || j.Stats.Requests != 5000 || j.Stats.Note != "busy" {
		t.Fatalf("stats = %+v", j.Stats)
	}
}

// Gate B ruling 4: a done job keeps stats.warning in job.error; a new
// attempt (retry or rerun) starts without the old warning.
func TestWarningBecomesDoneError(t *testing.T) {
	const warn = "块 2 修复失败，未产生 patch"
	var prepRuns, termRuns atomic.Int32
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		if prepRuns.Add(1) == 1 {
			UpdateStats(ctx, func(st *model.JobStats) { st.Warning = warn })
		}
		return nil
	}}
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		if termRuns.Add(1) == 1 {
			UpdateStats(ctx, func(st *model.JobStats) { st.Warning = "partial" })
			return errors.New("HTTP 503") // auto retry, same job
		}
		return nil
	}}
	e := newEnv(t, nil, prep, instant(model.StageMaterialExtract, ResourceCPU), terms)
	l := e.lecture()
	dir := model.LectureDir(e.dataDir, e.course.ID, l.ID) // media for the prep rerun
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "m.mp4"), []byte("m"), 0o644)
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()

	j := e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	if j.Error == nil || *j.Error != warn || j.Stats.Warning != warn {
		t.Fatalf("done with warning: error=%v stats=%+v", j.Error, j.Stats)
	}
	// Rerun: the new run has no warning → error cleared.
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StagePrep, model.ActionRerun); err != nil {
		t.Fatal(err)
	}
	e.waitFor("rerun done", 5*time.Second, func() bool {
		return prepRuns.Load() == 2 && e.lj(l.ID, model.StagePrep).Status == model.StatusDone
	})
	if j = e.lj(l.ID, model.StagePrep); j.Error != nil || j.Stats.Warning != "" {
		t.Fatalf("rerun kept the old warning: error=%v stats=%+v", j.Error, j.Stats)
	}

	// Retry of the same job: the first attempt's warning does not survive.
	var tj model.Job
	e.waitFor("material_terms done", 5*time.Second, func() bool {
		tj = e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)
		return tj.Status == model.StatusDone
	})
	if termRuns.Load() != 2 || tj.Attempt != 1 || tj.Error != nil || tj.Stats.Warning != "" {
		t.Fatalf("retried job: runs=%d stats=%+v error=%v", termRuns.Load(), tj.Stats, tj.Error)
	}
}
