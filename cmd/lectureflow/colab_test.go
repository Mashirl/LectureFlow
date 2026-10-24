// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
)

// SPEC-v3 V14 (v14-colab-integrate): cmd builds the Colab backend, wires it
// into the scheduler, the device table, the resource bar and the HTTP API,
// and the graceful stop deletes a credential of the "session" lifecycle.
func TestColabWiring(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.test-access","expires_in":3599,"token_type":"Bearer",`+
			`"scope":"openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/colaboratory"}`)
	}))
	defer oauth.Close()
	dir := t.TempDir()
	adc := filepath.Join(dir, "application_default_credentials.json")
	raw, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "id.apps.example",
		"client_secret": "test-client-secret", "refresh_token": "1//test-refresh"})
	if err := os.WriteFile(adc, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	fakeASR, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr.json"))
	cfg.ASR.FakeResult = fakeASR
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := buildApp(ctx, cfg, AppOptions{
		Sched: func(o *sched.Options) { o.ScanInterval = 10 * time.Millisecond },
		Colab: func(o *colab.Options) {
			o.TokenURL, o.RevokeURL, o.ColabURL, o.ADCPath = oauth.URL+"/token", oauth.URL+"/revoke", oauth.URL+"/colab", adc
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.Colab == nil {
		t.Fatal("no Colab backend")
	}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Server.Handler())
	defer srv.Close()
	call := func(method, path string, body any) []byte {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		return b
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("Colab startup", app.Colab.ready.Load)

	call("PUT", "/api/colab/settings", map[string]any{"enabled": true})
	b := call("POST", "/api/colab/auth/import", map[string]any{"lifecycle": "session"})
	if strings.Contains(string(b), "test-refresh") || strings.Contains(string(b), "test-client-secret") {
		t.Fatalf("import answered a secret: %s", b)
	}
	waitFor("colab:remote in the device table", func() bool { return app.Devices.Present(model.DeviceColab) })
	var snap resources.Snapshot
	if err := json.Unmarshal(call("GET", "/api/resources", nil), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Remote == nil || !snap.Remote.Configured || snap.Remote.State != colab.StateNone {
		t.Fatalf("resources remote = %+v", snap.Remote)
	}
	for _, d := range snap.Devices {
		if d.ID == model.DeviceColab {
			t.Fatal("colab:remote as a local device card")
		}
	}

	// The graceful stop ends the run: the "session" credential is deleted.
	srv.Close()
	app.Close()
	st, err := db.Open(filepath.Join(cfg.DataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, ok, _ := st.GetKV(context.Background(), colab.KVCredential); ok && v != "" {
		t.Fatal("the session credential survived the graceful stop")
	}
}
