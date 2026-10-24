// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"slices"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// TestTierInvalidation: with the real hashers, moving a term between tiers
// makes repair and translate stale (SPEC-v2 G3.1: a janitor run makes them
// stale), while the other v2 term columns and the prompt version recorded in
// the snapshots make nothing stale (lead ruling 9, SPEC-v2 G5.5).
func TestTierInvalidation(t *testing.T) {
	setTerm := func(e *env, en string, f func(*model.Term)) {
		for _, tm := range e.terms() {
			if tm.En == en {
				f(&tm)
				e.must(e.st.UpdateTerm(e.ctx, tm))
				return
			}
		}
		e.t.Fatalf("no term %q", en)
	}
	cases := []struct {
		name   string
		change func(e *env)
		stale  []string
	}{
		{"term moved to reference", func(e *env) {
			setTerm(e, "cache", func(tm *model.Term) { tm.Tier = model.TierReference })
		}, []string{model.StageRepair, model.StageTranslate}},
		{"English-only term moved to reference", func(e *env) {
			setTerm(e, "LRU", func(tm *model.Term) { tm.Tier = model.TierReference })
		}, []string{model.StageRepair, model.StageTranslate}},
		{"lock, common word, note and zh source", func(e *env) {
			setTerm(e, "cache", func(tm *model.Term) {
				tm.Locked, tm.CommonWord, tm.JanitorNote, tm.ZhSource = true, true, "Common word.", model.ZhSourceUser
			})
		}, nil},
		{"prompt version of the snapshots", func(e *env) {
			ls, err := e.st.ListLectures(e.ctx, e.course.ID)
			e.must(err)
			for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
				s, _ := model.ParseLLMSnapshot(e.job(model.ScopeLecture, ls[0].ID, stage).ConfigSnapshot)
				s.PromptVersion = 1
				e.setSnapshot(ls[0].ID, stage, model.MarshalSnapshot(s))
			}
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, hashers := doneLecture(t)
			c.change(e)
			stale, err := state.InvalidateCourse(e.ctx, e.st, hashers, e.course.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := staleStages(stale); !slices.Equal(got, c.stale) {
				t.Errorf("stale %v, want %v", got, c.stale)
			}
		})
	}
	// a term moved to reference and back: nothing stale
	t.Run("moved back to enforce", func(t *testing.T) {
		e, _, hashers := doneLecture(t)
		setTerm(e, "cache", func(tm *model.Term) { tm.Tier = model.TierReference })
		setTerm(e, "cache", func(tm *model.Term) { tm.Tier = model.TierEnforce })
		if stale, err := state.InvalidateCourse(e.ctx, e.st, hashers, e.course.ID); err != nil || len(stale) != 0 {
			t.Errorf("stale %v err %v", staleStages(stale), err)
		}
	})
}
