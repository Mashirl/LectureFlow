// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// The asr stage view carries job.stats.asr_timing (phase 3b: the hover
// card's transcription time, audio length and RTF); no other stage does,
// and a stage without a timing has no asr_timing key at all.
func TestStageViewASRTiming(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	ctx := context.Background()
	get := func() (Lecture, []byte) {
		t.Helper()
		var lv Lecture
		e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		_, raw := e.do("GET", "/api/lectures/"+l.ID, nil)
		return lv, raw
	}

	lv, raw := get()
	for _, st := range lv.Stages {
		if st.ASRTiming != nil {
			t.Errorf("%s: asr_timing %+v before any run", st.Stage, st.ASRTiming)
		}
	}
	if bytes.Contains(raw, []byte(`"asr_timing"`)) {
		t.Errorf("asr_timing present before any run: %s", raw)
	}

	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	j := e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning)
	// running: no timing yet (the stage clears it when an attempt starts)
	lv, _ = get()
	if st := stageOf(lv.Stages, model.StageASR); st.ASRTiming != nil {
		t.Errorf("running asr: asr_timing %+v", st.ASRTiming)
	}
	want := model.ASRTiming{LoadMS: 238400, TranscribeMS: 160200, AudioMS: 3780000, FirstCompile: true, PeakMemMB: 2683}
	if err := e.st.SetJobStats(ctx, j.ID, model.JobStats{ASRTiming: &want, DurationMS: 401000}); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusDone)

	// another stage whose stats (wrongly) carry a timing does not show it
	sum, err := e.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageSummary)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetJobStats(ctx, sum.ID, model.JobStats{ASRTiming: &model.ASRTiming{LoadMS: 1, TranscribeMS: 2, AudioMS: 3}}); err != nil {
		t.Fatal(err)
	}

	lv, raw = get()
	for _, st := range lv.Stages {
		switch {
		case st.Stage == model.StageASR:
			if st.ASRTiming == nil || *st.ASRTiming != want {
				t.Errorf("asr: asr_timing %+v, want %+v", st.ASRTiming, want)
			}
			if st.DurationMS == nil || *st.DurationMS != 401000 {
				t.Errorf("asr: duration_ms %v", st.DurationMS)
			}
		case st.ASRTiming != nil:
			t.Errorf("%s: asr_timing %+v", st.Stage, st.ASRTiming)
		}
	}
	wantJSON := []byte(`"asr_timing":{"load_ms":238400,"transcribe_ms":160200,"audio_ms":3780000,"first_compile":true,"peak_mem_mb":2683}`)
	if !bytes.Contains(raw, wantJSON) || bytes.Count(raw, []byte(`"asr_timing"`)) != 1 {
		t.Errorf("lecture JSON: want one %s, got %s", wantJSON, raw)
	}
	// the course's lecture list shows the same stage view
	if _, list := e.do("GET", "/api/courses/"+c.ID+"/lectures", nil); !bytes.Contains(list, wantJSON) {
		t.Errorf("lecture list: no %s in %s", wantJSON, list)
	}

	// a stale asr keeps the timing of its run (it is still that run's output)
	if err := state.TransitionJob(ctx, e.st, j.ID, model.StatusDone, model.StatusStale, db.JobFinish{}); err != nil {
		t.Fatal(err)
	}
	lv, _ = get()
	if st := stageOf(lv.Stages, model.StageASR); st.Status != model.StatusStale || st.ASRTiming == nil || *st.ASRTiming != want {
		t.Errorf("stale asr: status %s asr_timing %+v", st.Status, st.ASRTiming)
	}

	// a run without first compile omits the flag (omitempty of the model type)
	plain := model.ASRTiming{LoadMS: 6200, TranscribeMS: 106000, AudioMS: 3120000}
	if err := e.st.SetJobStats(ctx, j.ID, model.JobStats{ASRTiming: &plain}); err != nil {
		t.Fatal(err)
	}
	lv, raw = get()
	if st := stageOf(lv.Stages, model.StageASR); st.ASRTiming == nil || *st.ASRTiming != plain {
		t.Errorf("asr_timing %+v, want %+v", st.ASRTiming, plain)
	}
	if !bytes.Contains(raw, []byte(`"asr_timing":{"load_ms":6200,"transcribe_ms":106000,"audio_ms":3120000}`)) {
		t.Errorf("lecture JSON without first_compile: %s", raw)
	}
}
