// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The 3.1.0 onboarding in the mock (.claude/v31-brief.md §1, §2): the
// "transcription mode" and "data folder" steps, the Colab branch of the
// download step (the gcloud tool component, signing in to Google) and the
// "cannot transcribe yet" notice.
//
//   - the gcloud component (kind "tool") is in v3_onboarding.go's catalog;
//     an install of tools only takes a quarter of the usual time;
//   - POST/GET/DELETE /api/colab/auth/login: POST answers 202 with state
//     waiting and a sign-in link, colabLoginWait later it is importing,
//     colabLoginImport after that done (the credential is imported as with
//     POST /api/colab/auth/import, 7 days, but with the source app: its
//     disconnect has no remote.gcloud_signed_out). 409 colab.gcloud_missing while
//     the gcloud component is not installed, 409 colab.login_running while
//     a sign-in waits or imports. DELETE cancels (state cancelled). Every
//     change of state sends SSE changed {kind: "colab"}, a successful one
//     also changed {kind: "settings"} (as the real server does).
//   - POST /api/app/data-dir/check {path}: the refusals of POST
//     /api/app/data-dir without moving (v3_settings.go checkTarget), with the
//     free space of the path's disk; GET /api/app/data-dir has free_bytes.
//   - GET /api/hardware and POST /api/hardware/detect have plan_error.
//
// Scenarios (?mock=…):
//
//	ob_storage       desktop mode: the onboarding has the "data folder" step
//	ob_storage_full  as ob_storage, and the data folder's disk has 2 GB free:
//	                 the recommended components do not fit
//	ob_noasr         desktop mode, no transcription component installed and
//	                 Colab not signed in: the workbench, the course page and
//	                 the onboarding's last step say "cannot transcribe yet"
//	ob_planerr       the recommendation names a model the manifest does not
//	                 have: plan_error components.unknown, the plan fails
//	ob_login_wait    the Google sign-in waits for ever (with a sign-in link)
//	ob_login_nourl   as ob_login_wait, without a link (gcloud printed none)
//	ob_login_fail    the sign-in fails after colabLoginWait: gcloud ended with
//	                 an error (colab.login_failed {exit_code})
//	ob_login_timeout the sign-in fails after colabLoginWait as after the real
//	                 ten minutes (colab.login_timeout {minutes})
//
// Free space by path (data-dir and check): a path containing "small" has
// 1.5 GB, one on D:, E: or under /Volumes 812 GB, the default 118.4 GB (2 GB
// in ob_storage_full and ob_disk). A path containing "busy", "notempty" is
// refused, "fail" fails while copying (v3_settings.go).

import (
	"net/http"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
	"lectureflow/internal/model"
)

// The simulated sign-in: waiting for the browser, then importing.
var (
	colabLoginWait   = 4 * time.Second
	colabLoginImport = 1200 * time.Millisecond
)

// The onboarding's data folder move (a fresh install: little data).
const (
	setFreshSize = int64(46 * 1024 * 1024)
	setFreshCopy = 2 * time.Second
)

// obPlanErrModel is the recommended model of ob_planerr that the mock
// manifest does not have.
const obPlanErrModel = "model:faster-whisper:large-v3:bfloat16"

const colabLoginURL = "https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=mock-client.apps.googleusercontent.com&redirect_uri=http%3A%2F%2Flocalhost%3A8085%2F&scope=openid&state=mock"

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		mux.HandleFunc("POST /api/colab/auth/login", a(m.colabLoginStart))
		mux.HandleFunc("GET /api/colab/auth/login", a(m.colabLoginGet))
		mux.HandleFunc("DELETE /api/colab/auth/login", a(m.colabLoginCancel))
	})
}

// obDesktopMode: the modes in which GET /api/app says "desktop".
func obDesktopMode(mode string) bool {
	switch mode {
	case "desktop", "storage", "storage_full", "noasr":
		return true
	}
	return false
}

// obToolsOnly: every id is a tool component (gcloud).
func obToolsOnly(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if c, ok := obFind(id); !ok || c.Kind != "tool" {
			return false
		}
	}
	return true
}

// appSetDataDir is the current data folder (it changes after a move).
func (m *mock) appSetDataDir() string {
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.dataDir
}

// ---------- data folder: free space, check ----------

// setDataDirView is GET /api/app/data-dir with free_bytes (3.1.0). The outer
// field wins over api.DataDirStatus's own once it has one.
type setDataDirView struct {
	api.DataDirStatus
	FreeBytes int64 `json:"free_bytes"`
}

// setFreeBytes is the free space of the disk of path in mode.
func setFreeBytes(path, mode string) int64 {
	lp := strings.ToLower(path)
	switch {
	case strings.Contains(lp, "small"):
		return 1_500_000_000
	case strings.HasPrefix(lp, `d:\`), strings.HasPrefix(lp, `e:\`), strings.HasPrefix(lp, "/volumes/"):
		return 812_000_000_000
	case strings.EqualFold(path, setDataDir) && (mode == "storage_full" || mode == "disk"):
		return int64(obDiskFreeMB(mode)) * obMB
	}
	return 118_400_000_000
}

type setCheckView struct {
	Path      string           `json:"path"`
	FreeBytes int64            `json:"free_bytes"`
	OK        bool             `json:"ok"`
	Error     *model.ErrorInfo `json:"error"`
}

func (m *mock) setCheckDir(w http.ResponseWriter, r *http.Request, _ *store) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	p := strings.TrimSpace(in.Path)
	st := m.appSet()
	st.mu.Lock()
	_, info := st.checkTarget(p)
	st.mu.Unlock()
	v := setCheckView{Path: p, FreeBytes: -1, OK: info == nil, Error: info}
	if info == nil || info.Code != "data_dir.path_invalid" {
		v.FreeBytes = setFreeBytes(p, obMode(r))
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------- Colab sign-in ----------

type colabLoginRun struct {
	state   string // waiting | importing | done | failed | cancelled
	url     string
	started time.Time
	forever bool             // ob_login_wait / ob_login_nourl: no browser ever answers
	fail    *model.ErrorInfo // ob_login_fail / ob_login_timeout: the error after the wait
	err     *model.ErrorInfo
}

// colabLoginView is ColabLogin of the brief.
type colabLoginView struct {
	State     string           `json:"state"` // idle | waiting | importing | done | failed | cancelled
	URL       string           `json:"url"`
	StartedAt *string          `json:"started_at"`
	Error     *model.ErrorInfo `json:"error"`
}

// loginAdvance moves the sign-in along the clock. c.mu is held. It reports
// whether the state changed.
func (c *colabMock) loginAdvance(now time.Time) bool {
	l := c.login
	if l == nil {
		return false
	}
	before := l.state
	if l.state == "waiting" && !l.forever && now.Sub(l.started) >= colabLoginWait {
		if l.fail != nil {
			l.state, l.err = "failed", l.fail
		} else {
			l.state = "importing"
		}
	}
	if l.state == "importing" && now.Sub(l.started) >= colabLoginWait+colabLoginImport {
		l.state = "done"
		c.auth = colabAuth("7d", 7, now, colab.SourceApp)
	}
	return l.state != before
}

func (c *colabMock) loginView(now time.Time) colabLoginView {
	c.loginAdvance(now)
	l := c.login
	if l == nil {
		return colabLoginView{State: "idle"}
	}
	at := l.started.UTC().Format(time.RFC3339)
	return colabLoginView{State: l.state, URL: l.url, StartedAt: &at, Error: l.err}
}

func (m *mock) colabLoginStart(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.ob()
	st.mu.Lock()
	now := time.Now()
	for _, t := range st.tasks {
		st.advance(t, now)
	}
	hasGcloud := st.installed["gcloud"]
	st.mu.Unlock()
	if !hasGcloud {
		codedError(w, http.StatusConflict, "colab.gcloud_missing", nil)
		return
	}
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loginAdvance(now)
	if l := c.login; l != nil && (l.state == "waiting" || l.state == "importing") {
		codedError(w, http.StatusConflict, "colab.login_running", nil)
		return
	}
	mode := obMode(r)
	l := &colabLoginRun{state: "waiting", url: colabLoginURL, started: now, forever: mode == "login_wait" || mode == "login_nourl"}
	switch mode {
	case "login_fail":
		l.fail = &model.ErrorInfo{Code: "colab.login_failed", Params: map[string]any{"exit_code": 1},
			Detail: "ERROR: (gcloud.auth.application-default.login) There was a problem with web authentication."}
	case "login_timeout":
		l.fail = &model.ErrorInfo{Code: "colab.login_timeout", Params: map[string]any{"minutes": 10}}
	}
	if mode == "login_nourl" {
		l.url = ""
	}
	c.login = l
	if !l.forever {
		// the SSE "changed colab" of each step (the page also polls)
		for _, d := range []time.Duration{colabLoginWait, colabLoginWait + colabLoginImport} {
			time.AfterFunc(d+20*time.Millisecond, func() {
				c.mu.Lock()
				changed := c.login == l && c.loginAdvance(time.Now())
				done := l.state == "done"
				c.mu.Unlock()
				if changed {
					s.notify(api.ChangedEvent{Kind: "colab"})
					if done { // the credential changed, as after "导入授权"
						s.notify(api.ChangedEvent{Kind: "settings"})
					}
				}
			})
		}
	}
	s.notify(api.ChangedEvent{Kind: "colab"})
	writeJSON(w, http.StatusAccepted, c.loginView(now))
}

func (m *mock) colabLoginGet(w http.ResponseWriter, r *http.Request, _ *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	writeJSON(w, http.StatusOK, c.loginView(time.Now()))
}

func (m *mock) colabLoginCancel(w http.ResponseWriter, r *http.Request, s *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.loginAdvance(now)
	if l := c.login; l != nil && (l.state == "waiting" || l.state == "importing") {
		l.state = "cancelled"
		s.notify(api.ChangedEvent{Kind: "colab"})
	}
	writeJSON(w, http.StatusOK, c.loginView(now))
}
