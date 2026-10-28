// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// "修复断句" (textrepair-int): the asr rerun with text_only keeps the
// sentences (and their human_text) for the asr job to repair, cleans up
// everything after asr as a summary rerun does, and gives the new asr job
// the mode and what describes the transcription it repairs.

func TestRerunTextRepairKeepsTheSentences(t *testing.T) {
	f := newFixture(t)
	l := f.fullLecture()
	ctx := f.ctx
	old := f.lj(l.ID, model.StageASR)
	gv := 4
	stats := model.JobStats{PromptTermsUsed: 12, ASRPrompt: "XOR, AES", GlossaryVersion: &gv, HashVersion: 3,
		ASRTiming:  &model.ASRTiming{LoadMS: 3000, TranscribeMS: 190000, AudioMS: 3300000},
		DurationMS: 200000, PowerInterrupts: 1}
	must(t, f.st.SetJobStats(ctx, old.ID, stats))
	snaps := map[string]string{}
	for _, s := range model.LectureStages {
		snaps[s] = f.lj(l.ID, s).ConfigSnapshot
	}
	if err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, RerunOpts{TextRepair: true}); err != nil {
		t.Fatal(err)
	}
	// the sentences stay; everything after asr is cleaned up as by a summary rerun
	n, _ := f.st.CountSentences(ctx, l.ID)
	if n != 3 {
		t.Fatalf("%d sentences, want the 3 kept", n)
	}
	s0, _ := f.st.GetSentence(ctx, l.ID, 0)
	s1, _ := f.st.GetSentence(ctx, l.ID, 1)
	if s0.RepairedText != s0.ASRText || s0.Text != s0.ASRText {
		t.Errorf("repaired_text not reset / text not recomputed: %+v", s0)
	}
	if s0.Zh != nil || s0.ZhStatus != model.ZhNone || s0.ZhHash != nil {
		t.Errorf("translations not cleared: %+v", s0)
	}
	if s1.HumanText == nil || *s1.HumanText != "a miss penalty!" || s1.Text != "a miss penalty!" {
		t.Errorf("human_text not kept: %+v", s1)
	}
	if np, _ := f.st.CountPatches(ctx, l.ID, ""); np != 0 {
		t.Errorf("%d patches left (the review's accept / reject records go)", np)
	}
	if _, err := f.st.GetSummary(ctx, l.ID); err == nil {
		t.Error("summary left")
	}
	dir := model.LectureDir(f.dataDir, l.CourseID, l.ID)
	if !exists(filepath.Join(dir, model.AudioFile)) {
		t.Error("audio.wav removed")
	}
	if exists(filepath.Join(dir, model.ExportDirName)) {
		t.Error("export dir kept")
	}
	// asr and every stage after it are new pending jobs with their snapshots
	for _, s := range model.LectureStages {
		j := f.lj(l.ID, s)
		want := model.StatusPending
		if s == model.StagePrep {
			want = model.StatusDone
		}
		if j.Status != want {
			t.Errorf("%s = %s, want %s", s, j.Status, want)
		}
		if s != model.StagePrep && j.ConfigSnapshot != withPromptVersion(s, snapshotOf(&model.Job{ConfigSnapshot: snaps[s]}), model.PromptVersionOf(s)) {
			t.Errorf("%s snapshot %q, was %q", s, j.ConfigSnapshot, snaps[s])
		}
	}
	nj := f.lj(l.ID, model.StageASR)
	if nj.ID == old.ID {
		t.Fatal("the asr job was not replaced")
	}
	want := model.JobStats{ASRMode: model.ASRModeTextRepair, PromptTermsUsed: 12, ASRPrompt: "XOR, AES",
		GlossaryVersion: &gv, HashVersion: 3, ASRTiming: stats.ASRTiming}
	if got := nj.Stats; got.ASRMode != want.ASRMode || got.PromptTermsUsed != 12 || got.ASRPrompt != want.ASRPrompt ||
		got.GlossaryVersion == nil || *got.GlossaryVersion != 4 || got.HashVersion != 3 ||
		got.ASRTiming == nil || *got.ASRTiming != *stats.ASRTiming || got.DurationMS != 0 || got.PowerInterrupts != 0 {
		t.Errorf("new asr job stats %+v, want %+v", got, want)
	}
	if !model.IsTextRepairJob(nj) {
		t.Error("IsTextRepairJob false for the new job")
	}
	// downstream jobs start with empty stats as after any rerun
	if st := f.lj(l.ID, model.StageSummary).Stats; st.ASRMode != "" {
		t.Errorf("summary stats %+v", st)
	}
	// its retry keeps the mode (same job, attempt + 1)
	f.drive(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if err := Retry(ctx, f.st, model.ScopeLecture, l.ID, model.StageASR); err != nil {
		t.Fatal(err)
	}
	if j := f.lj(l.ID, model.StageASR); j.ID != nj.ID || j.Stats.ASRMode != model.ASRModeTextRepair {
		t.Errorf("retry: %+v", j)
	}
}

func TestRerunTextRepairGuards(t *testing.T) {
	f := newFixture(t)
	l := f.fullLecture()
	ctx := f.ctx
	for _, stage := range []string{model.StagePrep, model.StageSummary, model.StageTranslate} {
		err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, stage, RerunOpts{TextRepair: true})
		if !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: %v, want ErrBadInput", stage, err)
		}
	}
	err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR,
		RerunOpts{TextRepair: true, NewSnapshots: map[string]string{model.StageASR: `{"model":"large-v2","language":"en"}`}})
	if !errors.Is(err, ErrBadInput) {
		t.Errorf("asr snapshot: %v, want ErrBadInput", err)
	}
	// nothing changed by the refusals
	if n, _ := f.st.CountPatches(ctx, l.ID, ""); n != 2 {
		t.Errorf("patches %d", n)
	}
	// the preconditions of a rerun: a running downstream stage
	if err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageExport, RerunOpts{}); err != nil {
		t.Fatal(err)
	}
	f.drive(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning)
	err = RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageASR, RerunOpts{TextRepair: true})
	if !errors.Is(err, ErrBusy) {
		t.Errorf("export running: %v, want ErrBusy", err)
	}
	if n, _ := f.st.CountPatches(ctx, l.ID, ""); n != 2 {
		t.Errorf("patches %d after a refused rerun", n)
	}
	// a draft
	d := f.draft()
	if err := RerunWith(ctx, f.st, f.dataDir, model.ScopeLecture, d.ID, model.StageASR, RerunOpts{TextRepair: true}); !errors.Is(err, ErrDraft) {
		t.Errorf("draft: %v", err)
	}
}

func TestWithTextRepair(t *testing.T) {
	asr := model.Job{Scope: model.ScopeLecture, Stage: model.StageASR, Status: model.StatusDone}
	rerun := []string{model.ActionRerun}
	cases := []struct {
		name  string
		j     model.Job
		acts  []string
		offer bool
		want  []string
	}{
		{"offered", asr, rerun, true, []string{model.ActionRerun, model.ActionRerunTextOnly}},
		{"stale", func() model.Job { j := asr; j.Status = model.StatusStale; return j }(), rerun, true,
			[]string{model.ActionRerun, model.ActionRerunTextOnly}},
		{"not worth it", asr, rerun, false, rerun},
		{"downstream running (no rerun)", asr, []string{}, true, []string{}},
		{"failed", func() model.Job { j := asr; j.Status = model.StatusFailed; return j }(), []string{model.ActionRetry}, true,
			[]string{model.ActionRetry}},
		{"another stage", func() model.Job { j := asr; j.Stage = model.StageSummary; return j }(), rerun, true, rerun},
		{"once", asr, []string{model.ActionRerun, model.ActionRerunTextOnly}, true, []string{model.ActionRerun, model.ActionRerunTextOnly}},
	}
	for _, c := range cases {
		in := slices.Clone(c.acts)
		got := WithTextRepair(c.j, c.acts, c.offer)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if !slices.Equal(c.acts, in) {
			t.Errorf("%s: the input was changed", c.name)
		}
	}
	// AllowedActions itself is unchanged: the offer is the caller's
	if got := AllowedActions(asr, ActionOpts{}); !slices.Equal(got, rerun) {
		t.Errorf("AllowedActions = %v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
