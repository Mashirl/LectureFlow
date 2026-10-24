// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

func (e *env) chaptersLecture() model.Lecture {
	e.t.Helper()
	l := e.upload("cache.mp4", lt(2026, 10, 20, 9, 0))
	e.insertSample(l.ID) // 14 sentences
	e.setSummary(l.ID, "This lecture introduces the cache hierarchy.")
	e.markDone(model.ScopeLecture, l.ID, model.StageSummary)
	return l
}

func (e *env) runChapters(d Deps, lectureID string) runResult {
	e.t.Helper()
	e.auxJob(model.StageChapters, lectureID)
	return e.run(e.stage(d, model.StageChapters), lectureID)
}

func (e *env) chapters(lectureID string) *model.Chapters {
	e.t.Helper()
	v := e.kv(model.KVChaptersPrefix + lectureID)
	if v == "" {
		return nil
	}
	var c model.Chapters
	e.must(json.Unmarshal([]byte(v), &c))
	return &c
}

// TestChaptersStored is P7: the chapters, the input hash, the time and the
// model are stored in kv; the request is the verbatim prompt with the
// summary and every "[idx] text".
func TestChaptersStored(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.chaptersLecture()
	r := e.runChapters(d, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	c := e.chapters(l.ID)
	if c == nil {
		t.Fatal("no chapters stored")
	}
	if len(c.Chapters) != 3 || c.Chapters[0].StartIdx != 0 || c.Chapters[1].StartIdx != 4 || c.Chapters[2].TitleZh != "相联度与冲突缺失" {
		t.Errorf("chapters %+v", c.Chapters)
	}
	h, err := ChaptersInputHash(e.ctx, d, l.ID)
	e.must(err)
	if c.InputHash != h || c.GeneratedAt == "" || c.Model == "" || c.Provider == "" {
		t.Errorf("record %+v (hash now %s)", c, h)
	}
	reqs := e.fake.Requests(llmtest.RoleChapters)
	if len(reqs) != 1 || !reqs[0].JSONMode {
		t.Fatalf("%d chapters requests", len(reqs))
	}
	ss := e.sentences(l.ID)
	if !strings.HasPrefix(reqs[0].User, "SUMMARY:\nThis lecture introduces the cache hierarchy.\n\nTRANSCRIPT:\n[0] "+ss[0].Text+"\n[1] ") ||
		!strings.HasSuffix(reqs[0].User, "[13] "+ss[13].Text) {
		t.Errorf("request user:\n%s", reqs[0].User)
	}
	if !e.ev.has("chapters", l.ID) {
		t.Error("no SSE change")
	}
}

// TestChaptersInputHashStale is P7: a summary or sentence text change makes
// the stored hash differ ("章节可能已过时"); nothing is regenerated.
func TestChaptersInputHashStale(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.chaptersLecture()
	if r := e.runChapters(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	stored := e.chapters(l.ID).InputHash

	human := "A write-back cache only writes a line to memory on eviction."
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		if err := tx.SetHumanText(e.ctx, l.ID, 2, &human); err != nil {
			return err
		}
		return tx.RecomputeText(e.ctx, l.ID, []int{2})
	}))
	h, err := ChaptersInputHash(e.ctx, d, l.ID)
	e.must(err)
	if h == stored {
		t.Error("sentence edit not detected")
	}
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		if err := tx.SetHumanText(e.ctx, l.ID, 2, nil); err != nil {
			return err
		}
		return tx.RecomputeText(e.ctx, l.ID, []int{2})
	}))
	if h, _ := ChaptersInputHash(e.ctx, d, l.ID); h != stored {
		t.Error("hash differs after the edit was reverted")
	}
	e.setSummary(l.ID, "A new summary.")
	if h, _ := ChaptersInputHash(e.ctx, d, l.ID); h == stored {
		t.Error("summary change not detected")
	}
	if n := e.fake.Count(llmtest.RoleChapters); n != 1 {
		t.Errorf("%d requests: chapters must not regenerate on their own", n)
	}
}

// TestChaptersValidation is P7 “校验失败重试一次，仍失败则提示用户”: the
// second rejection fails the task and keeps the earlier chapters.
func TestChaptersValidation(t *testing.T) {
	bad := map[string]string{
		"too few":        `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}]}`,
		"first not 0":    `{"chapters": [{"start_idx": 1, "title_en": "A", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 9, "title_en": "C", "title_zh": "丙"}]}`,
		"not increasing": `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 5, "title_en": "C", "title_zh": "丙"}]}`,
		"out of range":   `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 14, "title_en": "C", "title_zh": "丙"}]}`,
		"long title":     `{"chapters": [{"start_idx": 0, "title_en": "One Two Three Four Five Six Seven Eight Nine", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 9, "title_en": "C", "title_zh": "丙"}]}`,
		"long zh title":  `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": "一二三四五六七八九十一二三四五六七"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 9, "title_en": "C", "title_zh": "丙"}]}`,
		"empty title":    `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": ""}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 9, "title_en": "C", "title_zh": "丙"}]}`,
		"too many":       `{"chapters": [` + strings.Repeat(`{"start_idx": 0, "title_en": "A", "title_zh": "甲"},`, 12) + `{"start_idx": 0, "title_en": "A", "title_zh": "甲"}]}`,
		"not JSON":       `Here are your chapters.`,
	}
	for name, answer := range bad {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			l := e.chaptersLecture()
			earlier := `{"chapters":[],"input_hash":"old"}`
			e.must(e.st.SetKV(e.ctx, model.KVChaptersPrefix+l.ID, earlier))
			e.fake.Handle(llmtest.RoleChapters, func(string, string) string { return answer })
			r := e.runChapters(d, l.ID)
			if errCode(r.err) != "chapters.invalid_output" {
				t.Fatalf("err %v (code %q)", r.err, errCode(r.err))
			}
			if n := e.fake.Count(llmtest.RoleChapters); n != 2 {
				t.Errorf("%d requests, want 2", n)
			}
			if e.kv(model.KVChaptersPrefix+l.ID) != earlier {
				t.Error("kv changed by a failed run")
			}
			if r.job.Status != model.StatusFailed {
				t.Errorf("job %s", r.job.Status)
			}
		})
	}

	// one bad answer, then a good one
	e := newEnv(t)
	e.organizerRoles()
	l := e.chaptersLecture()
	n := 0
	e.fake.Handle(llmtest.RoleChapters, func(string, string) string {
		n++
		if n == 1 {
			return bad["first not 0"]
		}
		return `{"chapters": [{"start_idx": 0, "title_en": "Intro", "title_zh": "引言"}, {"start_idx": 3, "title_en": "Sets", "title_zh": "组"}, {"start_idx": 9, "title_en": "Misses", "title_zh": "缺失"}]}`
	})
	if r := e.runChapters(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if c := e.chapters(l.ID); c == nil || len(c.Chapters) != 3 || c.Chapters[1].StartIdx != 3 {
		t.Errorf("chapters after the retry: %+v", c)
	}
}

// TestChaptersRetryFeedback: the retry repeats the request (system prompt
// verbatim) with why the answer was rejected, as title and filename_rule do
// (lead ruling after the phase 2 acceptance); an answer that is not JSON
// gets the not-JSON problem.
func TestChaptersRetryFeedback(t *testing.T) {
	for _, c := range []struct {
		name, first, want string
	}{
		{"invalid", `{"chapters": [{"start_idx": 0, "title_en": "A", "title_zh": "甲"}, {"start_idx": 5, "title_en": "B", "title_zh": "乙"}, {"start_idx": 5, "title_en": "C", "title_zh": "丙"}]}`,
			"- start_idx 5 after 5 is not increasing"},
		{"not JSON", `Here are your chapters.`, ctxbuild.NotJSONProblem("")[:20]},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			l := e.chaptersLecture()
			n := 0
			e.fake.Handle(llmtest.RoleChapters, func(string, string) string {
				n++
				if n == 1 {
					return c.first
				}
				return `{"chapters": [{"start_idx": 0, "title_en": "Intro", "title_zh": "引言"}, {"start_idx": 3, "title_en": "Sets", "title_zh": "组"}, {"start_idx": 9, "title_en": "Misses", "title_zh": "缺失"}]}`
			})
			if r := e.runChapters(e.deps(nil), l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			reqs := e.fake.Requests(llmtest.RoleChapters)
			if len(reqs) != 2 {
				t.Fatalf("%d requests", len(reqs))
			}
			if reqs[0].System != ctxbuild.ChaptersSystem || reqs[1].System != reqs[0].System {
				t.Error("the retry changed the system prompt")
			}
			if strings.Contains(reqs[0].User, "REJECTED") {
				t.Errorf("first request has feedback: %q", reqs[0].User)
			}
			fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
			if fb == reqs[1].User {
				t.Fatalf("the retry does not start with the original request: %q", reqs[1].User)
			}
			for _, want := range []string{"YOUR PREVIOUS ANSWER WAS REJECTED", c.want} {
				if !strings.Contains(fb, want) {
					t.Errorf("feedback lacks %q:\n%s", want, fb)
				}
			}
			if ch := e.chapters(l.ID); ch == nil || len(ch.Chapters) != 3 {
				t.Errorf("chapters after the retry: %+v", ch)
			}
		})
	}
}

// TestChaptersTooShortOrNoSummary: no request when chapters cannot be made.
func TestChaptersTooShortOrNoSummary(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.upload("short.mp4", lt(2026, 10, 20, 9, 0))
	e.insertSentences(l.ID, []model.Sentence{
		{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hello.", RepairedText: "Hello.", ZhStatus: model.ZhNone},
		{LectureID: l.ID, Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "Bye.", RepairedText: "Bye.", ZhStatus: model.ZhNone},
	})
	if r := e.runChapters(d, l.ID); errCode(r.err) != "chapters.no_summary" {
		t.Errorf("no summary: %v", r.err)
	}
	e.setSummary(l.ID, "Short.")
	if r := e.runChapters(d, l.ID); errCode(r.err) != "chapters.too_short" {
		t.Errorf("two sentences: %v", r.err)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d requests", n)
	}
}
