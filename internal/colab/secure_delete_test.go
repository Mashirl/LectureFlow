// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// v3 Gate round 1 item 9: every way the credential is deleted (lifecycle
// ended, Google rejected it, session credential of an earlier run, the app
// closing with a session credential, 断开连接) goes through the store's
// SetSecretKV, so neither the database file nor its WAL keeps the refresh
// token or the client secret. Checked with the real SQLite store and a
// raw byte scan.
func TestCredentialDeletionLeavesNoBytes(t *testing.T) {
	cases := []struct {
		name   string
		runID  string
		lc     func(now time.Time) (Lifecycle, time.Time, string)
		answer func(url.Values) (int, string)
		delete func(t *testing.T, b *Backend)
	}{
		{"lifecycle ended", "run-1",
			func(now time.Time) (Lifecycle, time.Time, string) {
				return Lifecycle{Kind: LifecycleDays, Days: 1}, now.Add(-25 * time.Hour), ""
			}, nil,
			func(t *testing.T, b *Backend) {
				if ok, w := b.CanDispatch(context.Background(), 600); ok || w != model.WaitRemoteAuth {
					t.Errorf("CanDispatch %v %q", ok, w)
				}
			}},
		{"rejected by Google", "run-1",
			func(now time.Time) (Lifecycle, time.Time, string) { return Lifecycle{Kind: LifecycleManual}, now, "" },
			func(url.Values) (int, string) { return 400, `{"error":"invalid_grant"}` },
			func(t *testing.T, b *Backend) {
				if _, err := b.auth.token(context.Background()); Code(err) != CodeAuthRejected {
					t.Errorf("token: %v", err)
				}
			}},
		{"session of an earlier run", "run-2",
			func(now time.Time) (Lifecycle, time.Time, string) {
				return Lifecycle{Kind: LifecycleSession}, now.Add(-time.Hour), "run-1"
			}, nil,
			func(t *testing.T, b *Backend) {
				if err := b.Startup(context.Background()); err != nil {
					t.Error(err)
				}
			}},
		{"session at app exit", "run-1",
			func(now time.Time) (Lifecycle, time.Time, string) {
				return Lifecycle{Kind: LifecycleSession}, now, "run-1"
			}, nil,
			func(t *testing.T, b *Backend) {
				if err := b.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}},
		{"disconnect", "run-1",
			func(now time.Time) (Lifecycle, time.Time, string) { return Lifecycle{Kind: LifecycleManual}, now, "" },
			func(url.Values) (int, string) { return 200, `{}` },
			func(t *testing.T, b *Backend) {
				if _, err := b.Disconnect(context.Background()); err != nil {
					t.Error(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "lectureflow.db")
			st, err := db.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, ok := any(st).(SecretKV); !ok {
				t.Fatal("db.Store does not implement SecretKV")
			}
			clk := newFakeClock()
			lg, _ := newTestLogger()
			o := Options{KV: st, Logger: lg, Now: clk.Now, Sleep: clk.Sleep, RunID: tc.runID, ColabURL: "http://127.0.0.1:1/colab",
				ADCPath: filepath.Join(t.TempDir(), "absent.json")}
			answer := tc.answer
			if answer == nil {
				answer = okToken(fullScope)
			}
			oauth := newFakeOAuth(t, answer)
			o.TokenURL, o.RevokeURL = oauth.srv.URL+"/token", oauth.srv.URL+"/revoke"
			b, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			lc, at, run := tc.lc(clk.Now())
			// Written a few times between other rows, as a real database
			// would have it (older copies, page splits).
			for i := 0; i < 3; i++ {
				storeCredential(t, st, lc, at, run)
				for j := 0; j < 30; j++ {
					if err := st.SetKV(ctx, fmt.Sprintf("filler.%d.%d", i, j), strings.Repeat("y", 250)); err != nil {
						t.Fatal(err)
					}
				}
			}
			tc.delete(t, b)
			if v, _, _ := st.GetKV(ctx, KVCredential); v != "" {
				t.Fatal("the credential is still stored")
			}
			for _, sfx := range []string{"", "-wal", "-journal"} {
				raw, err := os.ReadFile(path + sfx)
				if err != nil {
					continue
				}
				for _, s := range []string{testRefreshToken, testClientSecret} {
					if bytes.Contains(raw, []byte(s)) {
						t.Errorf("%s still holds %q", filepath.Base(path+sfx), s)
					}
				}
			}
		})
	}
}
