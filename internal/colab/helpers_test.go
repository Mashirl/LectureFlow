// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Secrets of the tests: none may appear in a log line, an error or a
// returned structure.
const (
	testRefreshToken = "1//replay-refresh-token-0123456789"
	testClientSecret = "replay-client-secret-abcdef"
	testClientID     = "replay-client.apps.googleusercontent.com"
	testAccessToken  = "ya29.replay-access-token-1"
	testProxyToken   = "proxy-token-replay-1"
	testXSRF         = "xsrf-assign-replay-1"
)

var testSecrets = []string{testRefreshToken, testClientSecret, testAccessToken, testProxyToken, testXSRF,
	"xsrf-unassign-replay-1", "ya29.replay-access-token-2", "proxy-token-replay-2"}

// fakeClock is the backend's Now and Sleep: Sleep advances it at once.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return nil
}

// logBuf collects the backend's log output.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newTestLogger() (*slog.Logger, *logBuf) {
	lb := &logBuf{}
	return slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})), lb
}

// assertNoSecrets fails when any test secret appears in texts.
func assertNoSecrets(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, txt := range texts {
		for _, s := range testSecrets {
			if strings.Contains(txt, s) {
				t.Errorf("%s contains a secret (%q…)", what, s[:8])
			}
		}
	}
}

// errTexts is everything an error exposes: its text and its ErrorInfo JSON.
func errTexts(err error) []string {
	if err == nil {
		return nil
	}
	out := []string{err.Error()}
	if info := infoOf(err); info != nil {
		b, _ := json.Marshal(info)
		out = append(out, string(b))
	}
	return out
}

// storeCredential puts a credential into kv as an import would.
func storeCredential(t *testing.T, kv KV, lc Lifecycle, importedAt time.Time, runID string) {
	t.Helper()
	c := storedCredential{Version: credVersion, ClientID: testClientID, ClientSecret: testClientSecret,
		RefreshToken: testRefreshToken, Lifecycle: lc, ImportedAt: importedAt,
		Scopes: []string{scopeOpenID, scopeEmail, scopeColab}}
	switch lc.Kind {
	case LifecycleDays:
		c.ExpiresAt = importedAt.Add(time.Duration(lc.Days) * 24 * time.Hour)
	case LifecycleSession:
		c.RunID = runID
	}
	b, _ := json.Marshal(c)
	if err := kv.SetKV(context.Background(), KVCredential, string(b)); err != nil {
		t.Fatal(err)
	}
}

// writeADC writes a gcloud ADC file.
func writeADC(t *testing.T, dir string, typ string) string {
	t.Helper()
	p := filepath.Join(dir, adcFileName)
	b, _ := json.Marshal(map[string]string{
		"type": typ, "client_id": testClientID, "client_secret": testClientSecret, "refresh_token": testRefreshToken,
		"account": "", "universe_domain": "googleapis.com",
	})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeWorkerDir copies the real worker scripts the backend uploads.
func workerDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(filepath.Join("..", "..", "worker"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range workerFiles {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Fatalf("worker file %s: %v", f, err)
		}
	}
	return d
}

// copyFFmpeg is a fake ffmpeg: it "encodes" by writing size bytes.
func fakeFFmpeg(size int) FFmpegRunner {
	return func(_ context.Context, _ string, args []string) error {
		out := args[len(args)-1]
		b := make([]byte, size)
		for i := range b {
			b[i] = byte('A' + i%26)
		}
		return os.WriteFile(out, b, 0o644)
	}
}
