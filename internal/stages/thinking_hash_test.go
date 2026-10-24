// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"slices"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// TestThinkingNotInInputHashes: a role's "深度思考" level (.claude/
// thinking-brief.md, T1) changes how a stage runs, not what it reads, so it
// enters no input hash: the stage hashes of summary, repair and translate,
// the per-sentence translate hashes and the window x are the same for
// snapshots with thinking absent, off, low, high and max. A level change
// therefore never makes a lecture stale; only the rerun decision (A4.3,
// model.SameSnapshot) sees it.
func TestThinkingNotInInputHashes(t *testing.T) {
	e := newEnv(t)
	e.fixtureTerms()
	l := e.lecture("L1", "m.mp4")
	stagesLLM := []string{model.StageSummary, model.StageRepair, model.StageTranslate}
	setThinking := func(level *string) {
		for _, stage := range stagesLLM {
			s := e.llmSnapshot(stage, "")
			s.Params.Thinking = level
			e.setSnapshot(l.ID, stage, model.MarshalSnapshot(s))
		}
	}
	setThinking(nil)
	e.insertSample(l.ID)
	d := e.deps(nil)
	recorded := map[string]string{}
	for _, stage := range stagesLLM {
		r := e.run(e.stage(d, stage), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", stage, r.err)
		}
		if r.job.InputHash == nil {
			t.Fatalf("%s: no input hash", stage)
		}
		recorded[stage] = *r.job.InputHash
	}
	hashes := func() map[string]string {
		out := map[string]string{}
		for _, stage := range stagesLLM {
			h, err := e.stage(d, stage).InputHash(e.ctx, l.ID)
			if err != nil || h == "" {
				t.Fatalf("%s InputHash %q %v", stage, h, err)
			}
			out[stage] = h
		}
		return out
	}
	off := hashes()
	for stage, h := range recorded {
		if off[stage] != h {
			t.Fatalf("%s: InputHash differs from the hash the run recorded", stage)
		}
	}
	zh := e.currentHashes(l.ID)
	x, err := state.TranslateWindow(e.ctx, e.st, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"", model.ThinkingOff, model.ThinkingLow, model.ThinkingHigh, model.ThinkingMax} {
		setThinking(&level)
		for stage, h := range hashes() {
			if h != off[stage] {
				t.Errorf("thinking %q: the %s input hash changed", level, stage)
			}
		}
		if got := e.currentHashes(l.ID); !slices.Equal(got, zh) {
			t.Errorf("thinking %q: the per-sentence translate hashes changed", level)
		}
		if got, err := state.TranslateWindow(e.ctx, e.st, l.ID); err != nil || got != x {
			t.Errorf("thinking %q: window %d %v, want %d", level, got, err, x)
		}
	}
}
