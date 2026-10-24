// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The settings page's "Components" section and the application update hint
// in the mock (SPEC-v3 V11, V3, V8):
//
//   - the application update relay of internal/api/handlers_update.go:
//     GET /api/app/update, POST /api/app/update/download, POST
//     /api/app/update/install-on-quit; and GET /api/app/activity (desktop.go).
//     The real relay answers only in desktop mode; the mock always answers
//     (the page itself shows the hint only when GET /api/app says desktop).
//     Scenarios: update_available (3.1.0 can be downloaded; the download
//     takes updDownloadDur), update_downloading, update_ready, update_error
//     (the download failed; retry works); every other scenario: "none".
//     The state is per mock instance and scenario.
//   - the component endpoints v3_onboarding.go does not have: update
//     (POST /api/components/{id}/update), uninstall (DELETE
//     /api/components/{id}; 409 components.in_use / busy / bundled /
//     not_installed) and import (POST /api/components/import: JSON {path} or
//     multipart "file"; a name containing "fail" fails with
//     components.import_checksum, one not ending in .zip is
//     components.import_invalid; otherwise the archive installs Whisper
//     medium for faster-whisper in a few seconds).
//   - POST /api/components/_demo (mock only; also the -components demo flag):
//     installs Python, NVIDIA transcription with Whisper large-v3 and Intel
//     transcription, offers an update for the NVIDIA runtime and marks the
//     Intel runtime as needing repair, so the settings page has something to
//     show without downloading first.

import (
	"flag"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

var compFlag = flag.String("components", "", "component demo state for every scenario: demo (v3_update.go)")

// updDownload is how long a simulated app update downloads (tests shorten it).
var updDownloadDur = 6 * time.Second

const updVersion = "3.1.0"

func init() {
	obCompHook = compDecorate
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		handle := func(pattern string, h http.HandlerFunc) {
			// another v3 mock file may already serve it (no duplicate panics)
			method, path, _ := strings.Cut(pattern, " ")
			if req, err := http.NewRequest(method, path, nil); err == nil {
				if _, p := mux.Handler(req); p == pattern {
					return
				}
			}
			mux.HandleFunc(pattern, h)
		}
		handle("GET /api/app/update", a(m.updGet))
		handle("POST /api/app/update/download", a(m.updDownload))
		handle("POST /api/app/update/install-on-quit", a(m.updInstallOnQuit))
		handle("GET /api/app/activity", a(m.updActivity))
		handle("POST /api/components/{id}/update", a(m.compUpdate))
		handle("DELETE /api/components/{id}", a(m.compUninstall))
		handle("POST /api/components/import", a(m.compImport))
		handle("POST /api/components/_demo", a(m.compDemo))
		if *compFlag == "demo" {
			m.compSeed()
		}
	})
}

// ---------- application update ----------

type updState struct {
	State         string // none | available | downloading | ready | error
	Version       string
	Started       time.Time // downloading since
	Requested     bool
	InstallOnQuit bool
	Err           string
}

type updStates struct {
	mu sync.Mutex
	by map[string]*updState // scenario → state
}

var updAll sync.Map // *mock → *updStates

func (m *mock) upd(r *http.Request) (*updStates, *updState) {
	v, _ := updAll.LoadOrStore(m, &updStates{by: map[string]*updState{}})
	all := v.(*updStates)
	all.mu.Lock()
	sc := scenario(r)
	st := all.by[sc]
	if st == nil {
		st = &updState{State: "none"}
		switch sc {
		case "update_available":
			st.State, st.Version = "available", updVersion
		case "update_downloading":
			st.State, st.Version, st.Started = "downloading", updVersion, time.Now()
		case "update_ready":
			st.State, st.Version = "ready", updVersion
		case "update_error":
			st.State, st.Version, st.Err = "error", updVersion, "update.download_failed"
		}
		all.by[sc] = st
	}
	return all, st // all.mu held
}

// view advances a download and returns the UpdateStatus.
func (st *updState) view(now time.Time) api.UpdateStatus {
	if st.State == "downloading" && now.Sub(st.Started) >= updDownloadDur {
		st.State = "ready"
	}
	v := api.UpdateStatus{State: st.State, Version: st.Version, DownloadRequested: st.Requested, InstallOnQuit: st.InstallOnQuit,
		UpdatedAt: now.UTC().Format(time.RFC3339)}
	switch st.State {
	case "downloading":
		v.Percent = float64(int(100 * now.Sub(st.Started).Seconds() / updDownloadDur.Seconds()))
	case "ready":
		v.Percent = 100
	case "error":
		v.Error = &model.ErrorInfo{Code: st.Err}
	}
	return v
}

func (m *mock) updGet(w http.ResponseWriter, r *http.Request, _ *store) {
	all, st := m.upd(r)
	defer all.mu.Unlock()
	writeJSON(w, http.StatusOK, st.view(time.Now()))
}

// updDownload: the page asks; the "main process" starts at once.
func (m *mock) updDownload(w http.ResponseWriter, r *http.Request, _ *store) {
	all, st := m.upd(r)
	defer all.mu.Unlock()
	if !(st.State == "available" || (st.State == "error" && st.Version != "")) {
		codedError(w, http.StatusConflict, "update.not_available", nil)
		return
	}
	st.State, st.Started, st.Err, st.Requested = "downloading", time.Now(), "", false
	writeJSON(w, http.StatusAccepted, st.view(time.Now()))
}

func (m *mock) updInstallOnQuit(w http.ResponseWriter, r *http.Request, _ *store) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if r.ContentLength != 0 {
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return
		}
	}
	enabled := in.Enabled == nil || *in.Enabled
	all, st := m.upd(r)
	defer all.mu.Unlock()
	if enabled && (st.Version == "" || st.State == "none") {
		codedError(w, http.StatusConflict, "update.not_available", nil)
		return
	}
	st.InstallOnQuit = enabled
	writeJSON(w, http.StatusOK, st.view(time.Now()))
}

// updActivity counts the running and queued lecture stages of the store.
func (m *mock) updActivity(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	s.advance(time.Now())
	var out api.AppActivity
	for _, l := range s.lectures {
		for _, st := range l.Stages {
			switch st.Status {
			case model.StatusRunning:
				out.Running++
				if st.Stage == model.StageASR {
					out.Transcribing = true
				} else if st.Stage != model.StagePrep && st.Stage != model.StageExport {
					out.APIRunning = true
				}
			case model.StatusQueued:
				out.Queued++
			}
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// ---------- components: update, uninstall, import, demo ----------

type compExtra struct {
	offer   map[string]string // id → offered update version
	updated map[string]string // id → version installed by an update
	broken  map[string]bool
}

var compExtras sync.Map // *obState → *compExtra

func compExtraOf(st *obState) *compExtra {
	v, _ := compExtras.LoadOrStore(st, &compExtra{offer: map[string]string{}, updated: map[string]string{}, broken: map[string]bool{}})
	return v.(*compExtra)
}

// compDecorate is obCompHook: versions after an update, offered updates
// and repairs. Holds st.mu.
func compDecorate(st *obState, c *obComp) {
	x := compExtraOf(st)
	for _, t := range st.tasks {
		if t.kind == "update" && t.state == "done" && compHas(t.ids, c.ID) {
			if v, ok := x.offer[c.ID]; ok {
				x.updated[c.ID] = v
			}
			delete(x.offer, c.ID)
			delete(x.broken, c.ID)
		}
	}
	if c.State != "installed" {
		return
	}
	if v, ok := x.updated[c.ID]; ok {
		c.Version, c.Installed = v, v
	}
	if v, ok := x.offer[c.ID]; ok {
		c.UpdateVersion = v
	}
	if x.broken[c.ID] {
		c.State = "broken"
	}
}

func compHas(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func (m *mock) compSeed() {
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, id := range []string{"python", "asr-cuda", "model:faster-whisper:large-v3:float16", "asr-openvino"} {
		st.installed[id] = true
	}
	x := compExtraOf(st)
	x.offer["asr-cuda"] = "1.2.0-1"
	x.broken["asr-openvino"] = true
}

func (m *mock) compDemo(w http.ResponseWriter, r *http.Request, _ *store) {
	m.compSeed()
	w.WriteHeader(http.StatusNoContent)
}

func compCode(w http.ResponseWriter, status int, code string, params map[string]any) {
	codedError(w, status, code, params)
}

// compNewTask starts a task over ids (st.mu held).
func (st *obState) compNewTask(kind string, ids []string, source string, dur time.Duration) *obTask {
	st.seq++
	t := &obTask{id: "task-" + strconv.Itoa(st.seq), kind: kind, ids: ids, state: "running", runStart: time.Now(), failAt: -1, notes: []model.ErrorInfo{}}
	for _, id := range ids {
		c, _ := obFind(id)
		t.items = append(t.items, obItem{ID: id, Total: c.DownloadBytes, Source: source})
		t.total += c.DownloadBytes
	}
	t.speed = float64(t.total) / dur.Seconds()
	if t.speed <= 0 {
		t.speed = 1
	}
	st.tasks = append(st.tasks, t)
	return t
}

func (m *mock) compUpdate(w http.ResponseWriter, r *http.Request, _ *store) {
	id := r.PathValue("id")
	c, ok := obFind(id)
	if !ok {
		compCode(w, http.StatusBadRequest, "components.unknown", map[string]any{"id": id})
		return
	}
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.taskViews(time.Now()) // advance
	x := compExtraOf(st)
	switch {
	case c.Kind == "binary":
		compCode(w, http.StatusConflict, "components.bundled", map[string]any{"id": id})
		return
	case !st.installed[id]:
		compCode(w, http.StatusConflict, "components.not_installed", map[string]any{"id": id})
		return
	case st.installing()[id]:
		compCode(w, http.StatusConflict, "components.busy", map[string]any{"id": id})
		return
	}
	if _, offered := x.offer[id]; !offered && !x.broken[id] {
		v := c.Version
		if u, ok := x.updated[id]; ok {
			v = u
		}
		compCode(w, http.StatusConflict, "components.up_to_date", map[string]any{"id": id, "version": v})
		return
	}
	src := st.sources.PyPI.Mode
	if c.Kind == "model" {
		src = st.sources.Models.Mode
	}
	t := st.compNewTask("update", []string{id}, src, obInstallDuration/2)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": st.taskView(t, time.Now())})
}

func (m *mock) compUninstall(w http.ResponseWriter, r *http.Request, _ *store) {
	id := r.PathValue("id")
	c, ok := obFind(id)
	if !ok {
		compCode(w, http.StatusBadRequest, "components.unknown", map[string]any{"id": id})
		return
	}
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.taskViews(time.Now()) // advance
	if c.Kind == "binary" {
		compCode(w, http.StatusConflict, "components.bundled", map[string]any{"id": id})
		return
	}
	if !st.installed[id] {
		compCode(w, http.StatusConflict, "components.not_installed", map[string]any{"id": id})
		return
	}
	if st.installing()[id] {
		compCode(w, http.StatusConflict, "components.busy", map[string]any{"id": id})
		return
	}
	var deps []string
	for _, o := range obCatalog {
		if st.installed[o.ID] && compHas(o.Requires, id) {
			deps = append(deps, o.ID)
		}
	}
	if len(deps) > 0 {
		compCode(w, http.StatusConflict, "components.in_use", map[string]any{"id": id, "dependents": strings.Join(deps, ", ")})
		return
	}
	delete(st.installed, id)
	x := compExtraOf(st)
	delete(x.offer, id)
	delete(x.updated, id)
	delete(x.broken, id)
	w.WriteHeader(http.StatusNoContent)
}

// compImportID is the component every successful mock import installs.
const compImportID = "model:faster-whisper:medium:int8_float16"

func (m *mock) compImport(w http.ResponseWriter, r *http.Request, _ *store) {
	var name string
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" { // JSON {path} (desktop)
		var in struct {
			Path string `json:"path"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		if strings.TrimSpace(in.Path) == "" {
			compCode(w, http.StatusBadRequest, "request.bad_json", nil)
			return
		}
		name = filepath.Base(strings.ReplaceAll(in.Path, `\`, "/"))
	} else {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			compCode(w, http.StatusBadRequest, "upload.not_multipart", nil)
			return
		}
		f.Close()
		name = hdr.Filename
	}
	if !strings.EqualFold(filepath.Ext(name), ".zip") {
		compCode(w, http.StatusBadRequest, "components.import_invalid", nil)
		return
	}
	st := m.ob()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.taskViews(time.Now())
	if st.installing()[compImportID] {
		compCode(w, http.StatusConflict, "components.busy", map[string]any{"id": compImportID})
		return
	}
	t := st.compNewTask("import", []string{compImportID}, "offline", 4*time.Second)
	if strings.Contains(strings.ToLower(name), "fail") {
		t.state, t.base = "failed", t.total/2
		t.err = &model.ErrorInfo{Code: "components.import_checksum", Params: map[string]any{"id": compImportID}}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": st.taskView(t, time.Now())})
}
