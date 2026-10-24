// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

func TestSummaryStage(t *testing.T) {
	e := newEnv(t)
	e.addTerm("cache", "缓存", model.SourceSyllabus, 3)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.material(nil, "slides.pdf", []string{"Caches"})
	stg := e.stage(e.deps(nil), model.StageSummary)

	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleSummary)
	if len(reqs) != 1 {
		t.Fatalf("%d summary requests", len(reqs))
	}
	q := reqs[0]
	// the model of the job's snapshot, taken from the configured role (the
	// default model changed from deepseek-chat to deepseek-flash in SPEC-v2 P12.1)
	if q.JSONMode || q.Temperature != 0.2 || q.Model != e.cfg.LLM.Roles[config.RoleSummary].Model {
		t.Errorf("summary request: json=%v temperature=%v model=%s (SPEC §12: plain text, 0.2)", q.JSONMode, q.Temperature, q.Model)
	}
	if !strings.HasPrefix(q.User, "TRANSCRIPT:\nOkay, let's get started") || !strings.Contains(q.System, "--- page 1 ---\nCaches") ||
		!strings.Contains(q.System, "GLOSSARY:\n"+ctxbuild.MandatoryTermsHeader+"\ncache => 缓存") {
		t.Errorf("summary request content:\n%s\n---\n%s", q.System, q.User)
	}
	sm, err := e.st.GetSummary(e.ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	ld, _ := e.deps(nil).loadLecture(e.ctx, l.ID)
	want := ctxbuild.SummaryInputHash(ld.snap)
	if !strings.HasPrefix(sm.Text, "This lecture introduces the cache hierarchy") || sm.InputHash != want {
		t.Errorf("summary row %+v, want input_hash %s", sm, want)
	}
	if r.job.InputHash == nil || *r.job.InputHash != want {
		t.Errorf("job input_hash %v, want %s", r.job.InputHash, want)
	}
	if r.stats.Requests != 1 || r.stats.PromptTokens == 0 {
		t.Errorf("stats %+v", r.stats)
	}
	if !e.ev.has("lecture", l.ID) {
		t.Error("no lecture event")
	}

	// the role and plain-text mode as seen by the client
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "  A summary.  ", nil }}
	l2 := e.lecture("L2", "m.mp4")
	e.insertSample(l2.ID)
	if r := e.run(e.stage(e.deps(fake), model.StageSummary), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if c := fake.Calls(); len(c) != 1 || c[0].Role != config.RoleSummary || c[0].JSONMode {
		t.Errorf("calls %+v", c)
	}
	if sm, _ := e.st.GetSummary(e.ctx, l2.ID); sm.Text != "A summary." {
		t.Errorf("summary text %q (trimmed)", sm.Text)
	}
}

func TestSummaryEmptyAnswerFails(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.fake.SetResponse(llmtest.RoleSummary, "   \n")
	r := e.run(e.stage(e.deps(nil), model.StageSummary), l.ID)
	if r.err == nil || codeOf(r.err) != CodeSummaryEmpty || jobErrorCode(r.job) != CodeSummaryEmpty || r.job.Status != model.StatusFailed {
		t.Errorf("err %v status %s", r.err, r.job.Status)
	}
	if _, err := e.st.GetSummary(e.ctx, l.ID); err == nil {
		t.Error("an empty summary was stored")
	}

	// no sentences: nothing to summarise
	l2 := e.lecture("L2", "m.mp4")
	if r := e.run(e.stage(e.deps(nil), model.StageSummary), l2.ID); r.err == nil {
		t.Error("summary of a lecture without sentences must fail")
	}
}
