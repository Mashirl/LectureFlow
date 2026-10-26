// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// "Transcribe only" in the mock (3.1.0, SPEC-v3 V4.2 step 4; the real
// server: internal/api/handlers_english.go, internal/sched transcribe_only):
//
//   - scenario "transcribe_only": CSC1003's "Block Cipher Modes of
//     Operation" (toLectureID) is transcribed (asr done, playable) and its
//     API stages wait for a model provider: summary pending with
//     waiting_for "no_provider", repair / translate / export pending after
//     it; no translation, no review, no export files. Every other lecture is
//     as in the default scenario.
//   - GET /api/lectures/{id}/english/{vtt|txt}: the English subtitles or
//     transcript made from the lecture's sentences (the mock's sentence
//     fixture), as "{number} {title}.en.vtt" / ".en.txt"; 404
//     export.not_found before asr is done, 400 export.bad_name for another
//     format, 404 lecture.not_found.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// toLectureID is the transcribed-only lecture of scenario "transcribe_only".
const toLectureID = "01JD3631000000000000000006"

// waitNoProvider is the scheduler's waiting_for of an API stage while no
// model provider is set up (internal/sched, 3.1.0).
const waitNoProvider = "no_provider"

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/lectures/{id}/english/{format}", m.api(m.englishFile))
	})
}

// setupTranscribeOnly turns toLectureID into a transcribed-only lecture.
func setupTranscribeOnly(s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lectures {
		l := &s.lectures[i]
		if l.ID != toLectureID {
			continue
		}
		for j := range l.Stages {
			st := &l.Stages[j]
			switch st.Stage {
			case model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport:
				resetStage(st, model.StatusPending)
				st.Model = nil
				if st.Stage == model.StageSummary {
					w := waitNoProvider
					st.WaitingFor = &w
				}
			}
		}
		l.Exports = []api.ExportFile{}
		l.Counts.ZhOK, l.Counts.ZhQAFailed, l.Counts.ZhStale, l.Counts.PatchesPending = 0, 0, 0, 0
		delete(s.progress0, l.ID+"/"+model.StageTranslate)
	}
	s.refreshAll()
}

func (m *mock) englishFile(w http.ResponseWriter, r *http.Request, s *store) {
	var suffix, ct string
	switch r.PathValue("format") {
	case "vtt":
		suffix, ct = ".en.vtt", "text/vtt; charset=utf-8"
	case "txt":
		suffix, ct = ".en.txt", "text/plain; charset=utf-8"
	default:
		codedError(w, http.StatusBadRequest, "export.bad_name", nil)
		return
	}
	s.mu.Lock()
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		s.mu.Unlock()
		codedError(w, http.StatusNotFound, "lecture.not_found", nil)
		return
	}
	playable, label, title := l.Playable, l.Label, l.Title
	ss := append([]api.Sentence(nil), s.sentences...)
	s.mu.Unlock()
	if !playable || len(ss) == 0 {
		codedError(w, http.StatusNotFound, "export.not_found", nil)
		return
	}
	var b strings.Builder
	clock := func(ms int64) string {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	if suffix == ".en.vtt" {
		b.WriteString("WEBVTT\n\n")
		for i, x := range ss {
			fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, clock(x.StartMS), clock(x.EndMS), x.Text)
		}
	} else {
		for _, x := range ss {
			fmt.Fprintf(&b, "%s\n", strings.Join(strings.Fields(x.Text), " "))
		}
	}
	name := strings.TrimSpace(label + " " + title + suffix)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	_, _ = w.Write([]byte(b.String()))
}
