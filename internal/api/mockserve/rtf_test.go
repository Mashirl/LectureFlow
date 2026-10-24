// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"math"
	"net/http"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// The mock's asr_timing examples (phase 3b hover card): W01L1 of CSC1003 on
// the NVIDIA GPU, P1 on the Intel GPU with its first compile; no other stage
// has one, and a rerun drops it (the real asr stage clears it per attempt).
func TestMockASRTiming(t *testing.T) {
	srv := testServer(t)
	cases := map[string]struct {
		label string
		want  model.ASRTiming
		rtf   float64
	}{
		lecID(c3631, 1): {asrLabelOf(labelCUDA, "large-v2", model.PrecisionAuto, "en"),
			model.ASRTiming{LoadMS: 3800, TranscribeMS: 209040, AudioMS: 3120000, PeakMemMB: 2140}, 0.067},
		lecID(c3631, 11): {asrLabelOf(labelIGPU, "large-v3-turbo", model.PrecisionFP16, "en"),
			model.ASRTiming{LoadMS: 7600, TranscribeMS: 114240, AudioMS: 3360000, FirstCompile: true, PeakMemMB: 2710}, 0.034},
	}
	var lectures []api.Lecture
	doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(c3631)+"/lectures", nil, http.StatusOK, &lectures)
	seen := 0
	for _, l := range lectures {
		c, ok := cases[l.ID]
		for _, st := range l.Stages {
			if st.Stage != model.StageASR || !ok {
				if st.ASRTiming != nil {
					t.Errorf("%s %s: asr_timing %+v", l.Label, st.Stage, st.ASRTiming)
				}
				continue
			}
			seen++
			if st.Status != model.StatusDone || st.ASRTiming == nil || *st.ASRTiming != c.want {
				t.Errorf("%s: %s asr_timing %+v, want %+v", l.Label, st.Status, st.ASRTiming, c.want)
				continue
			}
			if st.Model == nil || st.Model.Label != c.label {
				t.Errorf("%s: asr model %+v, want %s", l.Label, st.Model, c.label)
			}
			// consistent with the lecture: the audio is the media length, the
			// RTF the intended one, the stage at least load + transcription
			tm := st.ASRTiming
			if l.DurationMS == nil || *l.DurationMS != tm.AudioMS {
				t.Errorf("%s: duration_ms %v, audio_ms %d", l.Label, l.DurationMS, tm.AudioMS)
			}
			if rtf := float64(tm.TranscribeMS) / float64(tm.AudioMS); math.Abs(rtf-c.rtf) > 1e-9 {
				t.Errorf("%s: RTF %v, want %v", l.Label, rtf, c.rtf)
			}
			if st.DurationMS == nil || *st.DurationMS < tm.LoadMS+tm.TranscribeMS {
				t.Errorf("%s: stage duration %v shorter than load + transcription", l.Label, st.DurationMS)
			}
		}
	}
	if seen != len(cases) {
		t.Fatalf("found %d of the %d timed lectures", seen, len(cases))
	}
	// other courses have none
	for _, cid := range []string{c2120, c3121, c3311} {
		var ls []api.Lecture
		doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(cid)+"/lectures", nil, http.StatusOK, &ls)
		for _, l := range ls {
			for _, st := range l.Stages {
				if st.ASRTiming != nil {
					t.Errorf("%s %s %s: asr_timing %+v", cid, l.Label, st.Stage, st.ASRTiming)
				}
			}
		}
	}

	// a rerun of the asr stage starts a new attempt without a timing
	id := lecID(c3631, 11)
	var l api.Lecture
	doJSON(t, "POST", srv.URL+"/api/lectures/"+id+"/stages/asr/rerun", api.ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, http.StatusOK, &l)
	doJSON(t, "GET", srv.URL+"/api/lectures/"+id, nil, http.StatusOK, &l)
	for _, st := range l.Stages {
		if st.ASRTiming != nil {
			t.Errorf("after the rerun: %s %s asr_timing %+v", st.Stage, st.Status, st.ASRTiming)
		}
	}
	if st := l.Stages[stageIndex(&l, model.StageASR)]; st.Status != model.StatusQueued {
		t.Errorf("after the rerun: asr %s", st.Status)
	}
}
