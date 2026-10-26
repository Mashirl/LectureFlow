// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// SPEC-v3 V14 (v14-colab-integrate): the /api/colab endpoints, "改用 Colab
// 处理", the remote area of the resources and the diagnostics section. The
// credential's secrets never appear in a response or a log line.

const (
	colabTestRefresh = "1//refresh-SECRET-7f3a9c"
	colabTestClient  = "client-SECRET-b81d"
	colabTestAccess  = "ya29.access-SECRET-55e0"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type colabEnv struct {
	*env
	b       *colab.Backend
	reg     *devices.Registry
	revoked int
	logs    *syncBuf
}

func newColabEnv(t *testing.T) *colabEnv {
	t.Helper()
	ce := &colabEnv{logs: &syncBuf{}}
	logger := slog.New(slog.NewTextHandler(ce.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	var mu sync.Mutex
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/revoke") {
			mu.Lock()
			ce.revoked++
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"`+colabTestAccess+`","expires_in":3599,"token_type":"Bearer",`+
			`"scope":"openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/colaboratory"}`)
	}))
	t.Cleanup(oauth.Close)
	adc := filepath.Join(t.TempDir(), "application_default_credentials.json")
	raw, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "client-id.apps.example",
		"client_secret": colabTestClient, "refresh_token": colabTestRefresh})
	if err := os.WriteFile(adc, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ce.env = newEnvWith(t, envConfig{
		sched: func(o *sched.Options) {
			ce.reg = devices.NewRegistry(o.Store, o.Config, o.DataDir, devices.RegistryOptions{
				CUDAName: func(context.Context) (string, error) { return "NVIDIA Test GPU", nil },
				Probe:    func(context.Context) (*devices.OVProbe, error) { return nil, errors.New("no OpenVINO in tests") },
				MLX:      func(context.Context) (string, bool) { return "", false },
			})
			if err := ce.reg.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ce.reg.Detect(context.Background()); err != nil {
				t.Fatal(err)
			}
			b, err := colab.New(colab.Options{KV: o.Store, TokenURL: oauth.URL + "/token", RevokeURL: oauth.URL + "/revoke",
				ColabURL: oauth.URL + "/colab", ADCPath: adc, Logger: logger, AllowHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Startup(context.Background()); err != nil {
				t.Fatal(err)
			}
			ce.b = b
			o.Devices, o.Remote = ce.reg, b
			o.OnRemote = func(ctx context.Context, present bool, row model.ASRDevice) {
				if err := ce.reg.SetRemote(ctx, present, row); err != nil {
					t.Error(err)
				}
			}
		},
		server: func(s *Server) { s.colab = ce.b },
	})
	return ce
}

// noSecrets fails when a response or the logs carry a secret.
func (ce *colabEnv) noSecrets(what string, b []byte) {
	ce.t.Helper()
	for _, s := range []string{colabTestRefresh, colabTestClient, colabTestAccess, "refresh-SECRET", "client-SECRET", "access-SECRET"} {
		if bytes.Contains(b, []byte(s)) {
			ce.t.Fatalf("%s carries a secret: %s", what, b)
		}
	}
}

func TestColabEndpointsWithoutBackend(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/colab"}, {"PUT", "/api/colab/settings"}, {"POST", "/api/colab/auth/import"},
		{"POST", "/api/colab/auth/check"}, {"DELETE", "/api/colab/auth"}, {"POST", "/api/colab/shutdown"},
	} {
		info := e.callErrInfo(c.method, c.path, map[string]any{}, http.StatusServiceUnavailable)
		if info.Code != "server.unavailable" || info.Params["service"] != "colab" {
			t.Fatalf("%s %s: %+v", c.method, c.path, info)
		}
	}
	c := e.course("CSC1003")
	l := e.lecture(c.ID, "W1")
	if info := e.callErrInfo("POST", "/api/lectures/"+l.ID+"/use-colab", nil, http.StatusServiceUnavailable); info.Code != "server.unavailable" {
		t.Fatalf("use-colab without Colab: %+v", info)
	}
}

func TestColabEndpoints(t *testing.T) {
	ce := newColabEnv(t)
	ctx := context.Background()

	var v ColabView
	ce.call("GET", "/api/colab", nil, http.StatusOK, &v)
	if v.Settings.Enabled || v.Settings.BatteryPrefer || v.Status.Configured || !v.ADC.Detected || v.Settings.Accelerator != "T4" || v.Held {
		t.Fatalf("initial view = %+v", v)
	}

	// Settings: partial, validated by the backend, switches in kv.
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": true, "battery_prefer": true, "accelerator": "L4", "tier": "free"},
		http.StatusOK, &v)
	if !v.Settings.Enabled || !v.Settings.BatteryPrefer || v.Settings.Accelerator != "L4" || v.Settings.Tier != "free" ||
		v.Settings.Model != "large-v3" {
		t.Fatalf("view after PUT = %+v", v)
	}
	if val, ok, _ := ce.st.GetKV(ctx, model.KVColabEnabled); !ok || val != "1" {
		t.Fatalf("kv %s = %q %v", model.KVColabEnabled, val, ok)
	}
	if info := ce.callErrInfo("PUT", "/api/colab/settings", map[string]any{"tier": "gold"}, http.StatusBadRequest); info.Code != colab.CodeSettingsInvalid ||
		info.Params["field"] != "tier" {
		t.Fatalf("bad tier: %+v", info)
	}

	// Enabled but no credential: not in the device table; use-colab says why.
	if ce.reg.Present(model.DeviceColab) {
		t.Fatal("colab:remote present without a credential")
	}
	c := ce.course("CSC1003")
	l := ce.lecture(c.ID, "W1")
	ce.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	ce.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	info := ce.callErrInfo("POST", "/api/lectures/"+l.ID+"/use-colab", nil, http.StatusConflict)
	if info.Code != sched.CodeRemoteUnavailable || info.Params["waiting_for"] != model.WaitRemoteAuth {
		t.Fatalf("use-colab without a credential: %+v", info)
	}

	// Import with a lifecycle: CredentialInfo only; colab:remote appears.
	res, b := ce.do("POST", "/api/colab/auth/import", map[string]any{"lifecycle": "days", "days": 3})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	ce.noSecrets("import", b)
	var auth colab.CredentialInfo
	if err := json.Unmarshal(b, &auth); err != nil || !auth.Present || auth.Lifecycle != "3d" || auth.ExpiresAt == nil ||
		auth.Source != colab.SourceGcloud {
		t.Fatalf("import = %s (%v)", b, err)
	}
	if !ce.reg.Present(model.DeviceColab) {
		t.Fatal("colab:remote not in the device table after the import")
	}
	if d, _ := ce.reg.Get(model.DeviceColab); d.Label != "Google Colab · L4" || d.Model != "large-v3" || d.Precision != "float16" {
		t.Fatalf("colab:remote row = %+v", d)
	}
	if info := ce.callErrInfo("POST", "/api/colab/auth/import", map[string]any{"lifecycle": "forever"}, http.StatusBadRequest); info.Code != colab.CodeLifecycleInvalid {
		t.Fatalf("bad lifecycle: %+v", info)
	}

	var chk ColabCheckView
	ce.call("POST", "/api/colab/auth/check", nil, http.StatusOK, &chk)
	if !chk.ADC.Detected || !chk.Auth.Present || chk.Auth.Lifecycle != "3d" {
		t.Fatalf("check = %+v", chk)
	}
	res, b = ce.do("GET", "/api/colab", nil)
	ce.noSecrets("GET /api/colab", b)
	if err := json.Unmarshal(b, &v); err != nil || !v.Status.Configured || !v.Status.Auth.Present || v.Status.Auth.Source != colab.SourceGcloud {
		t.Fatalf("view after import: %d %s", res.StatusCode, b)
	}

	// use-colab: the queued transcription starts over on Colab, pinned.
	var lv Lecture
	ce.call("POST", "/api/lectures/"+l.ID+"/use-colab", nil, http.StatusOK, &lv)
	j, err := ce.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageASR)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !j.Stats.RemotePin || snap.DeviceID != model.DeviceColab || snap.Backend != model.BackendColab || j.Status != model.StatusQueued {
		t.Fatalf("asr after use-colab: %s %+v %+v", j.Status, j.Stats, snap)
	}
	// A done transcription cannot be moved.
	l2 := ce.lecture(c.ID, "W2")
	ce.moveJob(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	ce.moveJob(model.ScopeLecture, l2.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusDone)
	if info := ce.callErrInfo("POST", "/api/lectures/"+l2.ID+"/use-colab", nil, http.StatusConflict); info.Code != "state.conflict" {
		t.Fatalf("use-colab of a done asr: %+v", info)
	}
	if info := ce.callErrInfo("POST", "/api/lectures/nope/use-colab", nil, http.StatusNotFound); info.Code == "" {
		t.Fatal("no code")
	}

	// The resource bar's remote area follows the switch.
	if st, ok := RemoteSnapshot(ce.b, ce.sc)(ctx); !ok || st.Accelerator != "L4" || !st.Configured {
		t.Fatalf("remote area = %+v %v", st, ok)
	}

	// Diagnostics: state and settings, no secret, no path.
	got, ok := diagColab(ctx, ce.srv)
	if !ok {
		t.Fatal("no colab.json in the diagnostics")
	}
	db, _ := json.Marshal(got)
	ce.noSecrets("colab.json", db)
	if bytes.Contains(db, []byte("application_default_credentials")) || !bytes.Contains(db, []byte(`"enabled":true`)) {
		t.Fatalf("colab.json = %s", db)
	}

	// Shutdown: mode checked; "now" holds the automatic dispatch.
	if info := ce.callErrInfo("POST", "/api/colab/shutdown", map[string]any{"mode": "later"}, http.StatusBadRequest); info.Code != "request.bad_param" {
		t.Fatalf("bad mode: %+v", info)
	}
	ce.call("POST", "/api/colab/shutdown", map[string]any{"mode": "now"}, http.StatusOK, &v)
	if !v.Held {
		t.Fatalf("view after shutdown now = %+v", v)
	}
	if j, _ := ce.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageASR); j.Stats.RemotePin {
		t.Fatal("the queued lecture is still pinned to Colab after the shutdown")
	}

	// Disconnect: revoked at Google, deleted locally, the note about gcloud
	// (the credential was imported from the user's own gcloud).
	var dv ColabDisconnectView
	ce.call("DELETE", "/api/colab/auth", nil, http.StatusOK, &dv)
	if !dv.Revoked || len(dv.Notes) != 1 || dv.Notes[0].Code != sched.CodeRemoteADCSignedOut || ce.revoked != 1 {
		t.Fatalf("disconnect = %+v (revocations %d)", dv, ce.revoked)
	}
	if ce.reg.Present(model.DeviceColab) || ce.b.Credential(ctx).Present {
		t.Fatal("colab:remote still usable after the disconnect")
	}
	if v, ok, _ := ce.st.GetKV(ctx, colab.KVCredential); ok && v != "" {
		t.Fatal("the credential is still in kv")
	}

	// Switched off: use-colab is refused, no remote area.
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": false}, http.StatusOK, &v)
	if info := ce.callErrInfo("POST", "/api/lectures/"+l.ID+"/use-colab", nil, http.StatusConflict); info.Code != sched.CodeRemoteDisabled {
		t.Fatalf("use-colab with Colab off: %+v", info)
	}
	if _, ok := RemoteSnapshot(ce.b, ce.sc)(ctx); ok {
		t.Fatal("remote area while Colab is off")
	}
	ce.noSecrets("logs", []byte(ce.logs.String()))
}

func TestColabStageView(t *testing.T) {
	ce := newColabEnv(t)
	ctx := context.Background()
	c := ce.course("CSC1003")
	l := ce.lecture(c.ID, "W1")
	ce.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	j := ce.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning)
	st := j.Stats
	st.Remote = &model.RemoteStats{Step: model.RemoteStepUploadAudio, Steps: model.RemoteSteps, EtaSec: 840, Accelerator: "T4"}
	if err := ce.st.SetJobStats(ctx, j.ID, st); err != nil {
		t.Fatal(err)
	}
	var lv Lecture
	ce.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	asr := stageOf(lv.Stages, model.StageASR)
	if r := asr.Stats.Remote; r == nil || r.Step != model.RemoteStepUploadAudio || r.EtaSec != 840 || r.Accelerator != "T4" || len(r.Steps) != 5 {
		t.Fatalf("stats.remote = %+v", asr.Stats.Remote)
	}
	// Other stages carry no remote field.
	_, b := ce.do("GET", "/api/lectures/"+l.ID, nil)
	if n := bytes.Count(b, []byte(`"remote":`)); n != 1 {
		t.Fatalf("%d remote fields in %s", n, b)
	}

	// Handed back by Colab: the queued job's note says so.
	if err := ce.st.SetJobStats(ctx, j.ID, model.JobStats{RemoteRequeues: 1,
		RemoteFallback: &model.ErrorInfo{Code: colab.CodeSessionReclaimed}}); err != nil {
		t.Fatal(err)
	}
	ce.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusFailed)
	ce.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	var lv2 Lecture
	ce.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv2)
	asr = stageOf(lv2.Stages, model.StageASR)
	if asr.NoteInfo == nil || !strings.Contains(mustJSON(t, asr.NoteInfo), sched.CodeRemoteBackToLocal) ||
		!strings.Contains(mustJSON(t, asr.NoteInfo), colab.CodeSessionReclaimed) || asr.Stats.Remote != nil {
		t.Fatalf("note of a lecture Colab handed back: %+v / %+v", asr.NoteInfo, asr.Stats)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
