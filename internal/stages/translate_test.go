// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// fixtureTerms is the glossary the fake LLM's term fixtures merge into.
func (e *env) fixtureTerms() {
	for _, t := range [][2]string{
		{"cache", "缓存"}, {"write-back", "写回"}, {"cache line", "缓存行"}, {"set associative", "组相联"}, {"LRU", ""},
		{"x86", ""}, {"cache hierarchy", "缓存层次结构"}, {"direct mapped", "直接映射"}, {"miss rate", "缺失率"},
		{"conflict miss", "冲突缺失"}, {"eviction", "驱逐"},
	} {
		e.addTerm(t[0], t[1], model.SourceSyllabus, 1)
	}
}

func (e *env) currentHashes(lectureID string) []string {
	e.t.Helper()
	ld, err := e.deps(nil).loadLecture(e.ctx, lectureID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ctxbuild.TranslateHashes(ld.snap, e.cfg.LLM.Roles["translate"].Window)
}

func TestTranslateSampleAfterRepair(t *testing.T) {
	e := newEnv(t)
	e.fixtureTerms()
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "A lecture about caches.")
	d := e.deps(nil)
	if r := e.run(e.stage(d, model.StageRepair), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	hashes := e.currentHashes(l.ID)
	for i, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh == "" || s.QAError != nil {
			t.Errorf("sentence %d: status %s zh %v qa %v", i, s.ZhStatus, s.Zh, s.QAError)
		}
		if s.ZhHash == nil || *s.ZhHash != hashes[i] {
			t.Errorf("sentence %d: zh_hash is not TranslateHash", i)
		}
	}
	idxs := e.fake.TranslateIdxs()
	sort.Ints(idxs)
	if !slices.Equal(idxs, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}) {
		t.Errorf("translated %v", idxs)
	}
	if r.stats.Requests != 14 || r.progress.last() != 1 {
		t.Errorf("stats %+v progress %v", r.stats, r.progress.last())
	}
	// every request carries the same prefix (cache hits, SPEC §12)
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	for _, q := range reqs {
		if q.System != reqs[0].System || q.Temperature != 0.3 || !q.JSONMode {
			t.Fatal("translate requests differ in prefix or parameters")
		}
	}
	if !e.ev.has("sentences", l.ID) {
		t.Error("no sentences event")
	}
	// an incremental run right after has nothing to do
	e.fake.Reset()
	newPendingTranslate(t, e, l.ID, state.NoteIncremental)
	if r := e.run(e.stage(d, model.StageTranslate), l.ID); r.err != nil || e.fake.Count(llmtest.RoleTranslate) != 0 || r.progress.last() != 1 {
		t.Errorf("empty incremental run: err %v, %d requests", r.err, e.fake.Count(llmtest.RoleTranslate))
	}
}

// answerFn builds translate answers from a per-idx function of the attempt number.
func answerFn(f func(idx, attempt int, user string) string) *scriptLLM {
	var mu sync.Mutex
	attempts := map[int]int{}
	return &scriptLLM{fn: func(_ context.Context, _ int, c llmCall) (string, error) {
		idx := llmtest.TargetIdx(c.User)
		mu.Lock()
		n := attempts[idx]
		attempts[idx]++
		mu.Unlock()
		return f(idx, n, c.User), nil
	}}
}

func zhJSON(idx int, zh string) string { return fmt.Sprintf(`{"idx": %d, "zh": %q}`, idx, zh) }

// good answers for the plain sample texts (no glossary in these tests)
var plainZh = map[int]string{
	0: "好，我们开始今天的课。", 1: "今天我们讲现代处理器中的缓存层次结构。", 2: "写回缓存只在行被驱逐时才把它写回内存。",
	3: "在大多数 x86 机器上，每个缓存行是 64 字节。", 4: "所以第一个问题是我们怎样在缓存里找到一行。",
	5: "我们把地址分成标记、索引和偏移量。", 6: "索引选出一个组，标记告诉我们是否命中。", 7: "在直接映射缓存中，每个组恰好只有一行。",
	8: "当两个地址映射到同一个组时，就会产生冲突缺失。", 9: "两路组相联缓存会把组的数量减半。", 10: "缺失率从 5% 降到大约 3%。",
	11: "Blah lab 是我们下周会在实验课上看到的内容。", 12: "最后，LRU 替换会淘汰最近最少使用的行。", 13: "在继续之前，大家有什么问题吗？",
}

func TestTranslateQARetry(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	var retryUsers sync.Map
	fake := answerFn(func(idx, attempt int, user string) string {
		if attempt > 0 {
			retryUsers.Store(idx, user)
		}
		switch {
		case idx == 3 && attempt == 0:
			return zhJSON(idx, "在大多数机器上，每个缓存行是六十四字节。") // numbers 64, 86 missing
		case idx == 10:
			return zhJSON(idx, "缺失率下降了。") // fails both times
		}
		return zhJSON(idx, plainZh[idx])
	})
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	ss := e.sentences(l.ID)
	if ss[3].ZhStatus != model.ZhOK || *ss[3].Zh != plainZh[3] || ss[3].QAError != nil {
		t.Errorf("sentence 3 (fixed by the retry): %s %q %v", ss[3].ZhStatus, *ss[3].Zh, ss[3].QAError)
	}
	u, _ := retryUsers.Load(3)
	if s, _ := u.(string); !strings.HasSuffix(s, "\n\nYour previous translation was rejected: the numbers 64, 86 from the source sentence are missing. Translate again.") {
		t.Errorf("retry user message:\n%s", s)
	}
	if ss[10].ZhStatus != model.ZhQAFailed || *ss[10].Zh != "缺失率下降了。" || ss[10].QAError == nil ||
		!strings.Contains(*ss[10].QAError, "numbers 5, 3") {
		t.Errorf("sentence 10: %s %v %v", ss[10].ZhStatus, ss[10].Zh, ss[10].QAError)
	}
	if n := len(fake.Calls()); n != 16 { // 14 + one retry each for 3 and 10
		t.Errorf("%d calls", n)
	}
}

func setTr(t *testing.T, e *env, lectureID string, idx int, status, hash string) {
	t.Helper()
	zh := "旧译文。"
	var qe *string
	if status == model.ZhQAFailed {
		s := "old reason"
		qe = &s
	}
	var h *string
	if hash != "" {
		h = &hash
	}
	e.must(e.st.SetTranslation(e.ctx, lectureID, idx, db.SentenceTr{Zh: &zh, ZhStatus: status, ZhHash: h, QAError: qe}))
}

func TestTranslateWhichSentencesRun(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	h := e.currentHashes(l.ID)
	for i := range 14 {
		setTr(t, e, l.ID, i, model.ZhOK, h[i])
	}
	setTr(t, e, l.ID, 1, model.ZhQAFailed, h[1])     // same inputs: not redone (SPEC §4.6)
	setTr(t, e, l.ID, 2, model.ZhQAFailed, "old")    // inputs changed: redone
	setTr(t, e, l.ID, 3, model.ZhStale, h[3])        // stale: redone
	setTr(t, e, l.ID, 6, model.ZhOK, "old")          // ok but inputs changed (§7.3: a different hash is stale): redone
	setTr(t, e, l.ID, 7, model.ZhOK, "")             // ok without a hash: redone
	e.must(e.st.ClearTranslations(e.ctx, "nothing")) // no-op on another lecture
	e.must(e.st.SetTranslation(e.ctx, l.ID, 4, db.SentenceTr{ZhStatus: model.ZhNone}))

	fake := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	stg := e.stage(e.deps(fake), model.StageTranslate)
	if r := e.run(stg, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := targetIdxs(fake); !slices.Equal(got, []int{2, 3, 4, 6, 7}) {
		t.Errorf("run translated %v, want [2 3 4 6 7]", got)
	}
	if s := e.sentences(l.ID)[1]; s.ZhStatus != model.ZhQAFailed || *s.Zh != "旧译文。" {
		t.Errorf("qa_failed sentence with unchanged hash was redone: %s %q", s.ZhStatus, *s.Zh)
	}

	// incremental run after a review that made sentence 9 stale: the
	// qa_failed sentences outside the window (1, 5) are not redone (Gate B).
	setTr(t, e, l.ID, 5, model.ZhQAFailed, h[5])
	setTr(t, e, l.ID, 9, model.ZhStale, h[9])
	newPendingTranslate(t, e, l.ID, state.NoteIncremental)
	fake2 := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	if r := e.run(e.stage(e.deps(fake2), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := targetIdxs(fake2); !slices.Equal(got, []int{9}) {
		t.Errorf("incremental run translated %v, want [9]", got)
	}
	for _, s := range e.sentences(l.ID) {
		want := model.ZhOK
		if s.Idx == 1 || s.Idx == 5 {
			want = model.ZhQAFailed
		}
		if s.ZhStatus != want {
			t.Errorf("sentence %d is %s, want %s", s.Idx, s.ZhStatus, want)
		}
	}
}

// TestTranslateCancelGlossaryChangeRetry (Gate B item 1): a translate run is
// cancelled, the glossary changes, and the retry must redo every sentence —
// including the ones that were ok under the old glossary — so the export
// never mixes translations made with the old and the new glossary.
func TestTranslateCancelGlossaryChangeRetry(t *testing.T) {
	e := newEnv(t)
	tm := e.addTerm("cache", "缓存", model.SourceSyllabus, 3)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	first := &scriptLLM{fn: func(cctx context.Context, n int, c llmCall) (string, error) {
		if n == 3 {
			cancel()
			<-cctx.Done()
			return "", cctx.Err()
		}
		idx := llmtest.TargetIdx(c.User)
		return zhJSON(idx, plainZh[idx]), nil
	}}
	d := e.deps(first)
	d.Concurrency = 1
	if r := e.runCtx(ctx, e.stage(d, model.StageTranslate), l.ID); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err %v", r.err)
	}
	ok := 0
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus == model.ZhOK {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("%d sentences translated before the cancel, want 3", ok)
	}

	tm.Zh = "快取" // the glossary changes between the cancel and the retry
	e.must(e.st.UpdateTerm(e.ctx, tm))
	second := answerFn(func(idx, _ int, _ string) string {
		return zhJSON(idx, strings.ReplaceAll(plainZh[idx], "缓存", "快取"))
	})
	if r := e.run(e.stage(e.deps(second), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := targetIdxs(second); len(got) != 14 {
		t.Errorf("retry translated %v, want all 14 sentences", got)
	}
	h := e.currentHashes(l.ID)
	for i, s := range e.sentences(l.ID) {
		if s.ZhHash == nil || *s.ZhHash != h[i] || s.ZhStatus != model.ZhOK {
			t.Errorf("sentence %d: status %s, hash current=%v", i, s.ZhStatus, s.ZhHash != nil && *s.ZhHash == h[i])
		}
	}
	if r := e.run(e.stage(e.deps(nil), model.StageExport), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	md, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName, e.exportNames(l)[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(md), "缓存") || !strings.Contains(string(md), "快取") {
		t.Errorf("export mixes old and new glossary translations:\n%s", md)
	}
}

// newPendingTranslate replaces the done translate job by a new pending one
// with stats.note and the old job's snapshot (what
// state.StartIncrementalTranslate does, v1.1 A4.2).
func newPendingTranslate(t *testing.T, e *env, lectureID, note string) {
	t.Helper()
	old := e.job(model.ScopeLecture, lectureID, model.StageTranslate)
	e.must(e.st.RetireJob(e.ctx, old.ID))
	e.must(e.st.InsertJob(e.ctx, model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: lectureID, Stage: model.StageTranslate,
		Status: model.StatusPending, IsCurrent: true, CreatedAt: db.Now(), Stats: model.JobStats{Note: note}, ConfigSnapshot: old.ConfigSnapshot}))
}

func targetIdxs(s *scriptLLM) []int {
	var out []int
	for _, c := range s.Calls() {
		out = append(out, llmtest.TargetIdx(c.User))
	}
	sort.Ints(out)
	return slices.Compact(out)
}

func TestTranslateCancelKeepsWritten(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	fake := &scriptLLM{fn: func(cctx context.Context, n int, c llmCall) (string, error) {
		idx := llmtest.TargetIdx(c.User)
		if n == 3 {
			cancel()
			<-cctx.Done()
			return "", cctx.Err()
		}
		return zhJSON(idx, plainZh[idx]), nil
	}}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.runCtx(ctx, e.stage(d, model.StageTranslate), l.ID)
	if !errors.Is(r.err, context.Canceled) || r.job.Status != model.StatusCancelled {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	done := 0
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus == model.ZhOK {
			done++
		}
	}
	if done != 3 {
		t.Errorf("%d sentences kept after cancel, want 3", done)
	}
	// retry: only the 11 remaining sentences
	fake2 := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	if r := e.run(e.stage(e.deps(fake2), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if n := len(targetIdxs(fake2)); n != 11 {
		t.Errorf("retry translated %d sentences, want 11", n)
	}
}

func TestTranslateUnusableSentenceFailsStage(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	fake := answerFn(func(idx, _ int, _ string) string {
		if idx == 2 {
			return zhJSON(idx+1000, "错位") // idx mismatch every time
		}
		return zhJSON(idx, plainZh[idx])
	})
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || sched.IsPermanent(r.err) || !errors.As(r.err, &ce) || ce.Info.Code != CodeTranslateFailed ||
		!paramsMatch(&ce.Info, "n", 1, "ids", "#2", "attempts", 3) || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	ss := e.sentences(l.ID)
	if ss[2].ZhStatus != model.ZhNone {
		t.Errorf("sentence 2 status %s", ss[2].ZhStatus)
	}
	for _, s := range ss {
		if s.Idx != 2 && s.ZhStatus != model.ZhOK {
			t.Errorf("sentence %d status %s", s.Idx, s.ZhStatus)
		}
	}
	// 3 attempts for sentence 2
	n := 0
	for _, c := range fake.Calls() {
		if llmtest.TargetIdx(c.User) == 2 {
			n++
		}
	}
	if n != 3 {
		t.Errorf("sentence 2 asked %d times, want 3", n)
	}
	// retry only redoes sentence 2
	fake2 := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	if r := e.run(e.stage(e.deps(fake2), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := targetIdxs(fake2); !slices.Equal(got, []int{2}) {
		t.Errorf("retry translated %v", got)
	}
}

func TestTranslatePermanentErrorStops(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) {
		return "", sched.Permanent(errors.New("HTTP 401，请检查 API key 或模型名"))
	}}
	d := e.deps(fake)
	d.Concurrency = 2
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if !sched.IsPermanent(r.err) {
		t.Fatalf("err %v", r.err)
	}
	if n := len(fake.Calls()); n > 2 {
		t.Errorf("%d requests after a permanent error", n)
	}
}

func TestTranslateStageInputHash(t *testing.T) {
	e := newEnv(t)
	tm := e.addTerm("cache", "缓存", model.SourceSyllabus, 1)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	stg := e.stage(e.deps(nil), model.StageTranslate)
	h0, _ := stg.InputHash(e.ctx, l.ID)
	human := "Edited."
	e.must(e.st.SetHumanText(e.ctx, l.ID, 3, &human))
	e.must(e.st.RecomputeText(e.ctx, l.ID, []int{3}))
	if h, _ := stg.InputHash(e.ctx, l.ID); h != h0 {
		t.Error("a sentence edit must not change the translate stage hash")
	}
	tm.Zh = "高速缓存"
	e.must(e.st.UpdateTerm(e.ctx, tm))
	if h, _ := stg.InputHash(e.ctx, l.ID); h == h0 {
		t.Error("a glossary change must change the translate stage hash")
	}
}

// TestTranslateIncrementalPrefixGuard (lead ruling after Gate B round 2): an
// incremental run carries the stage hash it was requested under
// ("incremental:<hash>"). Unchanged prefix → only the processing set; changed
// prefix (glossary/summary edited while it queued) → no request at all and a
// permanent error, because §7.3 rule 3 forbids an automatic full retranslation.
func TestTranslateIncrementalPrefixGuard(t *testing.T) {
	e := newEnv(t)
	tm := e.addTerm("cache", "缓存", model.SourceSyllabus, 3)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	h := e.currentHashes(l.ID)
	for i := range 14 {
		setTr(t, e, l.ID, i, model.ZhOK, h[i])
	}
	e.markDone(model.ScopeLecture, l.ID, model.StageTranslate)
	stageHash := func() string {
		t.Helper()
		v, err := e.stage(e.deps(nil), model.StageTranslate).InputHash(e.ctx, l.ID)
		e.must(err)
		return v
	}

	// prefix unchanged: only the stale sentence
	setTr(t, e, l.ID, 5, model.ZhStale, h[5])
	newPendingTranslate(t, e, l.ID, state.NoteIncremental+":"+stageHash())
	fake := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	if r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := targetIdxs(fake); !slices.Equal(got, []int{5}) {
		t.Errorf("incremental run translated %v, want [5]", got)
	}

	// prefix changed while the incremental run waited: nothing is sent
	setTr(t, e, l.ID, 6, model.ZhStale, h[6])
	newPendingTranslate(t, e, l.ID, state.NoteIncremental+":"+stageHash())
	tm.Zh = "快取"
	e.must(e.st.UpdateTerm(e.ctx, tm))
	fake2 := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	r := e.run(e.stage(e.deps(fake2), model.StageTranslate), l.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeTranslatePrefixChanged || detailOf(r.err) != IncrementalPrefixChanged {
		t.Fatalf("err %v (permanent=%v)", r.err, sched.IsPermanent(r.err))
	}
	if n := len(fake2.Calls()); n != 0 {
		t.Errorf("%d translate requests sent after the prefix changed", n)
	}
	if r.job.Status != model.StatusFailed || jobErrorCode(r.job) != CodeTranslatePrefixChanged {
		t.Errorf("job %s error %v", r.job.Status, r.job.Error)
	}
	for _, s := range e.sentences(l.ID) {
		want := model.ZhOK
		if s.Idx == 6 {
			want = model.ZhStale
		}
		if s.ZhStatus != want {
			t.Errorf("sentence %d is %s, want %s (nothing may be retranslated)", s.Idx, s.ZhStatus, want)
		}
	}

	// a plain "incremental" note (no base hash) keeps the normal behaviour
	newPendingTranslate(t, e, l.ID, state.NoteIncremental)
	fake3 := answerFn(func(idx, _ int, _ string) string {
		return zhJSON(idx, strings.ReplaceAll(plainZh[idx], "缓存", "快取"))
	})
	if r := e.run(e.stage(e.deps(fake3), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if n := len(targetIdxs(fake3)); n != 14 {
		t.Errorf("run without a base hash translated %d sentences, want 14 (all hashes changed)", n)
	}
}
