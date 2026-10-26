// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// GET /api/hardware and POST /api/hardware/detect (SPEC-v3 V3.4, V4.2 step
// 3; .claude/v3-brief.md §7.2): the hardware is detected on the first call
// and cached for the life of the server; "重新检测" detects again. The
// recommendation is recomputed on every call, and its download size comes
// from the component manager's plan (what is already installed is not
// counted).

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/hardware", s.getHardware)
		h("POST /api/hardware/detect", s.detectHardware)
	})
}

// hardwareDetectTimeout bounds one detection run (each probe has its own
// shorter timeout; PowerShell's first start can take 20 s).
const hardwareDetectTimeout = 90 * time.Second

// HardwareRecommendation is hwdetect.Recommendation with the download size
// in bytes from the component plan.
type HardwareRecommendation struct {
	Components    []string          `json:"components"`
	Reasons       []model.ErrorInfo `json:"reasons"`
	Notes         []model.ErrorInfo `json:"notes"`
	DownloadBytes int64             `json:"download_bytes"`
}

// HardwareView is the body of both hardware endpoints.
type HardwareView struct {
	HW             hwdetect.HW            `json:"hw"`
	Recommendation HardwareRecommendation `json:"recommendation"`
	// PlanError (3.1.0, M2) is why the component manager cannot plan the
	// recommendation as it stands (components.unknown,
	// components.unsupported_platform, components.unavailable,
	// components.path_too_long, …); null when it can, or when there is no
	// component manager. With an error, recommendation.download_bytes is
	// only what could be planned (or the recommendation's estimate) and the
	// page shows the error instead of the size.
	PlanError *model.ErrorInfo `json:"plan_error"`
}

func (s *Server) getHardware(w http.ResponseWriter, r *http.Request) {
	s.respondHardware(w, r, false)
}

func (s *Server) detectHardware(w http.ResponseWriter, r *http.Request) {
	s.respondHardware(w, r, true)
}

func (s *Server) respondHardware(w http.ResponseWriter, r *http.Request, fresh bool) {
	hw, err := s.hardware(r.Context(), fresh)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := s.hardwareView(hw)
	if v.PlanError != nil {
		slog.Info("hardware: the recommendation cannot be planned", "code", v.PlanError.Code, "params", v.PlanError.Params)
	}
	writeJSON(w, http.StatusOK, v)
}

// hardware returns the cached detection, detecting when there is none or
// fresh is set. Concurrent callers wait for one run. The run does not end
// with the request (a closed page still fills the cache).
func (s *Server) hardware(ctx context.Context, fresh bool) (hwdetect.HW, error) {
	cs := s.compState()
	cs.hwMu.Lock()
	defer cs.hwMu.Unlock()
	if cs.hw != nil && !fresh {
		return *cs.hw, nil
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hardwareDetectTimeout)
	defer cancel()
	hw, err := cs.detect(dctx, s.dataDir)
	if err != nil {
		return hwdetect.HW{}, err
	}
	if hw.NVIDIA == nil {
		hw.NVIDIA = []hwdetect.GPU{}
	}
	cs.hw = &hw
	return hw, nil
}

func (s *Server) hardwareView(hw hwdetect.HW) HardwareView {
	rec := hwdetect.Recommend(hw)
	v := HardwareView{HW: hw, Recommendation: HardwareRecommendation{
		Components: append([]string{}, rec.Components...),
		Reasons:    append([]model.ErrorInfo{}, rec.Reasons...),
		Notes:      append([]model.ErrorInfo{}, rec.Notes...),
	}}
	v.Recommendation.DownloadBytes = int64(rec.DownloadMB) << 20
	if s.comps == nil || len(rec.Components) == 0 {
		return v
	}
	// The whole recommendation first: this is what "按推荐安装" installs.
	p, err := s.comps.Plan(rec.Components)
	if err == nil {
		v.Recommendation.DownloadBytes = p.DownloadBytes
		return v
	}
	_, info := classify(context.Background(), err, "", nil)
	if info.Code == "" || info.Code == "server.internal" {
		info = model.ErrorInfo{Code: "server.internal", Detail: err.Error()}
	}
	v.PlanError = &info
	// The size of what this platform's manifest has (the recommendation may
	// name a component the manifest lists as unavailable or not at all).
	known := map[string]bool{}
	for _, c := range s.comps.List() {
		if c.Available || c.Installed {
			known[c.ID] = true
		}
	}
	var ids []string
	for _, id := range rec.Components {
		if known[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return v
	}
	if p, err := s.comps.Plan(ids); err == nil {
		v.Recommendation.DownloadBytes = p.DownloadBytes
	}
	return v
}
