// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"slices"
	"sort"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// doneLecture runs the sample lecture through asr…export with the real
// stages (fake ASR with prompt_terms_used = 3, fake LLM) and returns the
// stages as state hashers.
func doneLecture(t *testing.T) (*env, model.Lecture, map[string]state.Hasher) {
	t.Helper()
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	e.addTerm("write-back", "写回", model.SourceManual, 1)
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	e.addTerm("LRU", "", model.SourceMaterial, 4)
	e.addTerm("x86", "", model.SourceMaterial, 2)
	e.addTerm("TLB", "快表", model.SourceMaterial, 1)
	for range 3 { // the runs record glossary_version 3
		if _, err := e.st.BumpGlossaryVersion(e.ctx, e.course.ID); err != nil {
			t.Fatal(err)
		}
	}
	l := e.lecture("L1", "m.mp4")
	e.writeWAV(l, "RIFF")
	d := e.deps(nil)
	e.markDone(model.ScopeLecture, l.ID, model.StagePrep)
	for _, name := range []string{model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		r := e.run(e.stage(d, name), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", name, r.err)
		}
		if r.job.Status != model.StatusDone {
			t.Fatalf("%s: %s", name, r.job.Status)
		}
	}
	hashers := map[string]state.Hasher{}
	for name, s := range ByName(d) {
		hashers[name] = s
	}
	// nothing is stale right after the run
	if stale, err := state.InvalidateCourse(e.ctx, e.st, hashers, e.course.ID); err != nil || len(stale) != 0 {
		t.Fatalf("fresh lecture: stale %v err %v", staleStages(stale), err)
	}
	return e, l, hashers
}

func staleStages(js []model.Job) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.Stage)
	}
	sort.Strings(out)
	return out
}

// TestRealHashersInvalidation is Gate A item 11: the real stage hashers decide
// staleness (SPEC §4.4, §10 step 2). v1.1 A1: a glossary change never makes
// asr stale (the asr hash has no prompt); repair and translate still go
// stale. A4: model and settings changes make nothing stale.
func TestRealHashersInvalidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(e *env)
		stale  []string
	}{
		{"term beyond the used prompt prefix", func(e *env) {
			e.addTerm("MESI", "", model.SourceSyllabus, 0) // sorts after the 3 terms the worker used
		}, []string{model.StageRepair, model.StageTranslate}},
		{"new manual term enters the used prefix", func(e *env) {
			e.addTerm("Belady", "", model.SourceManual, 0) // manual terms come first: the prompt changes
		}, []string{model.StageRepair, model.StageTranslate}},
		{"term zh changed", func(e *env) {
			for _, tm := range e.terms() {
				if tm.En == "cache" {
					tm.Zh = "高速缓存"
					e.must(e.st.UpdateTerm(e.ctx, tm))
				}
			}
		}, []string{model.StageRepair, model.StageTranslate}},
		{"used term disabled", func(e *env) {
			for _, tm := range e.terms() {
				if tm.En == "LRU" {
					tm.Enabled = false
					e.must(e.st.UpdateTerm(e.ctx, tm))
				}
			}
		}, []string{model.StageRepair, model.StageTranslate}},
		{"course-wide slides added", func(e *env) {
			e.material(nil, "slides.pdf", []string{"Caches"})
		}, []string{model.StageSummary}},
		{"freq of an unused term changed", func(e *env) {
			for _, tm := range e.terms() {
				if tm.En == "TLB" {
					e.must(e.st.SetTermFreq(e.ctx, tm.ID, 2)) // still behind cache and LRU
				}
			}
		}, nil},
		{"freq change reorders the used prompt", func(e *env) {
			for _, tm := range e.terms() {
				if tm.En == "TLB" {
					e.must(e.st.SetTermFreq(e.ctx, tm.ID, 99)) // now ahead of cache: a different prompt
				}
			}
		}, nil},
		{"audio changed", func(e *env) {
			ls, err := e.st.ListLectures(e.ctx, e.course.ID)
			e.must(err)
			e.writeWAV(ls[0], "RIFF but different audio")
		}, []string{model.StageASR}},
		{"model and settings changed", func(e *env) {
			// current settings (the legacy config here) point elsewhere …
			e.cfg.ASR.Model, e.cfg.ASR.ComputeType, e.cfg.ASR.Device, e.cfg.ASR.Language = "large-v2", "float16", "cpu", "de"
			for role, r := range e.cfg.LLM.Roles {
				r.Model, r.Window = "another-model", 5
				e.cfg.LLM.Roles[role] = r
			}
			// … and even the done jobs' snapshots name another provider/model
			ls, err := e.st.ListLectures(e.ctx, e.course.ID)
			e.must(err)
			for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
				s, _ := model.ParseLLMSnapshot(e.job(model.ScopeLecture, ls[0].ID, stage).ConfigSnapshot)
				s.ProviderID, s.ProviderName, s.Model = "01JOTHERPROVIDER", "Anthropic", "claude-other"
				e.setSnapshot(ls[0].ID, stage, model.MarshalSnapshot(s))
			}
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, l, hashers := doneLecture(t)
			c.change(e)
			stale, err := state.InvalidateCourse(e.ctx, e.st, hashers, e.course.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := staleStages(stale); !slices.Equal(got, c.stale) {
				t.Errorf("stale %v, want %v", got, c.stale)
			}
			for _, j := range stale {
				if j.ScopeID != l.ID {
					t.Errorf("unexpected scope %s", j.ScopeID)
				}
			}
		})
	}
}

// TestStaleReasonUsesRecordedGlossaryVersion: repair and translate record
// the glossary version they ran with, so a glossary change reported with
// its cause (A2.2) reads "术语表已更新（v3 → v4）"; asr is not stale (A1).
func TestStaleReasonUsesRecordedGlossaryVersion(t *testing.T) {
	e, l, hashers := doneLecture(t)
	for _, stage := range []string{model.StageASR, model.StageRepair, model.StageTranslate} {
		if v := e.job(model.ScopeLecture, l.ID, stage).Stats.GlossaryVersion; v == nil || *v != 3 {
			t.Errorf("%s recorded glossary_version %d, want 3", stage, v)
		}
	}
	e.addTerm("Belady", "", model.SourceManual, 0)
	if _, err := e.st.BumpGlossaryVersion(e.ctx, e.course.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := state.InvalidateCourseWith(e.ctx, e.st, hashers, e.course.ID, state.StaleCause{Kind: state.StaleGlossary})
	if err != nil {
		t.Fatal(err)
	}
	if got := staleStages(stale); !slices.Equal(got, []string{model.StageRepair, model.StageTranslate}) {
		t.Fatalf("stale %v", got)
	}
	for _, stage := range []string{model.StageRepair, model.StageTranslate} {
		j := e.job(model.ScopeLecture, l.ID, stage)
		// v2 P14.3: stored as JSON {"code":"glossary_updated","params":{"from":3,"to":4}}
		// (the text "术语表已更新（v3 → v4）" before phase 2).
		if j.StaleReason == nil {
			t.Errorf("%s: no stale_reason", stage)
			continue
		}
		r := model.ParseStaleReason(*j.StaleReason)
		if r.Code != model.StaleCodeGlossary || r.Params["from"] != float64(3) || r.Params["to"] != float64(4) {
			t.Errorf("%s stale_reason %s", stage, *j.StaleReason)
		}
	}
}
