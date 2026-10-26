// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Onboarding endpoints of the mock (SPEC-v3 V4, .claude/v3-brief.md §7.2-
// §7.4): /api/app and /api/onboarding, hardware detection and its
// recommendation, the component manager (list, plan, install with progress
// that advances with time, tasks and their pause / resume / cancel / retry,
// download sources, cost tiers), the environment-variable keys of step 4 and
// the data import of step 1 (desktop mode).
//
// The state (onboarding progress, installed components, tasks, sources,
// import) lives per mock instance and is shared by every scenario; restart
// the server (or DELETE /api/onboarding, mock only) to start over.
//
// Failure modes come from the scenario of the request (?mock=…) or, for
// every scenario, from the -onboarding flag:
//
//	ob_fail    the install task stops with components.network at 35 %;
//	           retry (after changing the source or not) finishes it
//	ob_mirror  the mirror source is unreachable: the task falls back to the
//	           default source and says so in its notes (components.mirror_fallback)
//	ob_disk    the data directory's disk has 2 GB free: the plan is not
//	           enough_space and an install fails with components.disk_full
//	ob_cpu     no NVIDIA or Intel accelerator: the recommendation is the
//	           OpenVINO CPU mode + small with the "slow, use night processing" note
//	ob_noenv   no API key in the environment (GET /api/onboarding/env-keys
//	           reports present: false)
//	ob_staged  each item of an install is installed as soon as it is
//	           downloaded, and GET /api/asr/devices lists only the devices of
//	           an installed back end (v3_accfix.go): the back ends appear
//	           one after the other in onboarding step 6
//	ob_desktop GET /api/app reports desktop mode and the diagnostics have the
//	           Python / faster-whisper / pypdf errors of a fresh desktop
//	           install (v3_accfix.go)
//
// The 3.1.0 scenarios (v31_onboarding.go has the details): ob_storage and
// ob_storage_full (desktop mode; the data folder's disk has room / 2 GB),
// ob_noasr (desktop mode, nothing usable to transcribe with), ob_planerr
// (the recommendation cannot be planned: plan_error), ob_login_wait,
// ob_login_nourl, ob_login_fail and ob_login_timeout (the Google sign-in of
// the Colab branch).
//
// Scenario "empty" (no courses) is the one where the front end enters the
// onboarding by itself (GET /api/app onboarding_done false + no course).
// No key is ever stored or returned in full: use-env-key only sets the
// provider's key_hint to the fake mask sk-…abcd.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

var obFlag = flag.String("onboarding", "", "onboarding mode for every scenario: fail | mirror | disk | cpu | noenv | staged | desktop | storage | storage_full | noasr | planerr | login_wait | login_nourl | login_fail | login_timeout (v3_onboarding.go, v31_onboarding.go)")

// obInstallDuration is how long a whole simulated install downloads (tests
// shorten it); verifying and installing take obPhase each after that.
var (
	obInstallDuration = 24 * time.Second
	obPhase           = 1200 * time.Millisecond
)

const (
	obMB = int64(1024 * 1024)
	obGB = 1024 * obMB
)

// obMode is the failure mode of a request: the scenario's, else the flag's.
func obMode(r *http.Request) string {
	if sc := scenario(r); strings.HasPrefix(sc, "ob_") {
		return strings.TrimPrefix(sc, "ob_")
	}
	return *obFlag
}

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		mux.HandleFunc("GET /api/app", a(m.obGetApp))
		mux.HandleFunc("GET /api/onboarding", a(m.obGet))
		mux.HandleFunc("PUT /api/onboarding", a(m.obPut))
		mux.HandleFunc("DELETE /api/onboarding", a(m.obReset)) // mock only: start over
		mux.HandleFunc("GET /api/onboarding/env-keys", a(m.obEnvKeys))
		mux.HandleFunc("POST /api/onboarding/use-env-key", a(m.obUseEnvKey))
		mux.HandleFunc("GET /api/hardware", a(m.obHardware))
		mux.HandleFunc("POST /api/hardware/detect", a(m.obHardware))
		mux.HandleFunc("GET /api/components", a(m.obComponents))
		mux.HandleFunc("POST /api/components/plan", a(m.obPlan))
		mux.HandleFunc("POST /api/components/install", a(m.obInstall))
		mux.HandleFunc("GET /api/components/tasks", a(m.obTasks))
		mux.HandleFunc("POST /api/components/tasks/{id}/{action}", a(m.obTaskAction))
		mux.HandleFunc("GET /api/components/sources", a(m.obGetSources))
		mux.HandleFunc("PUT /api/components/sources", a(m.obPutSources))
		mux.HandleFunc("GET /api/components/presets", a(m.obPresets))
		mux.HandleFunc("POST /api/app/import", a(m.obStartImport))
		mux.HandleFunc("GET /api/app/import", a(m.obImportStatus))
	})
}

// ---------- state ----------

type obState struct {
	mu         sync.Mutex
	onboarding api.OnboardingState
	installed  map[string]bool
	tasks      []*obTask
	seq        int
	sources    obSources
	imp        *obImport
}

var obStates sync.Map // *mock → *obState

func (m *mock) ob() *obState {
	if v, ok := obStates.Load(m); ok {
		return v.(*obState)
	}
	v, _ := obStates.LoadOrStore(m, newOBState())
	return v.(*obState)
}

func newOBState() *obState {
	return &obState{
		onboarding: api.OnboardingState{Step: "welcome", Data: json.RawMessage("{}")},
		installed:  map[string]bool{"core": true},
		sources: obSources{
			PyPI:   obSource{Mode: "default"},
			Models: obSource{Mode: "default"},
		},
	}
}

// ---------- /api/app, /api/onboarding ----------

func (m *mock) obGetApp(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	done := st.onboarding.Done
	st.mu.Unlock()
	info := api.AppInfo{Version: "3.0.0-mock", Mode: "web", Platform: "windows", Arch: "amd64",
		DataDir: m.appSetDataDir(), OnboardingDone: done}
	if obDesktopMode(obMode(r)) {
		info.Mode, info.ResourcesDir = "desktop", `C:\Program Files\LectureFlow\resources`
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *mock) obGet(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	writeJSON(w, http.StatusOK, st.onboarding)
}

var obSteps = map[string]bool{"welcome": true, "privacy": true, "computer": true, "asr_mode": true, "storage": true,
	"providers": true, "download": true, "selftest": true, "first_course": true, "done": true}

// obPut mirrors internal/api putOnboarding (step a non-empty string, done a
// boolean, data an object); the mock also checks the step names of §7.4.
func (m *mock) obPut(w http.ResponseWriter, r *http.Request, s *store) {
	var raw map[string]json.RawMessage
	if err := decodeBody(r, &raw); err != nil || raw == nil {
		writeErr(w, errBad("请求体不是有效的 JSON"))
		return
	}
	var in api.OnboardingState
	if err := json.Unmarshal(raw["step"], &in.Step); err != nil || !obSteps[in.Step] {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "onboarding_state.invalid", Params: map[string]any{"field": "step"}}})
		return
	}
	if d := string(raw["done"]); d != "true" && d != "false" {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "onboarding_state.invalid", Params: map[string]any{"field": "done"}}})
		return
	}
	in.Done = string(raw["done"]) == "true"
	in.Data = json.RawMessage("{}")
	if d := raw["data"]; len(d) > 0 && string(d) != "null" {
		var obj map[string]any
		if json.Unmarshal(d, &obj) != nil || obj == nil {
			writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "onboarding_state.invalid", Params: map[string]any{"field": "data"}}})
			return
		}
		in.Data, _ = json.Marshal(obj)
	}
	st := m.ob()
	st.mu.Lock()
	st.onboarding = in
	st.mu.Unlock()
	s.notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, in)
}

func (m *mock) obReset(w http.ResponseWriter, r *http.Request, s *store) {
	obStates.Store(m, newOBState())
	w.WriteHeader(http.StatusNoContent)
}

// ---------- hardware ----------

type obHW struct {
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
	CPU          string  `json:"cpu"`
	RAMMB        int     `json:"ram_mb"`
	DiskFreeMB   int     `json:"disk_free_mb"`
	NVIDIA       []obGPU `json:"nvidia"`
	IntelGPU     bool    `json:"intel_gpu"`
	IntelNPU     bool    `json:"intel_npu"`
	IntelGPUName string  `json:"intel_gpu_name"`
	IntelNPUName string  `json:"intel_npu_name"`
	AppleSilicon bool    `json:"apple_silicon"`
	Chip         string  `json:"chip"`
	UnifiedMemMB int     `json:"unified_mem_mb"`
	HasBattery   bool    `json:"has_battery"`
}

type obGPU struct {
	Name   string `json:"name"`
	VRAMMB int    `json:"vram_mb"`
	Driver string `json:"driver"`
	Source string `json:"source"`
}

type obRecommendation struct {
	Components    []string          `json:"components"`
	Reasons       []model.ErrorInfo `json:"reasons"`
	Notes         []model.ErrorInfo `json:"notes"`
	DownloadBytes int64             `json:"download_bytes"`
}

type obHardwareView struct {
	HW             obHW             `json:"hw"`
	Recommendation obRecommendation `json:"recommendation"`
	// PlanError: the recommendation could not be planned (3.1.0 M2); null otherwise.
	PlanError *model.ErrorInfo `json:"plan_error"`
}

func obDiskFreeMB(mode string) int {
	if mode == "disk" || mode == "storage_full" {
		return 2048
	}
	return 214500
}

func (m *mock) obHardware(w http.ResponseWriter, r *http.Request, s *store) {
	mode := obMode(r)
	hw := obHW{OS: "windows", Arch: "amd64", CPU: "Intel(R) Core(TM) Ultra 7 155H", RAMMB: 32446,
		DiskFreeMB: obDiskFreeMB(mode), NVIDIA: []obGPU{}, HasBattery: true}
	rec := obRecommendation{Reasons: []model.ErrorInfo{}, Notes: []model.ErrorInfo{}}
	if mode == "cpu" {
		hw.CPU, hw.RAMMB = "Intel(R) Core(TM) i5-8250U CPU @ 1.60GHz", 8072
		rec.Components = []string{"python", "asr-openvino", "model:openvino:small:int8"}
		rec.Reasons = append(rec.Reasons, model.ErrorInfo{Code: "hw.reason.cpu_only", Params: map[string]any{"cpu": hw.CPU}})
		rec.Notes = append(rec.Notes, model.ErrorInfo{Code: "hw.note.cpu_slow"})
	} else {
		hw.NVIDIA = []obGPU{{Name: "NVIDIA GeForce RTX 4070 Laptop GPU", VRAMMB: 8188, Driver: "566.14", Source: "nvidia-smi"}}
		hw.IntelGPU, hw.IntelGPUName = true, "Intel(R) Arc(TM) Graphics"
		hw.IntelNPU, hw.IntelNPUName = true, "Intel(R) AI Boost"
		rec.Components = []string{"python", "asr-cuda", "model:faster-whisper:large-v3:float16", "asr-openvino", "model:openvino:large-v3-turbo:fp16"}
		rec.Reasons = append(rec.Reasons,
			model.ErrorInfo{Code: "hw.reason.nvidia", Params: map[string]any{"name": hw.NVIDIA[0].Name, "vram_gb": 8}},
			model.ErrorInfo{Code: "hw.reason.npu_battery", Params: map[string]any{"name": hw.IntelNPUName}})
	}
	view := obHardwareView{HW: hw}
	if mode == "planerr" {
		// a recommended model the manifest does not have: the plan fails
		// (the real server then reports the estimate as download_bytes)
		rec.Components = append(rec.Components, obPlanErrModel)
		view.PlanError = &model.ErrorInfo{Code: "components.unknown", Params: map[string]any{"id": obPlanErrModel}}
	}
	st := m.ob()
	st.mu.Lock()
	rec.DownloadBytes = st.plan(rec.Components, mode).DownloadBytes
	st.mu.Unlock()
	view.Recommendation = rec
	writeJSON(w, http.StatusOK, view)
}

// ---------- components ----------

type obComp struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // python-runtime | venv | model | binary | tool
	Version       string   `json:"version"`
	NameKey       string   `json:"name_key,omitempty"`
	DescKey       string   `json:"desc_key,omitempty"`
	Backend       string   `json:"backend,omitempty"`
	Model         string   `json:"model,omitempty"`
	Precision     string   `json:"precision,omitempty"`
	DownloadBytes int64    `json:"download_bytes"`
	InstallBytes  int64    `json:"install_bytes"`
	Requires      []string `json:"requires"`
	License       string   `json:"license"`
	State         string   `json:"state"` // available | installed | installing | broken | unavailable
	Installed     string   `json:"installed_version,omitempty"`
	UpdateVersion string   `json:"update_version,omitempty"` // set by obCompHook (v3_update.go)
	Unavailable   string   `json:"unavailable_reason,omitempty"`
}

// obCompHook, when set, adjusts each listed component after its state is
// known (v3_update.go: offered updates, repairs, versions after an update).
// Called with st.mu held.
var obCompHook func(st *obState, c *obComp)

// obCatalog is the windows-x64 manifest of the mock (sizes are plausible,
// not measured).
var obCatalog = []obComp{
	{ID: "core", Kind: "binary", Version: "3.0.0", DownloadBytes: 0, InstallBytes: 210 * obMB, Requires: []string{}, License: "AGPL-3.0-or-later"},
	{ID: "python", Kind: "python-runtime", Version: "3.12.8-1", DownloadBytes: 46 * obMB, InstallBytes: 160 * obMB, Requires: []string{}, License: "PSF-2.0"},
	{ID: "asr-cuda", Kind: "venv", Version: "1.1.1-1", Backend: "faster-whisper", DownloadBytes: 1290 * obMB, InstallBytes: 2900 * obMB, Requires: []string{"python"}, License: "MIT"},
	{ID: "asr-openvino", Kind: "venv", Version: "2025.3-1", Backend: "openvino", DownloadBytes: 385 * obMB, InstallBytes: 1100 * obMB, Requires: []string{"python"}, License: "Apache-2.0"},
	{ID: "asr-mlx", Kind: "venv", Version: "0.4.2-1", Backend: "mlx", DownloadBytes: 120 * obMB, InstallBytes: 300 * obMB, Requires: []string{"python"}, License: "MIT", Unavailable: "platform"},
	// 3.1.0: the Google Cloud CLI of the Colab branch (a tool with its own Python)
	{ID: "gcloud", Kind: "tool", Version: "548.0.0", NameKey: "compui.name.gcloud", DescKey: "compui.hint.gcloud", DownloadBytes: 118 * obMB, InstallBytes: 430 * obMB, Requires: []string{}, License: "Apache-2.0"},
	{ID: "model:faster-whisper:large-v3:float16", Kind: "model", Version: "1", Backend: "faster-whisper", Model: "large-v3", Precision: "float16", DownloadBytes: 3087 * obMB, InstallBytes: 3087 * obMB, Requires: []string{"asr-cuda"}, License: "MIT"},
	{ID: "model:faster-whisper:large-v3-turbo:float16", Kind: "model", Version: "1", Backend: "faster-whisper", Model: "large-v3-turbo", Precision: "float16", DownloadBytes: 1620 * obMB, InstallBytes: 1620 * obMB, Requires: []string{"asr-cuda"}, License: "MIT"},
	{ID: "model:faster-whisper:medium:int8_float16", Kind: "model", Version: "1", Backend: "faster-whisper", Model: "medium", Precision: "int8_float16", DownloadBytes: 1530 * obMB, InstallBytes: 1530 * obMB, Requires: []string{"asr-cuda"}, License: "MIT"},
	{ID: "model:openvino:large-v3-turbo:fp16", Kind: "model", Version: "1", Backend: "openvino", Model: "large-v3-turbo", Precision: "fp16", DownloadBytes: 1640 * obMB, InstallBytes: 1640 * obMB, Requires: []string{"asr-openvino"}, License: "MIT"},
	{ID: "model:openvino:small:int8", Kind: "model", Version: "1", Backend: "openvino", Model: "small", Precision: "int8", DownloadBytes: 260 * obMB, InstallBytes: 260 * obMB, Requires: []string{"asr-openvino"}, License: "MIT"},
}

func obFind(id string) (obComp, bool) {
	for _, c := range obCatalog {
		if c.ID == id {
			return c, true
		}
	}
	return obComp{}, false
}

// installing reports the ids of the active tasks. Must hold st.mu.
func (st *obState) installing() map[string]bool {
	out := map[string]bool{}
	for _, t := range st.tasks {
		if t.active() {
			for _, it := range t.items {
				out[it.ID] = true
			}
		}
	}
	return out
}

func (st *obState) list() []obComp {
	busy := st.installing()
	out := make([]obComp, 0, len(obCatalog))
	for _, c := range obCatalog {
		c.Requires = append([]string{}, c.Requires...)
		switch {
		case c.Unavailable != "":
			c.State = "unavailable"
		case st.installed[c.ID]:
			c.State, c.Installed = "installed", c.Version
		case busy[c.ID]:
			c.State = "installing"
		default:
			c.State = "available"
		}
		if obCompHook != nil {
			obCompHook(st, &c)
		}
		out = append(out, c)
	}
	return out
}

type obPlanView struct {
	IDs           []string `json:"ids"`
	DownloadBytes int64    `json:"download_bytes"`
	InstallBytes  int64    `json:"install_bytes"`
	FreeBytes     int64    `json:"free_bytes"`
	EnoughSpace   bool     `json:"enough_space"`
	NeedBytes     int64    `json:"need_bytes"`
}

// closure adds the requirements of ids (manifest order), leaving out the
// unknown and unavailable ones.
func obClosure(ids []string) ([]string, error) {
	want := map[string]bool{}
	var add func(id string) error
	add = func(id string) error {
		c, ok := obFind(id)
		if !ok {
			return &httpError{status: 400, body: api.ErrorBody{Error: model.ErrorInfo{Code: "components.unknown", Params: map[string]any{"id": id}}}}
		}
		if c.Unavailable != "" {
			return &httpError{status: 400, body: api.ErrorBody{Error: model.ErrorInfo{Code: "components.unavailable", Params: map[string]any{"id": id, "reason": c.Unavailable}}}}
		}
		if want[id] {
			return nil
		}
		want[id] = true
		for _, d := range c.Requires {
			if err := add(d); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, c := range obCatalog {
		if want[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out, nil
}

// plan of ids (unknown ids are dropped here; the handler reports them).
// Must hold st.mu.
func (st *obState) plan(ids []string, mode string) obPlanView {
	all, err := obClosure(ids)
	if err != nil {
		all = nil
		for _, id := range ids {
			if c, ok := obFind(id); ok && c.Unavailable == "" {
				all = append(all, id)
			}
		}
	}
	p := obPlanView{IDs: []string{}, FreeBytes: int64(obDiskFreeMB(mode)) * obMB}
	for _, id := range all {
		p.IDs = append(p.IDs, id)
		if st.installed[id] {
			continue
		}
		c, _ := obFind(id)
		p.DownloadBytes += c.DownloadBytes
		p.InstallBytes += c.InstallBytes
	}
	// the archive and the unpacked files exist side by side for a moment
	p.NeedBytes = p.DownloadBytes + p.InstallBytes
	p.EnoughSpace = p.FreeBytes >= p.NeedBytes
	return p
}

type obComponentsView struct {
	Platform string         `json:"platform"`
	Manifest map[string]any `json:"manifest"`
	Comps    []obComp       `json:"components"`
	Tasks    []obTaskView   `json:"tasks"`
}

func (m *mock) obComponents(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	now := time.Now()
	v := obComponentsView{Platform: "windows-x64",
		Manifest: map[string]any{"app_version": "3.0.0", "published_at": "2026-10-20T08:00:00Z", "update_available": false},
		Tasks:    st.taskViews(now)}
	v.Comps = st.list()
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) obPlan(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := obClosure(in.IDs); err != nil {
		writeErr(w, err)
		return
	}
	st := m.ob()
	st.mu.Lock()
	p := st.plan(in.IDs, obMode(r))
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, p)
}

// ---------- tasks ----------

type obItem struct {
	ID     string
	Total  int64
	Source string
}

type obTask struct {
	id       string
	kind     string
	ids      []string
	items    []obItem
	total    int64
	speed    float64 // bytes per second while running
	state    string  // queued | running | paused | verifying | installing | done | failed | cancelled
	base     int64   // bytes done before the current run
	runStart time.Time
	failAt   int64 // -1 = no failure
	err      *model.ErrorInfo
	notes    []model.ErrorInfo
	doneAt   time.Time // when the download finished (verifying / installing follow)
	staged   bool      // ob_staged: each item is installed once downloaded
}

func (t *obTask) active() bool {
	switch t.state {
	case "queued", "running", "verifying", "installing", "paused":
		return true
	}
	return false
}

type obItemView struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	DoneBytes int64  `json:"done_bytes"`
	Total     int64  `json:"total_bytes"`
	Source    string `json:"source"`
}

type obTaskView struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	State     string            `json:"state"`
	IDs       []string          `json:"ids"`
	DoneBytes int64             `json:"done_bytes"`
	Total     int64             `json:"total_bytes"`
	SpeedBps  int64             `json:"speed_bps"`
	ETAS      int64             `json:"eta_s"`
	Items     []obItemView      `json:"items"`
	Error     *model.ErrorInfo  `json:"error,omitempty"`
	Notes     []model.ErrorInfo `json:"notes"`
}

// advance moves a task along the clock: download, failure point,
// verifying, installing, done (the components become installed).
func (st *obState) advance(t *obTask, now time.Time) {
	if t.state == "running" {
		done := t.base + int64(t.speed*now.Sub(t.runStart).Seconds())
		if t.staged {
			st.installDownloaded(t, done)
		}
		if t.failAt >= 0 && done >= t.failAt {
			t.base, t.state, t.failAt = t.failAt, "failed", -1
			t.err = &model.ErrorInfo{Code: "components.network"}
			return
		}
		if done >= t.total {
			t.base, t.state, t.doneAt = t.total, "verifying", t.runStart.Add(time.Duration(float64(t.total-t.base)/t.speed*float64(time.Second)))
		}
	}
	if t.state == "verifying" && now.Sub(t.doneAt) >= obPhase {
		t.state = "installing"
	}
	if t.state == "installing" && now.Sub(t.doneAt) >= 2*obPhase {
		t.state = "done"
		for _, it := range t.items {
			st.installed[it.ID] = true
		}
	}
}

func (st *obState) taskView(t *obTask, now time.Time) obTaskView {
	st.advance(t, now)
	done := t.base
	if t.state == "running" {
		done = t.base + int64(t.speed*now.Sub(t.runStart).Seconds())
		if done > t.total {
			done = t.total
		}
	}
	v := obTaskView{ID: t.id, Kind: t.kind, State: t.state, IDs: append([]string{}, t.ids...), DoneBytes: done, Total: t.total,
		Error: t.err, Notes: append([]model.ErrorInfo{}, t.notes...), Items: []obItemView{}}
	if t.state == "running" {
		v.SpeedBps = int64(t.speed)
		v.ETAS = int64(float64(t.total-done) / t.speed)
	}
	left := done
	for _, it := range t.items {
		iv := obItemView{ID: it.ID, Total: it.Total, Source: it.Source}
		switch {
		case left >= it.Total:
			iv.DoneBytes, iv.State = it.Total, "downloaded"
			left -= it.Total
		case left > 0:
			iv.DoneBytes, iv.State = left, "downloading"
			left = 0
		default:
			iv.State = "queued"
		}
		if t.staged && st.installed[it.ID] && t.state != "failed" && t.state != "cancelled" {
			iv.State = "installed"
			v.Items = append(v.Items, iv)
			continue
		}
		switch t.state {
		case "verifying", "installing", "done":
			iv.State = t.state
			if t.state == "done" {
				iv.State = "installed"
			}
		case "failed":
			if iv.State == "downloading" {
				iv.State = "failed"
			}
		case "paused", "cancelled":
			if iv.State == "downloading" {
				iv.State = t.state
			}
		}
		v.Items = append(v.Items, iv)
	}
	return v
}

func (st *obState) taskViews(now time.Time) []obTaskView {
	out := []obTaskView{}
	for _, t := range st.tasks {
		out = append(out, st.taskView(t, now))
	}
	return out
}

func (st *obState) task(id string) *obTask {
	for _, t := range st.tasks {
		if t.id == id {
			return t
		}
	}
	return nil
}

func obTaskNotFound(id string) error {
	return &httpError{status: 404, body: api.ErrorBody{Error: model.ErrorInfo{Code: "components.task_not_found", Params: map[string]any{"task": id}}}}
}

func (m *mock) obInstall(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	all, err := obClosure(in.IDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	mode := obMode(r)
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	busy := st.installing()
	var todo []string
	for _, id := range all {
		if busy[id] {
			writeJSON(w, http.StatusConflict, api.ErrorBody{Error: model.ErrorInfo{Code: "components.busy", Params: map[string]any{"id": id}}})
			return
		}
		if !st.installed[id] {
			todo = append(todo, id)
		}
	}
	if len(todo) == 0 {
		writeJSON(w, http.StatusConflict, api.ErrorBody{Error: model.ErrorInfo{Code: "components.up_to_date", Params: map[string]any{"id": strings.Join(all, ", "), "version": ""}}})
		return
	}
	st.seq++
	t := &obTask{id: fmt.Sprintf("task-%d", st.seq), kind: "install", ids: todo, state: "running", runStart: now, failAt: -1, notes: []model.ErrorInfo{}, staged: mode == "staged"}
	for _, id := range todo {
		c, _ := obFind(id)
		src := st.sources.PyPI.Mode
		if c.Kind == "model" {
			src = st.sources.Models.Mode
		}
		if mode == "mirror" {
			src = "default"
		}
		t.items = append(t.items, obItem{ID: id, Total: c.DownloadBytes, Source: src})
		t.total += c.DownloadBytes
	}
	dur := obInstallDuration
	if obToolsOnly(todo) {
		dur /= 4 // the Google Cloud CLI of the Colab branch is a small download
	}
	t.speed = float64(t.total) / dur.Seconds()
	if t.speed <= 0 {
		t.speed = 1
	}
	switch mode {
	case "fail":
		t.failAt = t.total * 35 / 100
	case "mirror":
		if st.sources.PyPI.Mode == "mirror" || st.sources.Models.Mode == "mirror" {
			group, mirror := "models", st.sources.Models.MirrorID
			if st.sources.Models.Mode != "mirror" {
				group, mirror = "pypi", st.sources.PyPI.MirrorID
			}
			t.notes = append(t.notes, model.ErrorInfo{Code: "components.mirror_fallback", Params: map[string]any{"group": group, "mirror": mirror, "reason": "components.network"}})
		}
	case "disk":
		p := st.plan(todo, mode)
		t.state = "failed"
		t.err = &model.ErrorInfo{Code: "components.disk_full", Params: map[string]any{"required_mb": p.NeedBytes / obMB, "free_mb": p.FreeBytes / obMB}}
	}
	st.tasks = append(st.tasks, t)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": st.taskView(t, now)})
}

func (m *mock) obTasks(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	v := st.taskViews(time.Now())
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) obTaskAction(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	id, action := r.PathValue("id"), r.PathValue("action")
	t := st.task(id)
	if t == nil {
		writeErr(w, obTaskNotFound(id))
		return
	}
	st.advance(t, now)
	bad := func() {
		writeJSON(w, http.StatusConflict, api.ErrorBody{Error: model.ErrorInfo{Code: "components.task_state", Params: map[string]any{"task": id, "state": t.state}}})
	}
	switch action {
	case "pause":
		if t.state != "running" {
			bad()
			return
		}
		t.base += int64(t.speed * now.Sub(t.runStart).Seconds())
		t.state = "paused"
	case "resume":
		if t.state != "paused" {
			bad()
			return
		}
		t.state, t.runStart = "running", now
	case "cancel":
		if !t.active() {
			bad()
			return
		}
		if t.state == "running" {
			t.base += int64(t.speed * now.Sub(t.runStart).Seconds())
		}
		t.state = "cancelled"
	case "retry":
		if t.state != "failed" && t.state != "cancelled" {
			bad()
			return
		}
		if t.err != nil && t.err.Code == "components.disk_full" && obMode(r) == "disk" {
			p := st.plan(t.ids, "disk")
			t.err = &model.ErrorInfo{Code: "components.disk_full", Params: map[string]any{"required_mb": p.NeedBytes / obMB, "free_mb": p.FreeBytes / obMB}}
			writeJSON(w, http.StatusAccepted, map[string]any{"task": st.taskView(t, now)})
			return
		}
		// a retry downloads with the sources as they are now
		for i := range t.items {
			c, _ := obFind(t.items[i].ID)
			src := st.sources.PyPI.Mode
			if c.Kind == "model" {
				src = st.sources.Models.Mode
			}
			t.items[i].Source = src
		}
		t.state, t.runStart, t.err, t.failAt = "running", now, nil, -1
	default:
		writeErr(w, errNotFound("操作"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": st.taskView(t, now)})
}

// ---------- sources ----------

type obSource struct {
	Mode      string `json:"mode"` // default | mirror
	MirrorID  string `json:"mirror_id"`
	CustomURL string `json:"custom_url"`
}

type obSourceOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type obSources struct {
	PyPI   obSource `json:"pypi"`
	Models obSource `json:"models"`
}

type obSourcesView struct {
	PyPI          obSource                    `json:"pypi"`
	Models        obSource                    `json:"models"`
	Options       map[string][]obSourceOption `json:"options"`
	SuggestMirror bool                        `json:"suggest_mirror"`
}

var obSourceOptions = map[string][]obSourceOption{
	"pypi": {
		{ID: "tuna", Label: "TUNA (Tsinghua University)", URL: "https://pypi.tuna.tsinghua.edu.cn/simple"},
		{ID: "aliyun", Label: "Alibaba Cloud", URL: "https://mirrors.aliyun.com/pypi/simple"},
	},
	"models": {
		{ID: "hf-mirror", Label: "hf-mirror.com", URL: "https://hf-mirror.com"},
		{ID: "modelscope", Label: "ModelScope", URL: "https://www.modelscope.cn"},
	},
}

func (st *obState) sourcesView() obSourcesView {
	return obSourcesView{PyPI: st.sources.PyPI, Models: st.sources.Models, Options: obSourceOptions, SuggestMirror: true}
}

func (m *mock) obGetSources(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	v := st.sourcesView()
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

func obCheckSource(group string, src obSource) error {
	invalid := func(field string) error {
		return &httpError{status: 400, body: api.ErrorBody{Error: model.ErrorInfo{Code: "components.sources_invalid", Params: map[string]any{"group": group, "field": field}}}}
	}
	switch src.Mode {
	case "default":
		return nil
	case "mirror":
		if src.CustomURL != "" {
			if !strings.HasPrefix(src.CustomURL, "https://") && !strings.HasPrefix(src.CustomURL, "http://") {
				return invalid("custom_url")
			}
			return nil
		}
		for _, o := range obSourceOptions[group] {
			if o.ID == src.MirrorID {
				return nil
			}
		}
		return invalid("mirror_id")
	}
	return invalid("mode")
}

func (m *mock) obPutSources(w http.ResponseWriter, r *http.Request, s *store) {
	var in obSources
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	for group, src := range map[string]obSource{"pypi": in.PyPI, "models": in.Models} {
		if err := obCheckSource(group, src); err != nil {
			writeErr(w, err)
			return
		}
	}
	st := m.ob()
	st.mu.Lock()
	st.sources = in
	v := st.sourcesView()
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// ---------- presets (the shape of internal/components.Presets) ----------

type obPresetRole struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

type obPresetTier struct {
	ID              string                  `json:"id"`
	NameKey         string                  `json:"name_key"`
	DescKey         string                  `json:"desc_key"`
	CostKey         string                  `json:"cost_key"`
	NightProcessing bool                    `json:"night_processing"`
	TranscribeOnly  bool                    `json:"transcribe_only"`
	Roles           map[string]obPresetRole `json:"roles,omitempty"`
}

type obPresetProvider struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	BaseURL      string   `json:"base_url"`
	DefaultModel string   `json:"default_model"`
	Free         bool     `json:"free"`
	TermsNotice  bool     `json:"terms_notice"`
	TermsURL     string   `json:"terms_url,omitempty"`
	Regions      []string `json:"regions,omitempty"`
	SignupURL    string   `json:"signup_url"`
	KeysURL      string   `json:"keys_url"`
	PricingURL   string   `json:"pricing_url,omitempty"`
	StepsKeys    []string `json:"steps_keys"`
	CostKey      string   `json:"cost_key"`
	BalanceAPI   string   `json:"balance_api,omitempty"`
}

type obPresets struct {
	Default   string             `json:"default"`
	Tiers     []obPresetTier     `json:"tiers"`
	Providers []obPresetProvider `json:"providers"`
	Free      struct {
		Default  string            `json:"default"`
		ByRegion map[string]string `json:"by_region,omitempty"`
	} `json:"free"`
}

func obRoles(fn func(role string) obPresetRole) map[string]obPresetRole {
	out := map[string]obPresetRole{}
	for _, r := range model.Roles {
		out[r] = fn(r)
	}
	return out
}

func obPresetsData() obPresets {
	var p obPresets
	p.Default = "standard"
	p.Providers = []obPresetProvider{
		{ID: "deepseek", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com", DefaultModel: "deepseek-flash",
			SignupURL: "https://platform.deepseek.com/sign_up", KeysURL: "https://platform.deepseek.com/api_keys", PricingURL: "https://api-docs.deepseek.com/quick_start/pricing",
			StepsKeys: []string{"onboarding.preset.deepseek.step1", "onboarding.preset.deepseek.step2", "onboarding.preset.deepseek.step3"},
			CostKey:   "onboarding.preset.deepseek.cost", BalanceAPI: model.BalanceDeepSeek},
		{ID: "glm-flash", Name: "Zhipu GLM-4-Flash", Type: model.ProviderOpenAI, BaseURL: "https://open.bigmodel.cn/api/paas/v4", DefaultModel: "glm-4-flash",
			Free: true, TermsNotice: true, TermsURL: "https://open.bigmodel.cn/", Regions: []string{"cn"},
			SignupURL: "https://open.bigmodel.cn/", KeysURL: "https://open.bigmodel.cn/usercenter/apikeys",
			StepsKeys: []string{"onboarding.preset.free.step1", "onboarding.preset.free.step2"}, CostKey: "onboarding.preset.free.cost"},
		{ID: "openrouter-free", Name: "OpenRouter (free models)", Type: model.ProviderOpenAI, BaseURL: "https://openrouter.ai/api/v1", DefaultModel: "deepseek/deepseek-chat:free",
			Free: true, TermsNotice: true, TermsURL: "https://openrouter.ai/terms",
			SignupURL: "https://openrouter.ai/", KeysURL: "https://openrouter.ai/settings/keys",
			StepsKeys: []string{"onboarding.preset.free.step1", "onboarding.preset.free.step2"}, CostKey: "onboarding.preset.free.cost"},
	}
	p.Free.Default = "openrouter-free"
	p.Free.ByRegion = map[string]string{"cn": "glm-flash"}
	reviewRoles := map[string]bool{model.RoleGlossaryJanitor: true, model.RoleChapters: true, model.RoleOrganizer: true, model.RoleSyllabusFormat: true}
	p.Tiers = []obPresetTier{
		{ID: "standard", NameKey: "onboarding.tier.standard.name", DescKey: "onboarding.tier.standard.desc", CostKey: "onboarding.tier.standard.cost",
			Roles: obRoles(func(string) obPresetRole { return obPresetRole{Provider: "deepseek"} })},
		{ID: "saver", NameKey: "onboarding.tier.saver.name", DescKey: "onboarding.tier.saver.desc", CostKey: "onboarding.tier.saver.cost",
			Roles: obRoles(func(r string) obPresetRole {
				if reviewRoles[r] {
					return obPresetRole{Provider: "$free"}
				}
				return obPresetRole{Provider: "deepseek"}
			})},
		{ID: "free", NameKey: "onboarding.tier.free.name", DescKey: "onboarding.tier.free.desc", CostKey: "onboarding.tier.free.cost", NightProcessing: true,
			Roles: obRoles(func(string) obPresetRole { return obPresetRole{Provider: "$free"} })},
		{ID: "transcribe_only", NameKey: "onboarding.tier.transcribe_only.name", DescKey: "onboarding.tier.transcribe_only.desc", CostKey: "onboarding.tier.transcribe_only.cost", TranscribeOnly: true},
	}
	return p
}

func (m *mock) obPresets(w http.ResponseWriter, r *http.Request, s *store) {
	writeJSON(w, http.StatusOK, obPresetsData())
}

// ---------- environment-variable keys ----------

type obEnvKey struct {
	ProviderType string `json:"provider_type"`
	Env          string `json:"env"`
	Present      bool   `json:"present"`
	Masked       string `json:"masked,omitempty"`
}

func (m *mock) obEnvKeys(w http.ResponseWriter, r *http.Request, s *store) {
	k := obEnvKey{ProviderType: "deepseek", Env: "DEEPSEEK_API_KEY", Present: obMode(r) != "noenv"}
	if k.Present {
		k.Masked = "sk-…abcd"
	}
	writeJSON(w, http.StatusOK, []obEnvKey{k})
}

// obUseEnvKey creates (or updates) the provider of the preset from the
// "environment": the mock stores only the mask.
func (m *mock) obUseEnvKey(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		ProviderType string `json:"provider_type"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	var pp *obPresetProvider
	for _, p := range obPresetsData().Providers {
		if p.ID == in.ProviderType {
			p := p
			pp = &p
		}
	}
	if pp == nil || in.ProviderType != "deepseek" || obMode(r) == "noenv" {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "onboarding.env_key_missing", Params: map[string]any{"provider_type": in.ProviderType}}})
		return
	}
	hint := "sk-…abcd"
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		for i, p := range st.settings.Providers {
			if strings.TrimRight(p.BaseURL, "/") == pp.BaseURL {
				p.KeyHint = hint
				st.settings.Providers[i] = p
				st.settingsChanged()
				return http.StatusOK, st.providerView(p), nil
			}
		}
		name, typ, base, bal := pp.Name, pp.Type, pp.BaseURL, pp.BalanceAPI
		p := api.ProviderView{ID: st.newID("01JPRV"), MaxConcurrency: settings.DefaultMaxConcurrency, RPM: settings.DefaultRPM,
			CreatedAt: time.Now().UTC().Format(time.RFC3339), UsedByRoles: []string{}, Models: []string{}}
		if err := applyProviderInput(&p, api.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, BalanceAPI: &bal}); err != nil {
			return 0, nil, err
		}
		for st.checkProviderName(p) != nil {
			p.Name += " (2)"
		}
		p.KeyHint = hint
		st.settings.Providers = append(st.settings.Providers, p)
		st.settingsChanged()
		return http.StatusOK, st.providerView(p), nil
	})
}

// ---------- data import (desktop mode, step 1) ----------

type obImport struct {
	Path    string
	Started time.Time
	Total   int64
}

type obImportView struct {
	State       string           `json:"state"` // copying | done | failed
	CopiedBytes int64            `json:"copied_bytes"`
	TotalBytes  int64            `json:"total_bytes"`
	Error       *model.ErrorInfo `json:"error,omitempty"`
}

func (imp *obImport) view(now time.Time) obImportView {
	const dur = 6 * time.Second
	el := now.Sub(imp.Started)
	if el >= dur {
		return obImportView{State: "done", CopiedBytes: imp.Total, TotalBytes: imp.Total}
	}
	return obImportView{State: "copying", CopiedBytes: int64(float64(imp.Total) * el.Seconds() / dur.Seconds()), TotalBytes: imp.Total}
}

func (m *mock) obStartImport(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Path) == "" {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "request.bad_json"}})
		return
	}
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	if strings.Contains(strings.ToLower(in.Path), "fail") {
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: model.ErrorInfo{Code: "app.import_not_data_dir", Params: map[string]any{"path": in.Path}}})
		return
	}
	st.imp = &obImport{Path: in.Path, Started: time.Now(), Total: 3*obGB + 412*obMB}
	writeJSON(w, http.StatusAccepted, st.imp.view(time.Now()))
}

func (m *mock) obImportStatus(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.imp == nil {
		writeErr(w, errNotFound("导入任务"))
		return
	}
	writeJSON(w, http.StatusOK, st.imp.view(time.Now()))
}
