// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

func patchesByIdx(t *testing.T, e *env, lectureID string) map[int]model.Patch {
	t.Helper()
	ps, err := e.st.ListPatches(e.ctx, lectureID, "")
	if err != nil {
		t.Fatal(err)
	}
	m := map[int]model.Patch{}
	for _, p := range ps {
		if _, dup := m[p.SentenceIdx]; dup {
			t.Errorf("two patches for sentence %d", p.SentenceIdx)
		}
		m[p.SentenceIdx] = p
	}
	return m
}

// TestRepairSampleRules runs the fake LLM's repair.json against the sample
// lecture (SPEC §6.4, §14).
func TestRepairSampleRules(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "A lecture about caches.")
	e.addTerm("cache", "缓存", model.SourceSyllabus, 3)
	// the user already decided sentence 2 by hand
	human := "A write-back cache writes lines back on eviction."
	e.must(e.st.SetHumanText(e.ctx, l.ID, 2, &human))
	e.must(e.st.RecomputeText(e.ctx, l.ID, []int{2}))

	r := e.run(e.stage(e.deps(nil), model.StageRepair), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	ps := patchesByIdx(t, e, l.ID)
	type want struct {
		class, review, after string
	}
	wants := map[int]want{
		1:  {model.ClassAuto, model.ReviewNone, "Today we talk about the cache hierarchy in modern processors."},
		2:  {model.ClassAuto, model.ReviewNone, "A write-back cache only writes a line to memory on eviction."}, // human_text wins: review none
		4:  {model.ClassAbsorb, model.ReviewNone, "So the first question is how we find a line in the cache."},  // last patch of the chunk
		9:  {model.ClassDecide, model.ReviewPending, "A four-way set associative cache halves the number of sets."},
		11: {model.ClassDecide, model.ReviewPending, "Belady's optimal replacement algorithm is something we will study in next week's lab session."}, // >50% edit: auto → decide
	}
	if len(ps) != len(wants) {
		t.Errorf("%d patches, want %d (idx 99 must be dropped): %v", len(ps), len(wants), ps)
	}
	for idx, w := range wants {
		p, ok := ps[idx]
		if !ok {
			t.Errorf("no patch for %d", idx)
			continue
		}
		if p.Class != w.class || p.Review != w.review || p.After != w.after || p.Before != ss[idx].ASRText || p.Reason == "" {
			t.Errorf("patch %d: %+v, want %+v", idx, p, w)
		}
	}
	got := e.sentences(l.ID)
	for i, s := range got {
		wantRepaired := s.ASRText
		if w, ok := wants[i]; ok {
			wantRepaired = w.after
		}
		if s.RepairedText != wantRepaired {
			t.Errorf("sentence %d repaired %q, want %q", i, s.RepairedText, wantRepaired)
		}
		wantText := wantRepaired
		if s.HumanText != nil {
			wantText = *s.HumanText
		}
		if s.Text != wantText {
			t.Errorf("sentence %d text %q, want %q", i, s.Text, wantText)
		}
	}
	if got[2].HumanText == nil || *got[2].HumanText != human {
		t.Error("human_text must survive repair")
	}
	if e.fake.Count(llmtest.RoleRepair) != 1 || r.stats.Requests != 1 || r.stats.PromptTokens == 0 {
		t.Errorf("requests %d, stats %+v", e.fake.Count(llmtest.RoleRepair), r.stats)
	}
	if r.stats.Note != "" || r.stats.Warning != "" || r.stats.NoteInfo != nil || r.stats.WarningInfo != nil {
		t.Errorf("note %q warning %q", r.stats.Note, r.stats.Warning)
	}
	if !e.ev.has("patches", l.ID) || !e.ev.has("sentences", l.ID) {
		t.Error("missing events")
	}
}

// synthLecture inserts n sentences "Sentence {i} about the cash." and a summary.
func synthLecture(e *env, n int) model.Lecture {
	l := e.lecture("Synth", "m.mp4")
	ss := make([]model.Sentence, n)
	for i := range ss {
		txt := fmt.Sprintf("Sentence %d about the cash.", i)
		ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: txt, RepairedText: txt, ZhStatus: model.ZhNone}
	}
	e.insertSentences(l.ID, ss)
	e.setSummary(l.ID, "Summary.")
	return l
}

var targetLineRE = regexp.MustCompile(`(?m)^\[(\d+)\] `)

// targetRange returns the first and last idx of the TARGET section.
func targetRange(user string) (int, int) {
	sec := user[strings.Index(user, "TARGET:\n"):]
	sec = sec[:strings.Index(sec, "\n\nCONTEXT (after):")]
	m := targetLineRE.FindAllStringSubmatch(sec, -1)
	first, _ := strconv.Atoi(m[0][1])
	last, _ := strconv.Atoi(m[len(m)-1][1])
	return first, last
}

func fixPatch(idx int, class string) string {
	return fmt.Sprintf(`{"idx": %d, "after": "Sentence %d about the ⟦cache⟧.", "class": %q, "reason": "homophone"}`, idx, idx, class)
}

func TestRepairChunksOverlapAndFailures(t *testing.T) {
	e := newEnv(t)
	l := synthLecture(e, 90) // chunks [0,40) [40,80) [80,90)
	// leftovers of an interrupted earlier attempt must disappear
	e.must(e.st.InsertPatches(e.ctx, []model.Patch{{LectureID: l.ID, SentenceIdx: 7, Before: "x", After: "y", Class: "auto", Review: "none"}}))
	e.must(e.st.SetRepairedText(e.ctx, l.ID, 7, "leftover"))

	fake := &scriptLLM{fn: func(_ context.Context, _ int, c llmCall) (string, error) {
		first, last := targetRange(c.User)
		switch first {
		case 0:
			// 39 is ours; 41 is a context sentence of this chunk → dropped;
			// 3 appears twice → the last wins; 5 is a no-op; 6 has an unknown class;
			// an entry without idx is dropped
			return `{"patches": [` + fixPatch(3, "absorb") + `, ` + fixPatch(39, "auto") + `, ` + fixPatch(41, "auto") + `, ` +
				fixPatch(3, "auto") + `, {"idx": 5, "after": "Sentence 5 about the cash.", "class": "auto", "reason": "same"}, ` +
				fixPatch(6, "maybe") + `, {"after": "Sentence 8 about the cache.", "class": "auto", "reason": "no idx"}]}`, nil
		case 40:
			return "I could not decide, sorry.", nil // unusable three times
		case 80:
			if last != 89 {
				t.Errorf("last chunk target ends at %d", last)
			}
			return "```json\n{\"patches\": [" + fixPatch(85, "decide") + ", " + fixPatch(78, "auto") + "]}\n```", nil
		}
		return "", fmt.Errorf("unexpected chunk %d", first)
	}}
	r := e.run(e.stage(e.deps(fake), model.StageRepair), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	ps := patchesByIdx(t, e, l.ID)
	if len(ps) != 4 {
		t.Fatalf("patches %v", ps)
	}
	for idx, want := range map[int][2]string{3: {"auto", "none"}, 39: {"auto", "none"}, 6: {"decide", "pending"}, 85: {"decide", "pending"}} {
		p := ps[idx]
		if p.Class != want[0] || p.Review != want[1] || p.After != fmt.Sprintf("Sentence %d about the cache.", idx) {
			t.Errorf("patch %d: %+v", idx, p)
		}
	}
	ss := e.sentences(l.ID)
	if ss[7].RepairedText != ss[7].ASRText || ss[41].RepairedText != ss[41].ASRText || ss[78].RepairedText != ss[78].ASRText {
		t.Error("leftover or out-of-chunk repairs applied")
	}
	if ss[85].Text != "Sentence 85 about the cache." {
		t.Errorf("sentence 85 text %q", ss[85].Text)
	}
	// chunk 2 was asked 3 times (2 business retries), the others once
	if n := len(fake.Calls()); n != 5 {
		t.Errorf("%d LLM calls, want 5", n)
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteRepairChunksFailed || !paramsMatch(n, "chunks", "2", "ranges", "41–80", "attempts", 3) ||
		r.job.Status != model.StatusDone {
		t.Errorf("note %+v status %s", n, r.job.Status)
	}
	if w := r.stats.WarningInfo; infoCode(w) != WarnRepairUnrepaired || !paramsMatch(w, "ranges", "41–80", "attempts", 3) || r.stats.Warning == "" {
		t.Errorf("warning %+v", w)
	}
	// every request shares the byte-identical prefix
	calls := fake.Calls()
	for _, c := range calls[1:] {
		if c.System != calls[0].System {
			t.Error("repair prefix differs between chunks")
		}
	}
}

func TestRepairAllChunksFail(t *testing.T) {
	e := newEnv(t)
	l := synthLecture(e, 50)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return `{"oops": true}`, nil }}
	r := e.run(e.stage(e.deps(fake), model.StageRepair), l.ID)
	if r.err == nil || sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed ||
		jobErrorCode(r.job) != CodeRepairAllFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if n, _ := e.st.CountPatches(e.ctx, l.ID, ""); n != 0 {
		t.Errorf("%d patches written", n)
	}
}

func TestRepairAtomicOnCancelAndError(t *testing.T) {
	for _, mode := range []string{"cancel", "permanent", "network"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			l := synthLecture(e, 90)
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			fake := &scriptLLM{fn: func(cctx context.Context, _ int, c llmCall) (string, error) {
				first, _ := targetRange(c.User)
				if first != 80 {
					return `{"patches": [` + fixPatch(first+1, "auto") + `]}`, nil
				}
				switch mode {
				case "cancel":
					time.AfterFunc(20*time.Millisecond, cancel)
					<-cctx.Done()
					return "", cctx.Err()
				case "permanent":
					return "", sched.Permanent(errors.New("HTTP 401，请检查 API key 或模型名"))
				}
				return "", errors.New("网络不可用")
			}}
			d := e.deps(fake)
			d.Concurrency = 1 // chunks in order: the first two succeed before the last one fails
			r := e.runCtx(ctx, e.stage(d, model.StageRepair), l.ID)
			switch mode {
			case "cancel":
				if !errors.Is(r.err, context.Canceled) {
					t.Errorf("err %v", r.err)
				}
			case "permanent":
				if !sched.IsPermanent(r.err) {
					t.Errorf("err %v", r.err)
				}
			default:
				if r.err == nil || sched.IsPermanent(r.err) {
					t.Errorf("err %v", r.err)
				}
			}
			if n, _ := e.st.CountPatches(e.ctx, l.ID, ""); n != 0 {
				t.Errorf("%d patches written although the stage did not finish", n)
			}
			for _, s := range e.sentences(l.ID) {
				if s.RepairedText != s.ASRText {
					t.Fatalf("sentence %d repaired although the stage did not finish", s.Idx)
				}
			}
		})
	}
}

func TestRepairNeedsSummary(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L", "m.mp4")
	e.insertSample(l.ID)
	if r := e.run(e.stage(e.deps(nil), model.StageRepair), l.ID); r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeRepairNoSummary {
		t.Errorf("err %v", r.err)
	}
}
