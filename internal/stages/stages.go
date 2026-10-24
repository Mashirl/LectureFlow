// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package stages implements every automatic task of SPEC §4.1/§4.3/§6 as a
// sched.Stage: prep, asr, summary, repair, translate, export (lecture),
// glossary_seed and glossary_review (course), material_extract and
// material_terms (material).
//
// Rules shared by all stages (internal/sched/scheduler.go file header):
//   - no LLM call and no waiting on database work inside Store.Tx: results are
//     collected in memory, the transaction only writes them;
//   - the progress callback is non-blocking and may be called from any goroutine;
//   - job.stats of a running job is written only through sched.UpdateStats
//     (token usage is reported by the llm client via sched.ReportUsage);
//   - files are written to *.tmp first and renamed (SPEC §4.7).
package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// Deps are the collaborators of every stage.
type Deps struct {
	Store   db.Store
	Cfg     *config.Config
	LLM     llm.Client
	DataDir string
	Events  sched.Events // nil = sched.NopEvents
	// Invalidate runs the course-wide invalidation check (SPEC §4.4) outside
	// any transaction, telling it why (v1.1 A2.2: the stale_reason of the
	// jobs it marks); wired to Scheduler.InvalidateCourseWith. nil = no-op.
	Invalidate func(ctx context.Context, courseID string, cause state.StaleCause) error
	// GPUTotalMB returns the total VRAM for compute_type=auto (SPEC §17.2);
	// wired to Scheduler.GPUStatus. nil or <= 0 = unknown.
	GPUTotalMB func(ctx context.Context) int
	// Concurrency is the number of requests one API stage keeps in flight
	// (translate sentences, repair and material_terms chunks).
	// 0 = the max_concurrency of the stage's provider (the job snapshot's
	// provider for lecture stages, the role's current provider for course
	// stages). The API pool still enforces the provider limits across all stages.
	Concurrency int
	// Settings is the source of the current settings (v1.1 A5):
	// keep_original_media and the providers' max_concurrency. Lecture stages
	// never read the current model settings: they use their job's
	// config_snapshot (A4.1). nil = the legacy fields of Cfg (tests of the
	// old path).
	Settings *settings.Service
	// Now is the clock of generated_at in the citation JSON (A11). nil = time.Now.
	Now func() time.Time
}

// All returns the nine stages of SPEC §4.1 and §4.3 and the course task
// glossary_review of SPEC-v2 G3.1, sharing d.
func All(d Deps) []sched.Stage {
	if d.Events == nil {
		d.Events = sched.NopEvents{}
	}
	return []sched.Stage{
		&prepStage{d: d},
		newASRStage(d),
		&summaryStage{apiStage{d: d, role: config.RoleSummary}},
		&repairStage{apiStage{d: d, role: config.RoleRepair}},
		&translateStage{apiStage{d: d, role: config.RoleTranslate}},
		&exportStage{d: d},
		&glossaryStage{apiStage{d: d, role: config.RoleGlossary}},
		&materialExtractStage{d: d},
		&materialTermsStage{apiStage{d: d, role: config.RoleGlossary}},        // SPEC §2: material_terms uses the glossary role
		&glossaryReviewStage{apiStage{d: d, role: model.RoleGlossaryJanitor}}, // SPEC-v2 G3: the glossary janitor
		// SPEC-v2 auxiliary tasks (not pipeline stages, phase 2 lead ruling 1)
		&titleStage{apiStage{d: d, role: model.RoleOrganizer}},        // P13.5
		&chaptersStage{apiStage{d: d, role: model.RoleChapters}},      // P7
		&filenameRuleStage{apiStage{d: d, role: model.RoleOrganizer}}, // P13.3.2
		&streamStage{d: d}, // P8.1: stream.mp4 for lectures prepared before v2 (CPU)
	}
}

// ByName returns All(d) keyed by stage name (for sched.Options.Stages).
func ByName(d Deps) map[string]sched.Stage {
	m := map[string]sched.Stage{}
	for _, s := range All(d) {
		m[s.Name()] = s
	}
	return m
}

func (d Deps) events() sched.Events {
	if d.Events == nil {
		return sched.NopEvents{}
	}
	return d.Events
}

func (d Deps) invalidate(ctx context.Context, courseID string, cause state.StaleCause) {
	if d.Invalidate == nil {
		return
	}
	if err := d.Invalidate(context.WithoutCancel(ctx), courseID, cause); err != nil {
		slog.Warn("course invalidation", "course", courseID, "cause", cause.Kind, "err", err)
	}
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// keepOriginalMedia is SPEC §17.5 keep_original_media (v1.1: a general setting).
func (d Deps) keepOriginalMedia() bool {
	if d.Settings != nil {
		return d.Settings.General().KeepOriginalMedia
	}
	return d.Cfg.Storage.KeepMedia()
}

// provider returns the id of the provider currently set for role (course
// stages, and the scheduler's api_slot hint). Legacy: the config key.
func (d Deps) provider(role string) string {
	if d.Settings != nil {
		if t, err := d.Settings.RoleTarget(role); err == nil {
			return t.ProviderID
		}
		return ""
	}
	p, _ := llm.RoleProvider(d.Cfg, role)
	return p
}

// concurrency is the number of parallel requests of one stage run: the
// max_concurrency of providerID ("" = the role's current provider).
func (d Deps) concurrency(role, providerID string) int {
	if d.Concurrency > 0 {
		return d.Concurrency
	}
	if providerID == "" {
		providerID = d.provider(role)
	}
	if d.Settings != nil {
		if p, ok := d.Settings.Provider(providerID); ok && p.MaxConcurrency > 0 {
			return p.MaxConcurrency
		}
		return 1
	}
	if p, ok := d.Cfg.LLM.Providers[providerID]; ok && p.MaxConcurrency > 0 {
		return p.MaxConcurrency
	}
	if p, ok := d.Cfg.LLM.Providers[d.provider(role)]; ok && p.MaxConcurrency > 0 {
		return p.MaxConcurrency
	}
	return 1
}

// ---------- model configuration snapshots (v1.1 A4) ----------

// MissingSnapshot is the error of a lecture stage whose job has no usable
// config_snapshot (lead ruling 4, NOTES.md). The stage fails permanently:
// running it with the current settings would silently change the model.
const MissingSnapshot = "该阶段缺少模型配置快照，请重跑并选择使用当前设置"

// currentJob returns the job a stage runs for: the one the scheduler put in
// ctx, else the current job of (scope, scopeID, stage).
func (d Deps) currentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error) {
	if j, ok := sched.JobFrom(ctx); ok && j.Stage == stage && j.ScopeID == scopeID {
		return j, nil
	}
	return d.Store.GetCurrentJob(ctx, scope, scopeID, stage)
}

// llmTarget reads the LLM snapshot of the running lecture job and attaches
// it to ctx with llm.WithTarget, so every request of the run goes to the
// snapshot's provider and model (A4.1: "阶段运行时只读取自己的快照"). The
// job is returned for its other snapshot parameters (translate's window).
func (a apiStage) llmTarget(ctx context.Context, lectureID, stage string) (context.Context, model.Job, model.LLMSnapshot, error) {
	j, err := a.d.currentJob(ctx, model.ScopeLecture, lectureID, stage)
	if err != nil {
		return ctx, j, model.LLMSnapshot{}, err
	}
	snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		return ctx, j, snap, sched.Permanent(model.Errorf(CodeSnapshotMissing, nil, errors.New(MissingSnapshot)))
	}
	return llm.WithTarget(ctx, snap), j, snap, nil
}

// recordGlossaryVersion writes stats.glossary_version: the course glossary
// the run used (v1.1 A2.2 "术语表已更新（v3 → v4）").
func recordGlossaryVersion(ctx context.Context, v int) {
	sched.UpdateStats(ctx, func(st *model.JobStats) { st.GlossaryVersion = &v })
}

func (d Deps) lectureDir(l model.Lecture) string {
	return model.LectureDir(d.DataDir, l.CourseID, l.ID)
}

// resolvePath resolves a stored path: absolute, relative to the working
// directory when it exists there, otherwise relative to base (the lecture,
// material or syllabus directory). Same rule as state.MediaExists.
func resolvePath(p, base string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return filepath.Join(base, p)
}

// within reports whether path lies inside dir.
func within(path, dir string) bool {
	ap, err1 := filepath.Abs(path)
	ad, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(ad, ap)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// lectureData is a lecture with its sentences and prompt snapshot.
type lectureData struct {
	lec       model.Lecture
	sentences []model.Sentence
	snap      ctxbuild.LectureSnapshot
	hasSum    bool
	// glossaryVersion is course.glossary_version read together with the
	// terms of snap (v1.1: recorded in stats.glossary_version).
	glossaryVersion int
}

// loadLecture loads everything the lecture-level prompts and hashes need,
// in one read transaction so that the terms and the glossary version agree.
// The snapshot comes from state.LoadSnapshot, the same loader the
// sentence-level invalidation of review operations uses, so the zh_hash
// written by translate and the hash recomputed on review always agree.
func (d Deps) loadLecture(ctx context.Context, lectureID string) (lectureData, error) {
	var ld lectureData
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		ld = lectureData{}
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return fmt.Errorf("讲座 %s：%w", lectureID, err)
		}
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		snap, err := state.LoadSnapshot(ctx, tx, lectureID)
		if err != nil {
			return err
		}
		ss, err := tx.ListSentences(ctx, lectureID, 0, 0)
		if err != nil {
			return err
		}
		if len(ss) != len(snap.Sentences) {
			return model.Errorf(CodeSentencesChanged, nil, fmt.Errorf("讲座 %s 的句子在读取期间发生了变化，请重试", lectureID))
		}
		switch _, err := tx.GetSummary(ctx, lectureID); {
		case err == nil:
			ld.hasSum = true
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		ld.lec, ld.sentences, ld.snap, ld.glossaryVersion = l, ss, snap, c.GlossaryVersion
		return nil
	})
	return ld, err
}

// ---------- LLM JSON requests with business retries (SPEC §12) ----------

// businessRetries is the number of extra attempts after an unparsable or
// invalid answer (repair: 2 per chunk, translate: 2 per sentence, SPEC §12).
const businessRetries = 2

// errUnusable marks an answer that stayed unparsable/invalid after the
// business retries. Transport errors (llm client) are returned unwrapped.
type errUnusable struct{ last error }

func (e *errUnusable) Error() string {
	return fmt.Sprintf("模型输出连续 %d 次无法使用：%v", businessRetries+1, e.last)
}
func (e *errUnusable) Unwrap() error { return e.last }

// As lets errors.As find the coded form (v2 P14.3): CodeOutputUnusable with
// the text above as the detail.
func (e *errUnusable) As(target any) bool {
	if p, ok := target.(**model.CodedError); ok {
		*p = &model.CodedError{Info: model.ErrorInfo{Code: CodeOutputUnusable, Params: map[string]any{"attempts": businessRetries + 1}},
			Err: errors.New(e.Error())}
		return true
	}
	return false
}

func isUnusable(err error) bool {
	var u *errUnusable
	return errors.As(err, &u)
}

// askJSON sends one JSON-mode request and decodes the answer into a fresh T,
// retrying up to businessRetries times when the answer cannot be parsed or
// check rejects it. Transport and context errors are returned at once.
func askJSON[T any](ctx context.Context, c llm.Client, role, system, user string, check func(*T) error) (T, error) {
	var last error
	for range businessRetries + 1 {
		var v T
		res, err := c.Complete(ctx, role, system, user, true)
		if err != nil {
			return v, err
		}
		if err := llm.ParseJSON(res.Text, &v); err != nil {
			last = err
			continue
		}
		if check != nil {
			if err := check(&v); err != nil {
				last = err
				continue
			}
		}
		return v, nil
	}
	var zero T
	return zero, &errUnusable{last: last}
}

// parallel runs fn(i) for i in [0, n) with at most workers goroutines. When
// fn returns a fatal error (stop == true) no new item is started; the first
// fatal error is returned after the running ones finish.
func parallel(ctx context.Context, n, workers int, fn func(ctx context.Context, i int) (stop bool, err error)) error {
	if n == 0 {
		return nil
	}
	workers = max(1, min(workers, n))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		next  int
		first error
		wg    sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if first != nil || next >= n || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				stop, err := fn(ctx, i)
				if stop {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return first
}

// progressCounter reports done/total through the progress callback.
type progressCounter struct {
	mu          sync.Mutex
	done, total int
	report      func(float64)
}

func (p *progressCounter) inc() {
	p.mu.Lock()
	p.done++
	v := float64(p.done) / float64(max(p.total, 1))
	p.mu.Unlock()
	p.report(v)
}

// writeFileAtomic writes data to path via path+".tmp" and a rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func jobID(ctx context.Context) string {
	if j, ok := sched.JobFrom(ctx); ok {
		return j.ID
	}
	return ""
}

// apiStage holds what every API stage shares.
type apiStage struct {
	d    Deps
	role string
}

func (a apiStage) Resource() sched.ResourceKind { return sched.ResourceAPI }

// Provider is the provider currently set for the stage's role. For lecture
// jobs the scheduler uses the provider of the job's snapshot (v1.1 sched).
func (a apiStage) Provider() string { return a.d.provider(a.role) }
