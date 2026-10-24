// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Desktop mode and the application endpoints of SPEC-v3 V1.1, V1.2, V2
// (.claude/v3-brief.md section 2): the access token and Host check of the
// desktop build, the security headers of both modes, and /api/health,
// /api/app, /api/app/settings, /api/app/activity, /api/app/shutdown,
// /api/onboarding.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// DesktopOptions are set by cmd in desktop mode (SPEC-v3 V1.2); nil = web mode.
type DesktopOptions struct {
	Token        string // every /api/ request must carry it in X-LF-Token
	Port         int    // Host header check: 127.0.0.1:Port or localhost:Port
	ResourcesDir string
	Shutdown     func() // POST /api/app/shutdown: start the graceful stop (returns at once)
	Restart      func() // graceful stop, then exit code 3 (step 5: data dir change / import)
	// LocationFile is {default data dir}/location.json ({"data_dir": "..."},
	// v3-brief §3): written when the user moves the data dir (V1.3); ""
	// when the data dir was given by LECTUREFLOW_DATA_DIR (no move then).
	LocationFile string
}

// TokenHeader is the request header carrying the desktop access token.
const TokenHeader = "X-LF-Token"

// contentSecurityPolicy is sent on every response in both modes (SPEC-v3 V2).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// kv keys of the application settings (V1.1, V11) and the onboarding state (V4).
const (
	kvAppCloseBehavior = model.KVAppCloseBehavior
	kvAppPreventSleep  = model.KVAppPreventSleep
	kvAppUpdateChannel = model.KVAppUpdateChannel
	kvOnboardingState  = model.KVOnboardingState
)

// Values of the application settings.
var (
	closeBehaviors = []string{"ask", "background", "quit"}
	updateChannels = []string{"stable", "beta"}
)

const maxOnboardingBytes = 64 << 10

// ---- middleware ----

// secure adds the security headers to every response and, in desktop mode,
// checks the Host header of every request and the token of every /api/
// request. The static front end needs no token: the window loads it before
// any script runs, and the main process injects the header into the API
// requests (fetch, EventSource, <video>).
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", contentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		if d := s.desktop; d != nil {
			if !d.hostAllowed(r.Host) {
				writeError(w, http.StatusForbidden, "auth.bad_host", nil)
				return
			}
			if (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) && !d.tokenOK(r.Header.Get(TokenHeader)) {
				writeError(w, http.StatusUnauthorized, "auth.token_required", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed: 127.0.0.1:Port or localhost:Port (DNS rebinding, V1.2).
func (d *DesktopOptions) hostAllowed(host string) bool {
	p := ":" + strconv.Itoa(d.Port)
	h := strings.ToLower(host)
	return h == "127.0.0.1"+p || h == "localhost"+p
}

// tokenOK compares in constant time; an empty configured token accepts nothing.
func (d *DesktopOptions) tokenOK(got string) bool {
	if d.Token == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.Token)) == 1
}

func (s *Server) mode() string {
	if s.desktop != nil {
		return "desktop"
	}
	return "web"
}

func (s *Server) appVersion() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// ---- GET /api/health, GET /api/app ----

// Health is GET /api/health.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Mode    string `json:"mode"` // desktop | web
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Health{OK: true, Version: s.appVersion(), Mode: s.mode()})
}

// AppInfo is GET /api/app.
type AppInfo struct {
	Version        string `json:"version"`
	Mode           string `json:"mode"`     // desktop | web
	Platform       string `json:"platform"` // windows | darwin | linux (runtime.GOOS)
	Arch           string `json:"arch"`     // runtime.GOARCH
	DataDir        string `json:"data_dir"`
	ResourcesDir   string `json:"resources_dir"` // "" in web mode
	OnboardingDone bool   `json:"onboarding_done"`
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	ob, err := s.onboarding(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir := s.dataDir
	if a, err := filepath.Abs(dir); err == nil && dir != "" {
		dir = a
	}
	info := AppInfo{Version: s.appVersion(), Mode: s.mode(), Platform: runtime.GOOS, Arch: runtime.GOARCH,
		DataDir: dir, OnboardingDone: ob.Done}
	if s.desktop != nil {
		info.ResourcesDir = s.desktop.ResourcesDir
	}
	writeJSON(w, http.StatusOK, info)
}

// ---- /api/app/settings ----

// AppSettings is GET/PATCH /api/app/settings (V1.1, V11).
type AppSettings struct {
	CloseBehavior string `json:"close_behavior"` // ask | background | quit
	PreventSleep  bool   `json:"prevent_sleep"`
	UpdateChannel string `json:"update_channel"` // stable | beta
}

// AppSettingsPatch is the body of PATCH /api/app/settings (omitted = unchanged).
type AppSettingsPatch struct {
	CloseBehavior *string `json:"close_behavior"`
	PreventSleep  *bool   `json:"prevent_sleep"`
	UpdateChannel *string `json:"update_channel"`
}

func oneOf(v string, list []string) bool {
	for _, x := range list {
		if v == x {
			return true
		}
	}
	return false
}

// kvGet returns the value of a kv key ("" and false when missing).
func (s *Server) kvGet(ctx context.Context, key string) (string, bool, error) {
	v, ok, err := s.st.GetKV(ctx, key)
	if err != nil {
		return "", false, err
	}
	return v, ok, nil
}

// appSettings reads the settings; a missing or unknown stored value is the default.
func (s *Server) appSettings(ctx context.Context) (AppSettings, error) {
	out := AppSettings{CloseBehavior: "ask", PreventSleep: true, UpdateChannel: "stable"}
	v, ok, err := s.kvGet(ctx, kvAppCloseBehavior)
	if err != nil {
		return out, err
	}
	if ok && oneOf(v, closeBehaviors) {
		out.CloseBehavior = v
	}
	if v, ok, err = s.kvGet(ctx, kvAppPreventSleep); err != nil {
		return out, err
	}
	if ok && v == "0" {
		out.PreventSleep = false
	}
	if v, ok, err = s.kvGet(ctx, kvAppUpdateChannel); err != nil {
		return out, err
	}
	if ok && oneOf(v, updateChannels) {
		out.UpdateChannel = v
	}
	return out, nil
}

func (s *Server) getAppSettings(w http.ResponseWriter, r *http.Request) {
	out, err := s.appSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /api/app/settings: every given field is validated before any is
// written; 400 app_settings.close_behavior_invalid / update_channel_invalid
// {value}, request.nothing_to_change for an empty patch.
func (s *Server) patchAppSettings(w http.ResponseWriter, r *http.Request) {
	var in AppSettingsPatch
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.CloseBehavior == nil && in.PreventSleep == nil && in.UpdateChannel == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change",
			map[string]any{"fields": "close_behavior, prevent_sleep, update_channel"})
		return
	}
	if in.CloseBehavior != nil && !oneOf(*in.CloseBehavior, closeBehaviors) {
		writeError(w, http.StatusBadRequest, "app_settings.close_behavior_invalid", map[string]any{"value": *in.CloseBehavior})
		return
	}
	if in.UpdateChannel != nil && !oneOf(*in.UpdateChannel, updateChannels) {
		writeError(w, http.StatusBadRequest, "app_settings.update_channel_invalid", map[string]any{"value": *in.UpdateChannel})
		return
	}
	ctx := r.Context()
	var writes [][2]string
	if in.CloseBehavior != nil {
		writes = append(writes, [2]string{kvAppCloseBehavior, *in.CloseBehavior})
	}
	if in.PreventSleep != nil {
		v := "0"
		if *in.PreventSleep {
			v = "1"
		}
		writes = append(writes, [2]string{kvAppPreventSleep, v})
	}
	if in.UpdateChannel != nil {
		writes = append(writes, [2]string{kvAppUpdateChannel, *in.UpdateChannel})
	}
	for _, kv := range writes {
		if err := s.st.SetKV(ctx, kv[0], kv[1]); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.hub.Changed("settings", "")
	s.getAppSettings(w, r)
}

// ---- GET /api/app/activity ----

// AppActivity is GET /api/app/activity, polled by the main process every 5
// seconds (the close-window question and the sleep blocker, V1.1).
type AppActivity struct {
	Running      int  `json:"running"`      // current jobs in status running
	Queued       int  `json:"queued"`       // current jobs in status queued
	Transcribing bool `json:"transcribing"` // an asr job or a device self-test runs
	APIRunning   bool `json:"api_running"`  // a job calling a model (an API stage) runs
}

func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListCurrentJobsByStatus(r.Context(), model.StatusRunning, model.StatusQueued)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var out AppActivity
	for _, j := range jobs {
		if j.Status == model.StatusQueued {
			out.Queued++
			continue
		}
		out.Running++
		if j.Stage == model.StageASR {
			out.Transcribing = true
		}
		if balanceStages[j.Stage] {
			out.APIRunning = true
		}
	}
	if s.dsc != nil && !out.Transcribing {
		for _, d := range s.dsc.DeviceStatuses() {
			if d.SelfTest != nil && d.SelfTest.State == sched.DeviceTestRunning {
				out.Transcribing = true
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- POST /api/app/shutdown ----

// POST /api/app/shutdown: desktop mode only (404 api.not_found in web
// mode); starts the graceful stop and answers 202 at once.
func (s *Server) postShutdown(w http.ResponseWriter, r *http.Request) {
	if s.desktop == nil || s.desktop.Shutdown == nil {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.desktop.Shutdown()
}

// ---- /api/onboarding ----

// OnboardingState is GET/PUT /api/onboarding (V4; the steps are step 6's).
// Stored as sent in kv onboarding.state; data is free-form for the wizard.
type OnboardingState struct {
	Step string          `json:"step"`
	Done bool            `json:"done"`
	Data json.RawMessage `json:"data"`
}

func defaultOnboarding() OnboardingState {
	return OnboardingState{Step: "welcome", Data: json.RawMessage("{}")}
}

// parseOnboarding validates a state: a JSON object whose step is a
// non-empty string, done a boolean and data (optional) an object.
func parseOnboarding(b []byte) (OnboardingState, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return OnboardingState{}, apiErr(http.StatusBadRequest, "request.bad_json", nil)
	}
	bad := func(field string) error {
		return apiErr(http.StatusBadRequest, "onboarding_state.invalid", map[string]any{"field": field})
	}
	out := defaultOnboarding()
	if err := json.Unmarshal(raw["step"], &out.Step); err != nil || raw["step"] == nil || strings.TrimSpace(out.Step) == "" || len(out.Step) > 64 {
		return out, bad("step")
	}
	v, ok := raw["done"]
	if !ok || (string(v) != "true" && string(v) != "false") {
		return out, bad("done")
	}
	out.Done = string(v) == "true"
	if d, ok := raw["data"]; ok && string(d) != "null" {
		if t := bytes.TrimSpace(d); len(t) == 0 || t[0] != '{' {
			return out, bad("data")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, d); err != nil {
			return out, bad("data")
		}
		out.Data = buf.Bytes()
	}
	return out, nil
}

// onboarding reads the stored state; a missing or unreadable value is the
// initial state (not done).
func (s *Server) onboarding(ctx context.Context) (OnboardingState, error) {
	v, ok, err := s.kvGet(ctx, kvOnboardingState)
	if err != nil {
		return defaultOnboarding(), err
	}
	if !ok {
		return defaultOnboarding(), nil
	}
	st, err := parseOnboarding([]byte(v))
	if err != nil {
		return defaultOnboarding(), nil
	}
	return st, nil
}

func (s *Server) getOnboarding(w http.ResponseWriter, r *http.Request) {
	st, err := s.onboarding(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// PUT /api/onboarding OnboardingState: replaces the state; 400
// onboarding_state.invalid {field} (step, done, data), onboarding_state.too_large.
func (s *Server) putOnboarding(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxOnboardingBytes+1))
	if err != nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_json", nil))
		return
	}
	if len(b) > maxOnboardingBytes {
		writeError(w, http.StatusBadRequest, "onboarding_state.too_large", map[string]any{"max_bytes": maxOnboardingBytes})
		return
	}
	st, err := parseOnboarding(b)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	enc, err := json.Marshal(st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.st.SetKV(r.Context(), kvOnboardingState, string(enc)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
