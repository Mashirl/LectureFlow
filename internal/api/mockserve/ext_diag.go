// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// ext-step1 (SPEC-ext "主程序配套改动 1. 诊断页按实际硬件显示"; the real API:
// internal/api/system_env.go, handlers_asrmode.go): the diagnostics by
// hardware and transcription mode.
//
//	diag_intel  a pure Intel laptop on AC power: no NVIDIA card; the
//	            OpenVINO rows and the CPU inference test (devices.go
//	            withDeviceChecks, as cpuonly and power_ac_intel)
//	diag_mac    an Apple silicon Mac, the desktop application with MLX and
//	            its model: the MLX rows and the MLX inference test
//	            (testdata/api/system_mac.json)
//	diag_cloud  only Colab: no local inference row, no device or GPU card,
//	            no transcription interpreter; gcloud, the Colab
//	            authorization and the reachability
//	            (testdata/api/system_remote.json)
//
// GET / PUT /api/asr-mode keep the transcription mode per scenario
// (diag_cloud starts "remote" from the guide, the others "local" by
// default); "remote" in any scenario shows the diagnostics of diag_cloud.

import (
	"net/http"
	"sync"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/asr-mode", m.api(m.getASRMode))
		mux.HandleFunc("PUT /api/asr-mode", m.api(m.putASRMode))
	})
}

// diagSetups are the stores of the ext-step1 scenarios.
var diagSetups = map[string]func(*store){
	"diag_intel": setupDiagIntel,
	"diag_mac":   setupDiagMac,
	"diag_cloud": setupDiagCloud,
}

// diagState is a mock's ext-step1 state (main.go's mock has no field for
// it: this file only adds).
type diagState struct {
	mu     sync.Mutex
	stores map[string]*lazyStore
	modes  map[string]string // scenario → the mode a PUT set
}

var diagStates sync.Map // *mock → *diagState

func (m *mock) diagState() *diagState {
	v, _ := diagStates.LoadOrStore(m, &diagState{stores: map[string]*lazyStore{}, modes: map[string]string{}})
	return v.(*diagState)
}

// diagStore is the store of an ext-step1 scenario; false for the others.
func (m *mock) diagStore(r *http.Request) (*store, bool) {
	sc := scenario(r)
	setup, ok := diagSetups[sc]
	if !ok {
		return nil, false
	}
	st := m.diagState()
	st.mu.Lock()
	l := st.stores[sc]
	if l == nil {
		l = &lazyStore{}
		st.stores[sc] = l
	}
	st.mu.Unlock()
	return l.get(m, setup), true
}

// setupDiagIntel: a Core Ultra laptop on AC power without an NVIDIA card.
func setupDiagIntel(s *store) {
	s.dev.noCUDA = true
	s.dev.power = powerOf(false, true, false, intPtr(100))
	s.dev.sys = sysReadings{cpuBase: 22, tempC: nil}
}

// setupDiagMac: an Apple M4 MacBook on AC power with MLX (mlx:gpu only).
func setupDiagMac(s *store) {
	s.dev.noCUDA, s.dev.noOV = true, true
	s.dev.power = powerOf(false, true, true, intPtr(92))
	s.dev.sys = sysReadings{cpuBase: 14, tempC: nil}
	at := s.dev.devs[0].row.DetectedAt
	s.dev.devs = []*mockDevice{{row: model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX, Label: cfgMacGPU, Enabled: true,
		Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 250, DetectedAt: at}, present: true, migrated: true}}
}

// setupDiagCloud: only Colab — no local transcription device.
func setupDiagCloud(s *store) {
	s.dev.noCUDA, s.dev.noOV = true, true
	s.dev.devs = nil
	s.dev.power = powerOf(false, true, false, intPtr(100))
	s.dev.sys = sysReadings{cpuBase: 9, tempC: nil}
}

// asrMode is the scenario's transcription mode.
func (m *mock) asrMode(r *http.Request) api.ASRModeView {
	sc := scenario(r)
	st := m.diagState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if v, ok := st.modes[sc]; ok {
		return api.ASRModeView{Mode: v, Source: api.ASRModeFromSettings}
	}
	if sc == "diag_cloud" {
		return api.ASRModeView{Mode: api.ASRModeRemote, Source: api.ASRModeFromOnboarding}
	}
	return api.ASRModeView{Mode: api.ASRModeLocal, Source: api.ASRModeFromDefault}
}

func (m *mock) getASRMode(w http.ResponseWriter, r *http.Request, _ *store) {
	writeJSON(w, http.StatusOK, m.asrMode(r))
}

func (m *mock) putASRMode(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.ASRModeRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Mode != api.ASRModeLocal && in.Mode != api.ASRModeRemote {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "request.bad_param", Params: map[string]any{"name": "mode"}}})
		return
	}
	st := m.diagState()
	st.mu.Lock()
	st.modes[scenario(r)] = in.Mode
	st.mu.Unlock()
	s.mu.Lock()
	s.notify(api.ChangedEvent{Kind: "settings"})
	s.notify(api.ChangedEvent{Kind: "system"})
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, api.ASRModeView{Mode: in.Mode, Source: api.ASRModeFromSettings})
}

// diagSystem gives the diagnostics of the Mac and of "only Colab" (the
// other environments come from the store's devices: withDeviceChecks,
// mockConfigView). Called with s.mu held, after the devices and the
// configuration are in sys.
func (m *mock) diagSystem(r *http.Request, s *store, sys *api.System) {
	if s.dev.cpuOnly || s.dev.noCUDA { // no NVIDIA card: nvidia-smi is not read
		sys.GPU = api.GPUInfo{Paused: sys.GPU.Paused}
	}
	if s.dev.cpuOnly { // no Intel device and no OpenVINO environment: no OpenVINO block
		sys.OpenVINO = nil
	}
	var name string
	switch {
	case m.asrMode(r).Mode == api.ASRModeRemote:
		name = "system_remote.json"
	case scenario(r) == "diag_mac":
		name = "system_mac.json"
	default:
		return
	}
	var ex api.System
	if err := m.readExample(name, &ex); err != nil {
		return
	}
	sys.Checks, sys.Config = ex.Checks, ex.Config
	sys.GPU = api.GPUInfo{Paused: sys.GPU.Paused}
	sys.OpenVINO = nil
	if name == "system_remote.json" { // no device, power or OpenVINO block
		sys.Devices, sys.Power = nil, nil
	}
}
