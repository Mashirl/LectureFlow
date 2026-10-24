// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// TestStagesUseCurrentPrompts: summary, repair and translate always send the
// current (v2) prompts, whatever prompt version their snapshots record
// (lead ruling 10, SPEC-v2 G5.5), and the prompt version never enters an
// input hash.
func TestStagesUseCurrentPrompts(t *testing.T) {
	e := newEnv(t)
	e.fixtureTerms()
	e.addTierTerm("line", "行", model.SourceMaterial, 3, model.TierReference, true, true)
	l := e.lecture("L1", "m.mp4")
	setPromptVersion := func(v int) {
		for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
			s := e.llmSnapshot(stage, "")
			s.PromptVersion = v
			e.setSnapshot(l.ID, stage, model.MarshalSnapshot(s))
		}
	}
	setPromptVersion(1) // a lecture launched before v2
	e.insertSample(l.ID)
	d := e.deps(nil)
	for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		if r := e.run(e.stage(d, stage), l.ID); r.err != nil {
			t.Fatalf("%s: %v", stage, r.err)
		}
	}
	glossary := "GLOSSARY:\n" + ctxbuild.MandatoryTermsHeader + "\n"
	preferred := "\n\n" + ctxbuild.PreferredTermsHeader + "\nline => 行\n\n"
	for _, role := range []string{llmtest.RoleSummary, llmtest.RoleRepair, llmtest.RoleTranslate} {
		reqs := e.fake.Requests(role)
		if len(reqs) == 0 {
			t.Fatalf("no %s request", role)
		}
		for _, q := range reqs {
			if !strings.Contains(q.System, glossary) || !strings.Contains(q.System, preferred) {
				t.Fatalf("%s: the system prompt lacks the two-section glossary:\n%s", role, q.System)
			}
		}
	}
	for _, q := range e.fake.Requests(llmtest.RoleTranslate) {
		if !strings.HasPrefix(q.System, strings.SplitN(ctxbuild.TranslateSystemTemplate, "GLOSSARY:", 2)[0]) {
			t.Fatalf("translate: not the v2 system prompt:\n%s", q.System)
		}
		if !strings.Contains(q.User, "[BEFORE]\n") || !strings.Contains(q.User, "\n[AFTER]\n") || llmtest.TargetIdx(q.User) < 0 {
			t.Fatalf("translate: not the v2 labels:\n%s", q.User)
		}
	}

	// the prompt version is in no hash: v1 and v2 snapshots give the same hashes
	hashes := func() map[string]string {
		out := map[string]string{}
		for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
			h, err := e.stage(d, stage).InputHash(e.ctx, l.ID)
			if err != nil || h == "" {
				t.Fatalf("%s InputHash %q %v", stage, h, err)
			}
			out[stage] = h
		}
		return out
	}
	v1 := hashes()
	before := e.currentHashes(l.ID)
	setPromptVersion(model.PromptVersionTranslate)
	v2 := hashes()
	for stage, h := range v1 {
		if v2[stage] != h {
			t.Errorf("%s: the input hash changed with the prompt version", stage)
		}
	}
	for i, h := range e.currentHashes(l.ID) {
		if h != before[i] {
			t.Errorf("sentence %d: translate hash changed with the prompt version", i)
		}
	}
}
