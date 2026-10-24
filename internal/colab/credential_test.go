// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Credential lifecycle (V14.2, V14.7): expired = deleted, never renewed
// past the user's limit, session credentials gone at the next start,
// disconnect revokes (replay_test.go), no secret in logs or errors.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// fakeOAuth is a token endpoint: answer(form) returns status and body.
type fakeOAuth struct {
	srv   *httptest.Server
	calls atomic.Int32
	forms []url.Values
}

func newFakeOAuth(t *testing.T, answer func(url.Values) (int, string)) *fakeOAuth {
	f := &fakeOAuth{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		f.forms = append(f.forms, form)
		st, body := answer(form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func okToken(scope string) func(url.Values) (int, string) {
	return func(url.Values) (int, string) {
		return 200, fmt.Sprintf(`{"access_token":%q,"expires_in":3599,"scope":%q,"token_type":"Bearer"}`, testAccessToken, scope)
	}
}

const fullScope = "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/colaboratory"

func newCredBackend(t *testing.T, oauth *fakeOAuth, runID string) (*Backend, *MemKV, *fakeClock, *logBuf) {
	t.Helper()
	kv := NewMemKV()
	clk := newFakeClock()
	lg, lb := newTestLogger()
	o := Options{KV: kv, Logger: lg, Now: clk.Now, Sleep: clk.Sleep, RunID: runID, ColabURL: "http://127.0.0.1:1/colab",
		ADCPath: filepath.Join(t.TempDir(), "absent.json")}
	if oauth != nil {
		o.TokenURL, o.RevokeURL = oauth.srv.URL+"/token", oauth.srv.URL+"/revoke"
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return b, kv, clk, lb
}

func TestParseLifecycle(t *testing.T) {
	for in, want := range map[string]Lifecycle{
		"7d": {Kind: LifecycleDays, Days: 7}, "30d": {Kind: LifecycleDays, Days: 30}, "90d": {Kind: LifecycleDays, Days: 90},
		"session": {Kind: LifecycleSession}, "manual": {Kind: LifecycleManual}, "": {Kind: LifecycleDays, Days: 7},
		" Session ": {Kind: LifecycleSession},
	} {
		got, err := ParseLifecycle(in)
		if err != nil || got != want {
			t.Errorf("ParseLifecycle(%q) = %+v, %v", in, got, err)
		}
		if err == nil && in != "" && strings.TrimSpace(strings.ToLower(in)) != got.String() {
			t.Errorf("String() = %q for %q", got.String(), in)
		}
	}
	for _, in := range []string{"0d", "91d", "7", "d", "forever", "-1d", "7days"} {
		if _, err := ParseLifecycle(in); Code(err) != CodeLifecycleInvalid {
			t.Errorf("ParseLifecycle(%q) err %v", in, err)
		}
	}
}

func TestExpiredCredentialIsDeletedBeforeUse(t *testing.T) {
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
	ctx := context.Background()
	storeCredential(t, kv, Lifecycle{Kind: LifecycleDays, Days: 7}, clk.Now().Add(-7*24*time.Hour-time.Second), "")
	if ok, w := b.CanDispatch(ctx, 600); ok || w != model.WaitRemoteAuth {
		t.Errorf("CanDispatch = %v %q", ok, w)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("expired credential not deleted")
	}
	if info := b.Credential(ctx); info.Present {
		t.Errorf("info %+v", info)
	}
	// Transcribe refuses without contacting anyone.
	storeCredential(t, kv, Lifecycle{Kind: LifecycleDays, Days: 7}, clk.Now().Add(-8*24*time.Hour), "")
	dir := t.TempDir()
	wav := filepath.Join(dir, "audio.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	b.o.WorkerDir = workerDir(t)
	_, err := b.Transcribe(ctx, Job{ID: "x", AudioPath: wav, DurationSec: 60, OutDir: dir}, nil)
	if Code(err) != CodeAuthExpired || WaitingFor(err) != model.WaitRemoteAuth || !IsFallback(err) {
		t.Errorf("Transcribe err %v", err)
	}
	if oauth.calls.Load() != 0 {
		t.Errorf("%d token requests with an expired credential", oauth.calls.Load())
	}
	assertNoSecrets(t, "log", lb.String())
	assertNoSecrets(t, "error", errTexts(err)...)
}

func TestSessionCredentialLifecycle(t *testing.T) {
	ctx := context.Background()
	// A session credential of an earlier run is deleted at start.
	b, kv, clk, _ := newCredBackend(t, nil, "run-2")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleSession}, clk.Now().Add(-time.Hour), "run-1")
	if err := b.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("session credential of an earlier run survived the start")
	}
	// This run's own: usable now, deleted when the app closes.
	storeCredential(t, kv, Lifecycle{Kind: LifecycleSession}, clk.Now(), "run-2")
	if err := b.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	if info := b.Credential(ctx); !info.Present || info.Lifecycle != "session" || info.RemainingSec != -1 {
		t.Errorf("info %+v", info)
	}
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("session credential survived Close")
	}
	// Manual: no time limit; Close keeps it.
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now().Add(-1000*24*time.Hour), "")
	if info := b.Credential(ctx); !info.Present || info.Lifecycle != "manual" || info.ExpiresAt != nil {
		t.Errorf("manual info %+v", info)
	}
	_ = b.Close(ctx)
	if v, _, _ := kv.GetKV(ctx, KVCredential); v == "" {
		t.Error("manual credential deleted by Close")
	}
}

func TestAccessTokenNeverOutlivesLifecycle(t *testing.T) {
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
	ctx := context.Background()
	// 1 day, imported 23 h 50 min ago: 10 minutes left; Google would give
	// an hour.
	imported := clk.Now().Add(-23*time.Hour - 50*time.Minute)
	storeCredential(t, kv, Lifecycle{Kind: LifecycleDays, Days: 1}, imported, "")
	before, _, _ := kv.GetKV(ctx, KVCredential)
	tok, err := b.auth.token(ctx)
	if err != nil || tok != testAccessToken {
		t.Fatalf("token: %v", err)
	}
	// The refresh did not extend the stored lifecycle.
	if after, _, _ := kv.GetKV(ctx, KVCredential); after != before {
		t.Error("the stored credential changed on refresh (renewal?)")
	}
	// The narrowed scopes are requested (no cloud-platform).
	if got := oauth.forms[0].Get("scope"); got != fullScope {
		t.Errorf("refresh scope %q", got)
	}
	clk.Advance(9 * time.Minute)
	if _, err := b.auth.token(ctx); err != nil {
		t.Errorf("token before the end: %v", err)
	}
	if n := oauth.calls.Load(); n != 1 {
		t.Errorf("%d refreshes, want 1 (cached)", n)
	}
	clk.Advance(61 * time.Second)
	_, err = b.auth.token(ctx)
	if Code(err) != CodeAuthExpired || WaitingFor(err) != model.WaitRemoteAuth {
		t.Errorf("token after the end: %v", err)
	}
	if n := oauth.calls.Load(); n != 1 {
		t.Errorf("%d refreshes after the end", n)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("credential not deleted at the end of its lifecycle")
	}
	assertNoSecrets(t, "log", lb.String())
}

func TestRefreshRejectedDeletesCredential(t *testing.T) {
	oauth := newFakeOAuth(t, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked. 1//replay-refresh-token-0123456789"}`
	})
	b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
	ctx := context.Background()
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	_, err := b.auth.token(ctx)
	if Code(err) != CodeAuthRejected || WaitingFor(err) != model.WaitRemoteAuth {
		t.Fatalf("err %v", err)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("rejected credential not deleted")
	}
	assertNoSecrets(t, "log", lb.String())
	assertNoSecrets(t, "error", errTexts(err)...)
}

func TestTokenEndpointFailureIsNotAuth(t *testing.T) {
	oauth := newFakeOAuth(t, func(url.Values) (int, string) { return 503, "unavailable " + testRefreshToken })
	b, kv, clk, _ := newCredBackend(t, oauth, "run-1")
	ctx := context.Background()
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	_, err := b.auth.token(ctx)
	if Code(err) != CodeAuthEndpoint || WaitingFor(err) != "" {
		t.Fatalf("err %v", err)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v == "" {
		t.Error("a transient failure deleted the credential")
	}
	assertNoSecrets(t, "error", errTexts(err)...)
}

func TestImportADC(t *testing.T) {
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
	ctx := context.Background()
	b.o.ADCPath = writeADC(t, t.TempDir(), "authorized_user")
	info, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleDays, Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Present || info.Lifecycle != "7d" || info.ExpiresAt == nil || !info.ExpiresAt.Equal(clk.Now().Add(7*24*time.Hour)) ||
		info.RemainingSec != 7*24*3600 {
		t.Errorf("info %+v", info)
	}
	var c storedCredential
	v, _, _ := kv.GetKV(ctx, KVCredential)
	if err := json.Unmarshal([]byte(v), &c); err != nil || c.RefreshToken != testRefreshToken || c.Lifecycle.Days != 7 {
		t.Errorf("stored %v", err)
	}
	// Nothing outward carries a secret: the info, the status, formatting.
	ib, _ := json.Marshal(info)
	sb, _ := json.Marshal(b.Status(ctx))
	assertNoSecrets(t, "credential info", string(ib))
	assertNoSecrets(t, "status", string(sb))
	assertNoSecrets(t, "formatting", fmt.Sprintf("%v %+v %#v %s", c, &c, c, c))
	slog.New(slog.NewTextHandler(io.Discard, nil)).Info("x", "c", c)
	b.log.Info("formatting check", "credential", c, "ptr", &c)
	assertNoSecrets(t, "log", lb.String())
	if !strings.Contains(lb.String(), "[colab credential]") {
		t.Error("the credential was not logged in its redacted form")
	}
	// The access token from the import is used without another refresh.
	if _, err := b.auth.token(ctx); err != nil || oauth.calls.Load() != 1 {
		t.Errorf("token after import: %v, %d calls", err, oauth.calls.Load())
	}
}

func TestImportADCRejects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string) string
		scope string
		code  string
	}{
		{"missing file", func(t *testing.T, dir string) string { return filepath.Join(dir, "nope.json") }, fullScope, CodeADCNotFound},
		{"service account", func(t *testing.T, dir string) string { return writeADC(t, dir, "service_account") }, fullScope, CodeADCUnsupported},
		{"not JSON", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, adcFileName)
			_ = os.WriteFile(p, []byte("{"), 0o600)
			return p
		}, fullScope, CodeADCInvalid},
		{"no refresh token", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, adcFileName)
			_ = os.WriteFile(p, []byte(`{"type":"authorized_user","client_id":"a","client_secret":"b"}`), 0o600)
			return p
		}, fullScope, CodeADCInvalid},
		{"login without the Colab scope", func(t *testing.T, dir string) string { return writeADC(t, dir, "authorized_user") },
			"openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/cloud-platform", CodeADCMissingScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oauth := newFakeOAuth(t, okToken(tc.scope))
			b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
			// An earlier, working credential must survive a failed import.
			storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
			before, _, _ := kv.GetKV(ctx, KVCredential)
			b.o.ADCPath = tc.setup(t, t.TempDir())
			_, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleManual})
			if Code(err) != tc.code {
				t.Fatalf("err %v, want %s", err, tc.code)
			}
			if after, _, _ := kv.GetKV(ctx, KVCredential); after != before {
				t.Error("a failed import changed the stored credential")
			}
			assertNoSecrets(t, "log", lb.String())
			assertNoSecrets(t, "error", errTexts(err)...)
			if tc.code == CodeADCMissingScope {
				if s, _ := codedParam(err, "scopes"); s != scopeColab {
					t.Errorf("missing scopes %q", s)
				}
			}
		})
	}
	// A refresh token Google rejects at import leaves the stored one alone.
	oauth := newFakeOAuth(t, func(url.Values) (int, string) { return 400, `{"error":"invalid_grant"}` })
	b, kv, clk, _ := newCredBackend(t, oauth, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	b.o.ADCPath = writeADC(t, t.TempDir(), "authorized_user")
	if _, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleManual}); Code(err) != CodeAuthRejected {
		t.Fatalf("err %v", err)
	}
	if v, _, _ := kv.GetKV(ctx, KVCredential); v == "" {
		t.Error("a rejected import deleted the stored credential")
	}
	if _, err := b.ImportADC(ctx, Lifecycle{Kind: "forever"}); Code(err) != CodeLifecycleInvalid {
		t.Errorf("lifecycle err %v", err)
	}
}

func TestDetectADCAndPath(t *testing.T) {
	dir := t.TempDir()
	if info := DetectADC(filepath.Join(dir, adcFileName)); info.Found || info.Problem != CodeADCNotFound ||
		!strings.Contains(info.LoginCommand, scopeColab) || !strings.Contains(info.LoginCommand, scopeCloud) {
		t.Errorf("absent: %+v", info)
	}
	p := writeADC(t, dir, "authorized_user")
	if info := DetectADC(p); !info.Found || info.Problem != "" || info.Type != "authorized_user" {
		t.Errorf("present: %+v", info)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CONFIG", dir)
	if got := ADCPath(); got != filepath.Join(dir, adcFileName) {
		t.Errorf("CLOUDSDK_CONFIG: %s", got)
	}
	other := filepath.Join(t.TempDir(), "creds.json")
	_ = os.WriteFile(other, []byte("{}"), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", other)
	if got := ADCPath(); got != other {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS: %s", got)
	}
}

func TestTransportErrorsDropTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL + "/rt")
	srv.Close() // connection refused from now on
	rt := &runtimeClient{hc: &http.Client{}, base: u, token: testProxyToken, agent: "lectureflow"}
	_, err := rt.download(context.Background(), "content/lectureflow/x/result.json")
	if err == nil {
		t.Fatal("no error")
	}
	assertNoSecrets(t, "transport error", err.Error())
	if strings.Contains(err.Error(), "colab-runtime-proxy-token") {
		t.Errorf("error quotes the URL: %v", err)
	}
}

func TestSecretSetScrubsErrors(t *testing.T) {
	var s secretSet
	s.add(testProxyToken, "abc") // "abc" is too short to be a secret
	err := newErr(CodeUnavailable, map[string]any{"endpoint": "x " + testProxyToken}, kindFallback, model.WaitRemoteQueue,
		fmt.Errorf("failed with %s and abc", testProxyToken))
	got := s.scrubErr(err)
	assertNoSecrets(t, "scrubbed", errTexts(got)...)
	if !strings.Contains(got.Error(), "abc") || Code(got) != CodeUnavailable || !IsFallback(got) || WaitingFor(got) != model.WaitRemoteQueue {
		t.Errorf("scrubbed error lost information: %v", got)
	}
	plain := fmt.Errorf("nothing secret")
	if s.scrubErr(plain) != plain {
		t.Error("an error without secrets was rebuilt")
	}
}
