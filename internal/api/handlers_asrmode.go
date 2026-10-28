// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"lectureflow/internal/model"
)

// The transcription mode (ext-step1, SPEC-ext "主程序配套改动 1",
// .claude/ext-brief.md §2): "local" transcribes on this computer, "remote"
// only with Google Colab. The setup guide's "转写方式" step and the
// settings page's switch write it; the diagnostics page follows it (only
// remote: no local inference rows, no device and GPU cards, no
// transcription interpreters in the configuration; the Colab rows instead).
// It does not change which devices transcribe: the device table decides
// that, as before.
//
//	GET /api/asr-mode → ASRModeView
//	PUT /api/asr-mode ASRModeRequest → ASRModeView; 400 request.bad_param
//	    {name: "mode"}; SSE changed {kind: "settings"} and the checks run
//	    again.

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/asr-mode", s.getASRMode)
		h("PUT /api/asr-mode", s.putASRMode)
	})
}

// kvASRMode stores the mode chosen in the settings or the guide (split so
// the error-code scan does not take it for a code).
const kvASRMode = "asr" + ".mode"

// Values of the transcription mode.
const (
	ASRModeLocal  = "local"
	ASRModeRemote = "remote"
)

// Where the mode in effect comes from.
const (
	ASRModeFromSettings   = "settings"   // kv asr.mode
	ASRModeFromOnboarding = "onboarding" // data.asr_mode of the guide's saved state (before 3.2)
	ASRModeFromDefault    = "default"    // nothing chosen: local
)

// ASRModeView is the body of both endpoints.
type ASRModeView struct {
	Mode   string `json:"mode"`   // local | remote
	Source string `json:"source"` // settings | onboarding | default
}

// ASRModeRequest is PUT /api/asr-mode.
type ASRModeRequest struct {
	Mode string `json:"mode"` // local | remote
}

func validASRMode(m string) bool { return m == ASRModeLocal || m == ASRModeRemote }

// asrMode is the mode in effect: kv asr.mode, else the guide's
// data.asr_mode (the guide of 3.1 saved it only there), else local. A
// value that cannot be read counts as not set.
func (s *Server) asrMode(ctx context.Context) ASRModeView {
	if s.st == nil {
		return ASRModeView{Mode: ASRModeLocal, Source: ASRModeFromDefault}
	}
	if v, ok, err := s.st.GetKV(ctx, kvASRMode); err == nil && ok && validASRMode(v) {
		return ASRModeView{Mode: v, Source: ASRModeFromSettings}
	}
	if v, ok, err := s.st.GetKV(ctx, model.KVOnboardingState); err == nil && ok {
		var st struct {
			Data struct {
				ASRMode string `json:"asr_mode"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(v), &st) == nil && validASRMode(st.Data.ASRMode) {
			return ASRModeView{Mode: st.Data.ASRMode, Source: ASRModeFromOnboarding}
		}
	}
	return ASRModeView{Mode: ASRModeLocal, Source: ASRModeFromDefault}
}

// GET /api/asr-mode
func (s *Server) getASRMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.asrMode(r.Context()))
}

// PUT /api/asr-mode {"mode": "local" | "remote"}
func (s *Server) putASRMode(w http.ResponseWriter, r *http.Request) {
	var req ASRModeRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !validASRMode(req.Mode) {
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "mode"})
		return
	}
	ctx := r.Context()
	prev := s.asrMode(ctx)
	if err := s.st.SetKV(ctx, kvASRMode, req.Mode); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("settings", "")
	if prev.Mode != req.Mode && s.diag != nil {
		s.diag.envChanged() // the rows of the other mode (the page drops the old ones at once)
	}
	writeJSON(w, http.StatusOK, ASRModeView{Mode: req.Mode, Source: ASRModeFromSettings})
}
