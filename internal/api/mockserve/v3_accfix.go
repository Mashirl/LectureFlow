// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Mock scenarios for the V12.2 acceptance fixes (v3 acc-fix-web):
//
//	ob_staged  an install marks each item installed as soon as its bytes are
//	           downloaded (the real manager installs item by item), and
//	           GET /api/asr/devices lists only the devices whose back end is
//	           installed — the server detects devices again after each
//	           install. Onboarding step 6 must show CUDA first, then the
//	           integrated GPU and the NPU once asr-openvino is in.
//	ob_desktop GET /api/app says mode "desktop" (v3_onboarding.go) and the
//	           diagnostics have the red Python / faster-whisper / pypdf rows
//	           of a desktop install without components, with the server's
//	           development hints (the page replaces them with "install in
//	           Settings → Components").

import (
	"net/http"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// installDownloaded marks the items of t whose bytes are all downloaded
// (done bytes of the task, items in order) as installed. Must hold st.mu.
func (st *obState) installDownloaded(t *obTask, done int64) {
	for _, it := range t.items {
		if done < it.Total {
			return
		}
		done -= it.Total
		st.installed[it.ID] = true
	}
}

// obBackendComponent is the component a device's back end comes from.
var obBackendComponent = map[string]string{
	model.BackendFasterWhisper: "asr-cuda",
	model.BackendOpenVINO:      "asr-openvino",
}

// obStagedDevices leaves out, in ob_staged, the devices whose back end is
// not installed yet; other scenarios get the list unchanged.
func (m *mock) obStagedDevices(r *http.Request, list []api.ASRDeviceView) []api.ASRDeviceView {
	if obMode(r) != "staged" {
		return list
	}
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for _, t := range st.tasks {
		st.advance(t, now)
	}
	out := []api.ASRDeviceView{}
	for _, d := range list {
		if id, ok := obBackendComponent[d.Backend]; ok && !st.installed[id] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// obDesktopChecks gives ob_desktop the failing checks of a desktop install
// before any component: the hints are the server's (development) codes.
func obDesktopChecks(r *http.Request, sys *api.System) {
	if obMode(r) != "desktop" {
		return
	}
	python := `C:\Program Files\LectureFlow\resources\.venv\Scripts\python.exe`
	fail := map[string]api.SystemCheck{
		"python": {Name: "python", Status: "error",
			DetailInfo: &model.ErrorInfo{Code: "check.run_failed", Params: map[string]any{"program": python, "error": "exec: file does not exist"}},
			HintInfo:   &model.ErrorInfo{Code: "check.hint_python"}},
		"faster-whisper": {Name: "faster-whisper", Status: "error",
			DetailInfo: &model.ErrorInfo{Code: "check.import_failed", Params: map[string]any{"module": "faster_whisper", "error": "exec: file does not exist"}},
			HintInfo:   &model.ErrorInfo{Code: "check.hint_pip"}},
		"pypdf": {Name: "pypdf", Status: "error",
			DetailInfo: &model.ErrorInfo{Code: "check.import_failed", Params: map[string]any{"module": "pypdf", "error": "exec: file does not exist"}},
			HintInfo:   &model.ErrorInfo{Code: "check.hint_pip"}},
	}
	for i, c := range sys.Checks {
		if f, ok := fail[c.Name]; ok {
			sys.Checks[i] = f
			delete(fail, c.Name)
		}
	}
	for _, name := range []string{"python", "faster-whisper", "pypdf"} {
		if f, ok := fail[name]; ok {
			sys.Checks = append(sys.Checks, f)
		}
	}
}
