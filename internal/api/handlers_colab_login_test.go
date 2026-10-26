// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/colab/loginfake"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// 3.1.0 (.claude/v31-brief.md §2.2): POST/GET/DELETE /api/colab/auth/login
// run a fake gcloud (internal/colab/loginfake) against the fake OAuth
// server of newColabEnv — never the real gcloud, never Google.

// fakeGcloud makes the sign-in run a fake gcloud in mode; it returns the
// directory where the fake records its arguments.
func fakeGcloud(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	adc, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "client-id.apps.example",
		"client_secret": colabTestClient, "refresh_token": colabTestRefresh})
	gcloud, err := loginfake.Install(dir, string(adc))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(loginfake.EnvMode, mode)
	t.Setenv(loginfake.EnvOut, out)
	setGcloud(t, gcloud, true)
	return out
}

func setGcloud(t *testing.T, path string, ok bool) {
	t.Helper()
	prev := gcloudLookup
	gcloudLookup = func(*Server) (string, bool) { return path, ok }
	t.Cleanup(func() { gcloudLookup = prev })
}

// loginStatus is GET /api/colab/auth/login (raw body too).
func (ce *colabEnv) loginStatus() (ColabLogin, []byte) {
	ce.t.Helper()
	res, b := ce.do("GET", "/api/colab/auth/login", nil)
	if res.StatusCode != http.StatusOK {
		ce.t.Fatalf("GET login: %d %s", res.StatusCode, b)
	}
	ce.noSecrets("GET login", b)
	var st ColabLogin
	if err := json.Unmarshal(b, &st); err != nil {
		ce.t.Fatal(err)
	}
	return st, b
}

func (ce *colabEnv) waitLogin(what string, cond func(ColabLogin) bool) ColabLogin {
	ce.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, _ := ce.loginStatus()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			ce.t.Fatalf("%s: login stayed %+v", what, st)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func loginEnded(st ColabLogin) bool {
	return st.State == colab.LoginDone || st.State == colab.LoginFailed || st.State == colab.LoginCancelled
}

func changedKind(kind string) func(sseFrame) bool {
	return func(f sseFrame) bool {
		var ev ChangedEvent
		return f.event == "changed" && json.Unmarshal([]byte(f.data), &ev) == nil && ev.Kind == kind
	}
}

func TestColabLoginWithoutBackend(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"POST", "GET", "DELETE"} {
		info := e.callErrInfo(m, "/api/colab/auth/login", nil, http.StatusServiceUnavailable)
		if info.Code != "server.unavailable" || info.Params["service"] != "colab" {
			t.Fatalf("%s: %+v", m, info)
		}
	}
}

// The sign-in end to end: idle → waiting (link) → importing → done; the
// credential is stored with the requested lifecycle, colab:remote joins the
// device table, SSE says changed colab (and settings), nothing secret
// leaves, the sign-in's directory is gone.
func TestColabLoginFlow(t *testing.T) {
	ce := newColabEnv(t)
	r := openSSE(t, ce.ts.URL+"/api/events")
	waitClients(t, ce.hub, 1)

	st, raw := ce.loginStatus()
	if st.State != colab.LoginIdle || st.URL != "" || st.StartedAt != nil || st.Error != nil ||
		!bytes.Contains(raw, []byte(`"started_at":null`)) || !bytes.Contains(raw, []byte(`"error":null`)) || !bytes.Contains(raw, []byte(`"url":""`)) {
		t.Fatalf("initial login %s", raw)
	}

	// No gcloud: 409 colab.gcloud_missing; nothing changes.
	setGcloud(t, "", false)
	if info := ce.callErrInfo("POST", "/api/colab/auth/login", nil, http.StatusConflict); info.Code != colab.CodeGcloudMissing {
		t.Fatalf("without gcloud: %+v", info)
	}
	out := fakeGcloud(t, loginfake.ModeSuccess)
	if info := ce.callErrInfo("POST", "/api/colab/auth/login", map[string]any{"lifecycle": "forever"}, http.StatusBadRequest); info.Code != colab.CodeLifecycleInvalid {
		t.Fatalf("bad lifecycle: %+v", info)
	}
	res, b := ce.doWith("POST", "/api/colab/auth/login", []byte("{"), "", "")
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "request.bad_json" {
		t.Fatalf("bad JSON: %d %s", res.StatusCode, b)
	}
	if st, _ := ce.loginStatus(); st.State != colab.LoginIdle {
		t.Fatalf("a refused start changed the login: %+v", st)
	}

	var v ColabView
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": true}, http.StatusOK, &v)
	res, b = ce.do("POST", "/api/colab/auth/login", map[string]any{"lifecycle": "days", "days": 3})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST login: %d %s", res.StatusCode, b)
	}
	if err := json.Unmarshal(b, &st); err != nil || st.State != colab.LoginWaiting || st.StartedAt == nil || st.Error != nil {
		t.Fatalf("POST login = %s (%v)", b, err)
	}
	r.next(5*time.Second, changedKind("colab"))
	st = ce.waitLogin("sign-in", loginEnded)
	if st.State != colab.LoginDone || st.URL != loginfake.URL || st.Error != nil {
		t.Fatalf("final login %+v", st)
	}
	r.next(5*time.Second, changedKind("settings"))
	var chk ColabCheckView
	ce.call("POST", "/api/colab/auth/check", nil, http.StatusOK, &chk)
	if !chk.Auth.Present || chk.Auth.Lifecycle != "3d" || chk.Auth.Source != colab.SourceApp {
		t.Fatalf("credential after the sign-in %+v", chk.Auth)
	}
	ce.call("GET", "/api/colab", nil, http.StatusOK, &v)
	if v.Status.Auth.Source != colab.SourceApp {
		t.Fatalf("GET /api/colab auth after the sign-in %+v", v.Status.Auth)
	}
	if !ce.reg.Present(model.DeviceColab) {
		t.Fatal("colab:remote is not in the device table after the sign-in")
	}
	if args, err := os.ReadFile(filepath.Join(out, "args.txt")); err != nil ||
		strings.TrimSpace(string(args)) != strings.Join(colab.LoginArgs(), " ") {
		t.Fatalf("gcloud args %q (%v)", args, err)
	}
	if _, err := os.Stat(filepath.Join(ce.cfg.DataDir, "colab", "gcloud")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the sign-in directory is still there: %v", err)
	}

	// Disconnecting the sign-in's credential: revoked and deleted, but no
	// remote.gcloud_signed_out — the user's own gcloud never held it.
	res, b = ce.do("DELETE", "/api/colab/auth", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE auth: %d %s", res.StatusCode, b)
	}
	var dv ColabDisconnectView
	if err := json.Unmarshal(b, &dv); err != nil || !dv.Revoked || len(dv.Notes) != 0 || !bytes.Contains(b, []byte(`"notes":[]`)) || ce.revoked != 1 {
		t.Fatalf("disconnect after the sign-in = %s (revocations %d)", b, ce.revoked)
	}
	if ce.b.Credential(t.Context()).Present {
		t.Fatal("the credential survived the disconnect")
	}
	ce.noSecrets("logs", []byte(ce.logs.String()))
}

// DELETE /api/colab/auth's gcloud note by the credential's source: the
// in-app sign-in (above: none), the user's own gcloud (TestColabEndpoints:
// the note) and a credential 3.0.0 imported, which has no source and counts
// as gcloud (the note).
func TestColabDisconnectNoteOldCredential(t *testing.T) {
	ce := newColabEnv(t)
	ctx := t.Context()
	raw, _ := json.Marshal(map[string]any{"v": 1, "client_id": "client-id.apps.example", "client_secret": colabTestClient,
		"refresh_token": colabTestRefresh, "lifecycle": map[string]any{"kind": "manual"},
		"imported_at": time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)})
	if err := ce.st.SetKV(ctx, colab.KVCredential, string(raw)); err != nil {
		t.Fatal(err)
	}
	var v ColabView
	res, b := ce.do("GET", "/api/colab", nil)
	if res.StatusCode != http.StatusOK || json.Unmarshal(b, &v) != nil || !v.Status.Auth.Present || v.Status.Auth.Source != colab.SourceGcloud {
		t.Fatalf("3.0.0 credential: %d %s", res.StatusCode, b)
	}
	ce.noSecrets("GET /api/colab", b)
	var dv ColabDisconnectView
	ce.call("DELETE", "/api/colab/auth", nil, http.StatusOK, &dv)
	if !dv.Revoked || len(dv.Notes) != 1 || dv.Notes[0].Code != sched.CodeRemoteADCSignedOut || ce.revoked != 1 {
		t.Fatalf("disconnect of a 3.0.0 credential = %+v (revocations %d)", dv, ce.revoked)
	}
	// Nothing stored: nothing revoked, no note.
	ce.call("DELETE", "/api/colab/auth", nil, http.StatusOK, &dv)
	if dv.Revoked || len(dv.Notes) != 0 || ce.revoked != 1 {
		t.Fatalf("disconnect without a credential = %+v (revocations %d)", dv, ce.revoked)
	}
}

// A sign-in waiting for the browser: a second start is 409
// colab.login_running; DELETE stops gcloud (cancelled) within seconds.
func TestColabLoginCancel(t *testing.T) {
	ce := newColabEnv(t)
	fakeGcloud(t, loginfake.ModeHang)
	var st ColabLogin
	ce.call("POST", "/api/colab/auth/login", nil, http.StatusAccepted, &st)
	ce.waitLogin("link", func(s ColabLogin) bool { return s.URL == loginfake.URL })
	if info := ce.callErrInfo("POST", "/api/colab/auth/login", nil, http.StatusConflict); info.Code != colab.CodeLoginRunning {
		t.Fatalf("second start: %+v", info)
	}
	t0 := time.Now()
	ce.call("DELETE", "/api/colab/auth/login", nil, http.StatusOK, &st)
	if st.State != colab.LoginCancelled || st.Error != nil {
		t.Fatalf("after DELETE %+v", st)
	}
	if d := time.Since(t0); d > colabLoginCancelWait {
		t.Errorf("DELETE took %v", d)
	}
	if ce.b.Credential(t.Context()).Present {
		t.Fatal("a credential after a cancelled sign-in")
	}
	// DELETE without a running sign-in returns the status unchanged.
	ce.call("DELETE", "/api/colab/auth/login", nil, http.StatusOK, &st)
	if st.State != colab.LoginCancelled {
		t.Fatalf("second DELETE %+v", st)
	}
}

// gcloud ending with an error: failed colab.login_failed {exit_code} with
// gcloud's ERROR line as the detail.
func TestColabLoginFailed(t *testing.T) {
	ce := newColabEnv(t)
	fakeGcloud(t, loginfake.ModeFail)
	var st ColabLogin
	ce.call("POST", "/api/colab/auth/login", map[string]any{"lifecycle": "manual"}, http.StatusAccepted, &st)
	st = ce.waitLogin("failure", loginEnded)
	if st.State != colab.LoginFailed || st.Error == nil || st.Error.Code != colab.CodeLoginFailed ||
		st.Error.Params["exit_code"] != float64(1) || st.Error.Detail != loginfake.ErrorLine {
		t.Fatalf("login %+v / %+v", st, st.Error)
	}
	// Its params are the catalog's.
	if msg := checkErrorBody(mustJSONBytes(t, ErrorBody{Error: *st.Error})); msg != "" {
		t.Fatal(msg)
	}
	// A new sign-in may start after a failure.
	fakeGcloud(t, loginfake.ModeSuccess)
	ce.call("POST", "/api/colab/auth/login", nil, http.StatusAccepted, &st)
	if st = ce.waitLogin("retry", loginEnded); st.State != colab.LoginDone {
		t.Fatalf("retry %+v", st)
	}
	if info := ce.b.Credential(t.Context()); info.Lifecycle != "7d" {
		t.Fatalf("default lifecycle %+v", info)
	}
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
