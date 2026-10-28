// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// IncrementalPrefixChanged is the error of an incremental translate run whose
// prefix (glossary / summary) changed after it was requested.
const IncrementalPrefixChanged = "审阅后术语表或摘要发生了变化，增量重翻已停止；请重跑翻译阶段以按新术语重新翻译整节课"

// translateStage implements SPEC §7: per-sentence translation with QA,
// written sentence by sentence.
type translateStage struct{ apiStage }

func (s *translateStage) Name() string { return model.StageTranslate }

// InputHash = SHA-256(translate prefix + x): a glossary or summary change
// makes the whole stage stale (SPEC §7.3 third rule); sentence-level changes
// are handled by zh_status (state's sentence-level invalidation). x is the
// window of the current translate job's snapshot (v1.1 lead ruling 4), the
// x the run used — never the current setting.
func (s *translateStage) InputHash(ctx context.Context, lectureID string) (string, error) {
	ld, err := s.d.loadLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	j, err := s.d.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if err != nil {
		return "", err
	}
	return ctxbuild.TranslateStageHash(ld.snap, model.SnapshotWindow(j.ConfigSnapshot)), nil
}

type translateAnswer struct {
	Idx *int    `json:"idx"`
	Zh  *string `json:"zh"`
}

// translation is the result for one sentence.
type translation struct {
	zh      string
	status  string // ok | qa_failed
	qaError string
	hash    string
}

// translateTodo returns the sentences a run must translate (lead ruling at
// Gate B, NOTES.md; SPEC §4.6, §7.3, §11): zh_status none or stale, plus ok
// or qa_failed sentences whose zh_hash differs from the current hash (their
// inputs changed: §7.3 "哈希不同即 stale"). The same set for every run —
// first run, retry and incremental run; stats.note = incremental does not
// change it. qa_failed with an unchanged hash is not redone (§4.6).
func translateTodo(ss []model.Sentence, hashes []string) []int {
	var todo []int
	for i, s := range ss {
		switch s.ZhStatus {
		case model.ZhNone, model.ZhStale:
			todo = append(todo, i)
		case model.ZhOK, model.ZhQAFailed:
			if s.ZhHash == nil || i >= len(hashes) || *s.ZhHash != hashes[i] {
				todo = append(todo, i)
			}
		}
	}
	return todo
}

func (s *translateStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	// The job's snapshot gives the model and the window x (v1.1 A4.1). An
	// incremental run carries the snapshot of the translate job it follows
	// (A4.2), so a lecture's translations come from one model.
	ctx, job, target, err := s.llmTarget(ctx, lectureID, model.StageTranslate)
	if err != nil {
		return err
	}
	ld, err := d.loadLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if !ld.hasSum {
		return sched.Permanent(model.Errorf(CodeTranslateNoSummary, nil, errors.New("找不到本讲座的摘要（summary 的产物），请重跑 summary 阶段")))
	}
	x := model.SnapshotWindow(job.ConfigSnapshot)
	// An incremental run (SPEC §7.3) only redoes what a review touched. If the
	// glossary or summary changed while it waited, every sentence hash has
	// changed: that is the whole-stage stale case of §7.3 rule 3, which must
	// not retranslate the lecture automatically (lead ruling, NOTES.md).
	if base, ok := state.IncrementalBaseHash(job.Stats.Note); ok && base != "" && base != ctxbuild.TranslateStageHash(ld.snap, x) {
		return sched.Permanent(model.Errorf(CodeTranslatePrefixChanged, nil, errors.New(IncrementalPrefixChanged)))
	}
	recordGlossaryVersion(ctx, ld.glossaryVersion)
	todo := translateTodo(ld.sentences, ctxbuild.TranslateHashes(ld.snap, x))
	if len(todo) == 0 {
		progress(1)
		return nil
	}

	var (
		mu     sync.Mutex
		failed []int
	)
	pc := &progressCounter{total: len(todo), report: progress}
	ev := d.events()
	notify := newThrottle(time.Second)
	wctx := context.WithoutCancel(ctx)
	// The first sentence goes out alone to warm the provider's prompt cache
	// (perf-llm, warmup.go); the order of sending is all that changes.
	err = parallelWarm(ctx, len(todo), d.concurrency(config.RoleTranslate, target.ProviderID), translateWarmup, func(ctx context.Context, k int) (bool, error) {
		idx := todo[k]
		tr, err := s.translateOne(ctx, ld.snap, idx, x)
		if err != nil {
			if isUnusable(err) {
				slog.Warn("translate sentence unusable", "lecture", lectureID, "idx", idx, "err", err)
				mu.Lock()
				failed = append(failed, idx)
				mu.Unlock()
				pc.inc()
				return false, nil
			}
			return true, err
		}
		// Written immediately (SPEC §4.6): a cancel or failure keeps it.
		var qe *string
		if tr.qaError != "" {
			qe = &tr.qaError
		}
		zh, h := tr.zh, tr.hash
		if err := d.Store.SetTranslation(wctx, lectureID, idx, db.SentenceTr{Zh: &zh, ZhStatus: tr.status, ZhHash: &h, QAError: qe}); err != nil {
			return true, err
		}
		pc.inc()
		if notify.ok() {
			ev.Changed("sentences", lectureID)
		}
		return false, nil
	})
	ev.Changed("sentences", lectureID)
	ev.Changed("lecture", lectureID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		// done must not leave untranslated sentences (lead ruling, NOTES.md):
		// fail loudly; a retry only redoes these (they are still none/stale).
		sort.Ints(failed)
		shown := failed
		more := ""
		if len(shown) > 20 {
			shown, more = shown[:20], "…"
		}
		ids := make([]string, len(shown))
		for i, idx := range shown {
			ids[i] = fmt.Sprintf("#%d", idx)
		}
		return model.Errorf(CodeTranslateFailed, map[string]any{"n": len(failed), "ids": strings.Join(ids, ", ") + more, "attempts": businessRetries + 1},
			fmt.Errorf("%d 句没有得到译文（模型输出连续 %d 次无法使用）：%s%s。点击重试只会重做这些句子",
				len(failed), businessRetries+1, strings.Join(ids, "、"), more))
	}
	progress(1)
	return nil
}

// translateOne translates sentence idx with its business retries, runs QA
// and, on a QA failure, one retranslation with the reason (SPEC §7.4).
func (s *translateStage) translateOne(ctx context.Context, lec ctxbuild.LectureSnapshot, idx, x int) (translation, error) {
	req := ctxbuild.BuildTranslateRequest(lec, idx, x)
	check := func(a *translateAnswer) error {
		if a.Idx == nil || *a.Idx != req.Idx {
			return fmt.Errorf("answer idx does not match target #%d", req.Idx)
		}
		if a.Zh == nil {
			return errors.New(`answer has no "zh"`)
		}
		return nil
	}
	target := lec.Sentences[idx].Text
	a, err := askJSON(ctx, s.d.LLM, config.RoleTranslate, req.System, req.User, check)
	if err != nil {
		return translation{}, err
	}
	zh := strings.TrimSpace(*a.Zh)
	ok, reason := qa.Check(target, zh, lec.Terms)
	if ok {
		return translation{zh: zh, status: model.ZhOK, hash: req.Hash}, nil
	}
	a2, err := askJSON(ctx, s.d.LLM, config.RoleTranslate, req.System, ctxbuild.TranslateRetryUser(req.User, reason), check)
	if err != nil {
		if isUnusable(err) { // keep the first answer, flagged
			return translation{zh: zh, status: model.ZhQAFailed, qaError: reason, hash: req.Hash}, nil
		}
		return translation{}, err
	}
	zh2 := strings.TrimSpace(*a2.Zh)
	if ok2, reason2 := qa.Check(target, zh2, lec.Terms); !ok2 {
		return translation{zh: zh2, status: model.ZhQAFailed, qaError: reason2, hash: req.Hash}, nil
	}
	return translation{zh: zh2, status: model.ZhOK, hash: req.Hash}, nil
}

// throttle lets an action through at most once per interval.
type throttle struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

func newThrottle(every time.Duration) *throttle { return &throttle{every: every} }

func (t *throttle) ok() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if now.Sub(t.last) < t.every {
		return false
	}
	t.last = now
	return true
}
