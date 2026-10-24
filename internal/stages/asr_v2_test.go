// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// addTierTerm inserts a term with the v2 columns set (pipeline-B helper).
func (e *env) addTierTerm(en, zh, source string, freq int, tier string, common, enabled bool) {
	e.t.Helper()
	e.must(e.st.InsertTerm(e.ctx, model.Term{ID: db.NewID(), CourseID: e.course.ID, En: en, EnKey: db.EnKey(en), Zh: zh,
		Source: source, Enabled: enabled, Freq: freq, Tier: tier, CommonWord: common}))
}

// TestASRPromptUsesBoundMaterials: the whisper prompt (SPEC-v2 G4) holds
// only enabled enforce terms, and the count in the materials bound to this
// lecture — not the course-wide ones, not another lecture's — moves a term
// ahead of a more frequent course term. The prompt stays out of the input
// hash (SPEC-v1.1 A1).
func TestASRPromptUsesBoundMaterials(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	e.addTierTerm("cache", "", model.SourceSyllabus, 20, model.TierEnforce, true, true) // common word: last
	e.addTierTerm("TLB", "", model.SourceMaterial, 9, model.TierEnforce, false, true)
	e.addTierTerm("Belady", "", model.SourceMaterial, 2, model.TierEnforce, false, true)
	e.addTierTerm("write-back", "", model.SourceManual, 0, model.TierEnforce, false, true)
	e.addTierTerm("block", "", model.SourceSyllabus, 30, model.TierReference, false, true) // reference: never prompted
	e.addTierTerm("world", "", model.SourceMaterial, 40, model.TierEnforce, false, false)  // disabled
	l := e.lecture("L1", "m.mp4")
	other := e.lecture("L2", "n.mp4")
	e.material(&l.ID, "replacement.pdf", []string{"Belady's anomaly", "Belady and LRU"})
	e.material(nil, "course.pdf", []string{"TLB TLB TLB"})
	e.material(&other.ID, "other.pdf", []string{"TLB TLB TLB TLB"})
	e.writeWAV(l, "wav")
	d := e.deps(nil)
	stg := e.stage(d, model.StageASR)
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	var job map[string]any
	b, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.must(json.Unmarshal(b, &job))
	if got, want := job["initial_prompt"], "write-back, Belady, TLB, cache"; got != want {
		t.Errorf("initial_prompt %q, want %q", got, want)
	}
	// the fake worker used 3 terms
	if r.stats.ASRPrompt != "write-back, Belady, TLB" {
		t.Errorf("asr_prompt %q", r.stats.ASRPrompt)
	}
	// materials are not part of the asr hash
	h, err := stg.InputHash(e.ctx, l.ID)
	if err != nil || h == "" || r.job.InputHash == nil || h != *r.job.InputHash {
		t.Fatalf("InputHash %q %v, job %v", h, err, r.job.InputHash)
	}
	e.material(&l.ID, "more.pdf", []string{"TLB TLB TLB TLB TLB"})
	if h2, _ := stg.InputHash(e.ctx, l.ID); h2 != h {
		t.Error("a bound material changed the asr input hash")
	}
}
