// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// The Colab endpoints (SPEC-v3 V14, .claude/v3-brief.md §8). Every body is
// built from internal/colab's exported types, which never hold a token, a
// refresh token or a client secret; nothing here logs a request body.
// Without Deps.Colab (or without the scheduler) every endpoint answers 503
// server.unavailable {service: "colab"}.

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/colab", s.getColab)
		h("PUT /api/colab/settings", s.putColabSettings)
		h("POST /api/colab/auth/import", s.importColabAuth)
		h("POST /api/colab/auth/check", s.checkColabAuth)
		h("DELETE /api/colab/auth", s.deleteColabAuth)
		h("POST /api/colab/shutdown", s.shutdownColab)
		h("POST /api/lectures/{id}/use-colab", s.useColab)
		// the in-app Google sign-in (3.1.0, .claude/v31-brief.md §2.2)
		h("POST /api/colab/auth/login", s.startColabLogin)
		h("GET /api/colab/auth/login", s.getColabLogin)
		h("DELETE /api/colab/auth/login", s.cancelColabLogin)
		if s.colab != nil {
			s.colabLogin() // set up at startup: wipes what an interrupted sign-in left in the data directory
		}
	})
	diagSections = append(diagSections, DiagSection{Name: "colab.json", Collect: diagColab})
}

// ColabSettingsView is the settings part of GET /api/colab: Colab's own
// settings plus the scheduler's switches.
type ColabSettingsView struct {
	colab.Settings
	Enabled       bool `json:"enabled"`
	BatteryPrefer bool `json:"battery_prefer"`
}

// ColabADCView is "检测授权": is gcloud's credential file there and
// importable (Detected), and how to log in.
type ColabADCView struct {
	Detected     bool   `json:"detected"`
	Path         string `json:"path"`
	Found        bool   `json:"found"`
	Problem      string `json:"problem,omitempty"` // an error code (colab.adc_*)
	LoginCommand string `json:"login_command"`
}

// ColabView is GET /api/colab (and the answer of PUT /api/colab/settings).
type ColabView struct {
	Status   colab.Status      `json:"status"`
	Settings ColabSettingsView `json:"settings"`
	ADC      ColabADCView      `json:"adc"`
	// Held: "处理完当前这节后关机" / "立即关机" holds the automatic dispatch
	// until no lecture waits for Colab any more.
	Held bool `json:"held"`
}

// ColabSettingsPatch is PUT /api/colab/settings (absent = unchanged).
type ColabSettingsPatch struct {
	Accelerator       *string `json:"accelerator"`
	Tier              *string `json:"tier"`
	MaxQueueWaitMin   *int    `json:"max_queue_wait_min"`
	Model             *string `json:"model"`
	ComputeType       *string `json:"compute_type"`
	IdleReleaseMin    *int    `json:"idle_release_min"`
	WindowCooldownMin *int    `json:"window_cooldown_min"`
	HighMem           *bool   `json:"high_mem"`
	Enabled           *bool   `json:"enabled"`
	BatteryPrefer     *bool   `json:"battery_prefer"`
}

// ColabImportRequest is POST /api/colab/auth/import: the lifecycle the user
// chose (V14.2: "7 天后失效" = days 7, "仅本次会话" = session, "直到我手动
// 断开" = manual).
type ColabImportRequest struct {
	Lifecycle string `json:"lifecycle"` // days | session | manual
	Days      int    `json:"days"`      // days only: 1..90 (0 = 7)
}

// ColabLogin is the body of the /api/colab/auth/login endpoints (3.1.0,
// .claude/v31-brief.md §2.2): {"state": idle | waiting | importing | done |
// failed | cancelled, "url" (gcloud's sign-in link, "" until it printed
// one), "started_at" (RFC 3339 or null), "error" (ErrorInfo or null)}.
type ColabLogin = colab.LoginStatus

// ColabLoginRequest is the optional body of POST /api/colab/auth/login: the
// lifecycle of the credential the sign-in imports, as for
// POST /api/colab/auth/import (no body = 7 days).
type ColabLoginRequest = ColabImportRequest

// colabLoginCancelWait bounds DELETE /api/colab/auth/login's wait for
// gcloud to end.
const colabLoginCancelWait = 5 * time.Second

// ColabCheckView is POST /api/colab/auth/check.
type ColabCheckView struct {
	ADC  ColabADCView         `json:"adc"`
	Auth colab.CredentialInfo `json:"auth"`
}

// ColabDisconnectView is DELETE /api/colab/auth: Revoked (Google confirmed),
// Released (a runtime was given back first), Warnings (colab.revoke_failed,
// …) and Notes: remote.gcloud_signed_out when the revocation went through
// and the credential came from the user's own gcloud (source gcloud, also
// every 3.0.0 import: gcloud's application-default login used the same
// refresh token). A credential of the in-app sign-in (source app) was only
// ever in the app's own gcloud directory: no note.
type ColabDisconnectView struct {
	Revoked  bool              `json:"revoked"`
	Released bool              `json:"released"`
	Warnings []model.ErrorInfo `json:"warnings"`
	Notes    []model.ErrorInfo `json:"notes"`
}

// ColabShutdownRequest is POST /api/colab/shutdown.
type ColabShutdownRequest struct {
	Mode string `json:"mode"` // after_current | now
}

// colabReady answers 503 when the backend or the scheduler is missing.
func (s *Server) colabReady(w http.ResponseWriter) bool {
	if s.colab == nil || s.sc == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "colab"})
		return false
	}
	return true
}

func adcView(a colab.ADCInfo) ColabADCView {
	return ColabADCView{Detected: a.Found && a.Problem == "", Path: a.Path, Found: a.Found, Problem: a.Problem,
		LoginCommand: a.LoginCommand}
}

func (s *Server) colabView(ctx context.Context) (ColabView, error) {
	prefs, err := s.sc.RemotePrefs(ctx)
	if err != nil {
		return ColabView{}, err
	}
	return ColabView{
		Status:   s.colab.Status(ctx),
		Settings: ColabSettingsView{Settings: s.colab.Settings(), Enabled: prefs.Enabled, BatteryPrefer: prefs.BatteryPrefer},
		ADC:      adcView(s.colab.DetectADC()),
		Held:     s.sc.RemoteHeld(),
	}, nil
}

// GET /api/colab
func (s *Server) getColab(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	v, err := s.colabView(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// PUT /api/colab/settings: Colab's settings (validated by the backend:
// colab.settings_invalid {field}, 400) and the switches; the device table
// and the scheduler follow at once.
func (s *Server) putColabSettings(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	ctx := r.Context()
	var p ColabSettingsPatch
	if err := decodeJSON(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	set := s.colab.Settings()
	changed := false
	str := func(dst *string, v *string) {
		if v != nil && *v != *dst {
			*dst, changed = *v, true
		}
	}
	num := func(dst *int, v *int) {
		if v != nil && *v != *dst {
			*dst, changed = *v, true
		}
	}
	str(&set.Accelerator, p.Accelerator)
	str(&set.Tier, p.Tier)
	num(&set.MaxQueueWaitMin, p.MaxQueueWaitMin)
	str(&set.Model, p.Model)
	str(&set.ComputeType, p.ComputeType)
	num(&set.IdleReleaseMin, p.IdleReleaseMin)
	num(&set.WindowCooldownMin, p.WindowCooldownMin)
	if p.HighMem != nil && *p.HighMem != set.HighMem {
		set.HighMem, changed = *p.HighMem, true
	}
	if changed {
		if err := s.colab.SetSettings(ctx, set); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	prefs, err := s.sc.RemotePrefs(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	np := prefs
	if p.Enabled != nil {
		np.Enabled = *p.Enabled
	}
	if p.BatteryPrefer != nil {
		np.BatteryPrefer = *p.BatteryPrefer
	}
	if np != prefs {
		if err := s.sc.SetRemotePrefs(ctx, np); err != nil {
			s.fail(w, r, err)
			return
		}
	} else if changed {
		s.sc.RefreshRemote(ctx) // the row's label, model and precision follow
	}
	s.hub.Changed("settings", "")
	s.getColab(w, r)
}

// POST /api/colab/auth/import: the user's own gcloud application default
// credentials with the user's lifecycle (V14.2) → CredentialInfo (no
// secret; source gcloud).
func (s *Server) importColabAuth(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	ctx := r.Context()
	var req ColabImportRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	info, err := s.colab.ImportADC(ctx, importLifecycle(req))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.sc.RefreshRemote(ctx)
	s.hub.Changed("settings", "")
	writeJSON(w, http.StatusOK, info)
}

// importLifecycle is the lifecycle a request asks for (days without a
// count = 7 days; an unknown kind is refused by the backend).
func importLifecycle(req ColabImportRequest) colab.Lifecycle {
	lc := colab.Lifecycle{Kind: req.Lifecycle}
	switch req.Lifecycle {
	case "", colab.LifecycleDays:
		lc.Kind, lc.Days = colab.LifecycleDays, req.Days
		if lc.Days == 0 {
			lc.Days = 7
		}
	}
	return lc
}

// gcloudLookup finds the gcloud the sign-in runs: config.Config's
// GcloudPath (the gcloud component, else gcloud on PATH; v31-brief §2.1)
// when the configuration has it, else gcloud on PATH. Tests replace it.
var gcloudLookup = func(s *Server) (string, bool) {
	if s.cfg != nil {
		if g, ok := any(s.cfg).(interface{ GcloudPath() (string, bool) }); ok {
			return g.GcloudPath()
		}
	}
	p, err := exec.LookPath("gcloud")
	return p, err == nil && p != ""
}

// gcloudEnv is the environment gcloud runs with: config.Config's
// GcloudEnv (the process environment plus the gcloud component's
// variables, e.g. CLOUDSDK_PYTHON on macOS) when the configuration has it,
// else the process environment.
var gcloudEnv = func(s *Server) []string {
	if s.cfg != nil {
		if g, ok := any(s.cfg).(interface{ GcloudEnv() []string }); ok {
			return g.GcloudEnv()
		}
	}
	return os.Environ()
}

// colabLogin is the backend's in-app sign-in, in {data_dir}/colab/gcloud.
func (s *Server) colabLogin() *colab.Login {
	return s.colab.Login(colab.LoginOptions{
		ConfigDir: filepath.Join(s.absDataDir(), "colab", "gcloud"),
		Gcloud:    func() (string, bool) { return gcloudLookup(s) },
		Env:       func() []string { return gcloudEnv(s) },
		OnChange:  s.colabLoginChanged,
	})
}

// colabLoginChanged: every change is SSE changed {kind: "colab"}; a
// finished sign-in also changed the credential (the device table gains
// colab:remote, the settings page refetches), as POST /api/colab/auth/import.
func (s *Server) colabLoginChanged(st colab.LoginStatus) {
	if st.State == colab.LoginDone {
		if s.sc != nil {
			s.sc.RefreshRemote(context.Background())
		}
		s.hub.Changed("settings", "")
	}
	s.hub.Changed("colab", "")
}

// POST /api/colab/auth/login {"lifecycle", "days"} (optional): run gcloud's
// sign-in → 202 ColabLogin (waiting; failed colab.gcloud_start_failed when
// gcloud cannot be started). 409 colab.gcloud_missing (no gcloud component
// and no gcloud on PATH), 409 colab.login_running, 400 colab.lifecycle_invalid.
func (s *Server) startColabLogin(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	var req ColabLoginRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.colabLogin().Start(importLifecycle(req))
	if err != nil {
		switch code := colab.Code(err); code {
		case colab.CodeGcloudMissing, colab.CodeLoginRunning:
			writeErrorInfo(w, http.StatusConflict, model.ErrorInfo{Code: code})
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// GET /api/colab/auth/login → ColabLogin.
func (s *Server) getColabLogin(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.colabLogin().Status())
}

// DELETE /api/colab/auth/login: gcloud's process tree is stopped → ColabLogin
// (cancelled; unchanged when no sign-in was running).
func (s *Server) cancelColabLogin(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.colabLogin().Cancel(colabLoginCancelWait))
}

// POST /api/colab/auth/check: "检测授权" — the ADC file again, and the
// stored credential under its lifecycle (an expired one is deleted).
func (s *Server) checkColabAuth(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	ctx := r.Context()
	v := ColabCheckView{ADC: adcView(s.colab.DetectADC()), Auth: s.colab.Credential(ctx)}
	s.sc.RefreshRemote(ctx)
	writeJSON(w, http.StatusOK, v)
}

// DELETE /api/colab/auth: "断开连接" — the running lecture goes back to the
// queue, the runtime is released, the credential revoked at Google and
// deleted locally (V14.2).
func (s *Server) deleteColabAuth(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	ctx := r.Context()
	res, err := s.colab.Disconnect(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := ColabDisconnectView{Revoked: res.Revoked, Released: res.Released, Warnings: res.Warnings, Notes: []model.ErrorInfo{}}
	if v.Warnings == nil {
		v.Warnings = []model.ErrorInfo{}
	}
	if res.Revoked && res.Source != colab.SourceApp {
		v.Notes = append(v.Notes, model.ErrorInfo{Code: sched.CodeRemoteADCSignedOut})
	}
	s.sc.RefreshRemote(ctx)
	s.hub.Changed("settings", "")
	writeJSON(w, http.StatusOK, v)
}

// POST /api/colab/shutdown {"mode": "after_current" | "now"}.
func (s *Server) shutdownColab(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	ctx := r.Context()
	var req ColabShutdownRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	var now bool
	switch req.Mode {
	case "after_current":
	case "now":
		now = true
	default:
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "mode"})
		return
	}
	if err := s.sc.ShutdownRemote(ctx, now); err != nil {
		s.fail(w, r, err)
		return
	}
	s.getColab(w, r)
}

// POST /api/lectures/{id}/use-colab: V14.4 "改用 Colab 处理" (the
// transcription starts over on colab:remote). 409 remote.disabled, or
// remote.unavailable {waiting_for} when Colab cannot take it now; the
// lecture view otherwise.
func (s *Server) useColab(w http.ResponseWriter, r *http.Request) {
	if !s.colabReady(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.getLectureRow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sc.UseRemote(r.Context(), id); err != nil {
		if errors.Is(err, sched.ErrRemoteNotConfigured) {
			writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "colab"})
			return
		}
		s.fail(w, r, err)
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}

// remoteSnapshot is the resource bar's remote area (resources.Deps.Remote):
// Colab's state while it is switched on.
func RemoteSnapshot(b *colab.Backend, sc *sched.Scheduler) func(ctx context.Context) (colab.Status, bool) {
	return func(ctx context.Context) (colab.Status, bool) {
		if b == nil || sc == nil {
			return colab.Status{}, false
		}
		p, err := sc.RemotePrefs(ctx)
		if err != nil || !p.Enabled {
			return colab.Status{}, false
		}
		return b.Status(ctx), true
	}
}

// diagColab is the diagnostics' colab.json: state and settings, never a
// credential (CredentialInfo holds none) and not the ADC file's path.
func diagColab(ctx context.Context, s *Server) (any, bool) {
	if s.colab == nil || s.sc == nil {
		return nil, false
	}
	v, err := s.colabView(ctx)
	if err != nil {
		return map[string]string{"error": err.Error()}, true
	}
	v.ADC.Path = ""
	return v, true
}
