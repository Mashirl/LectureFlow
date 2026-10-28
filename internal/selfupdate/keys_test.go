// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The maintainer's public key is built in (2026-10-27): it parses, its key
// id as minisign prints it is PublicKeyID, and the release documentation
// names both (so a changed key cannot leave the instructions behind).
func TestBuiltinPublicKey(t *testing.T) {
	if PublicKey == "" || builtinPublicKey != PublicKey || BuiltinPublicKey() != PublicKey {
		t.Fatalf("the built-in key is not PublicKey (%q)", builtinPublicKey)
	}
	pk, err := ParsePublicKey(PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%X", keyIDDisplay(pk.KeyID)); got != PublicKeyID {
		t.Fatalf("key id %s, PublicKeyID %s", got, PublicKeyID)
	}
	if pk.String() != PublicKey {
		t.Fatalf("the key prints as %q", pk.String())
	}
	// the .pub file as minisign writes it is the same key
	file, err := ParsePublicKey("untrusted comment: minisign public key " + PublicKeyID + "\n" + PublicKey + "\n")
	if err != nil || file.KeyID != pk.KeyID || !file.Key.Equal(pk.Key) {
		t.Fatalf(".pub file: %+v %v", file, err)
	}
	for _, doc := range []string{"README.md", filepath.Join("..", "..", "scripts", "release", "README.md")} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{PublicKeyID, PublicKey} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s does not name %s", doc, want)
			}
		}
	}
}

// The built-in key is the one that counts: an update.json signed with any
// other key is refused (selfupdate.signature_invalid, naming the built-in
// key id), nothing is offered.
func TestBuiltinKeyRefusesOtherKeys(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	u := newTestUpdater(t, f, k, func(o *Options) { o.PublicKey = "" }) // "" = the built-in key
	if !u.Enabled() {
		t.Fatalf("not configured with the built-in key: %+v", u.Status(bg))
	}
	st, err := u.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateError || st.Error == nil || st.Error.Code != CodeSignatureInvalid || st.Available != nil {
		t.Fatalf("status %+v", st)
	}
	if !strings.Contains(st.Error.Detail, PublicKeyID) {
		t.Fatalf("detail %q does not name the built-in key %s", st.Error.Detail, PublicKeyID)
	}
	if _, err := u.Download(bg); CodeOf(err) != CodeNotAvailable {
		t.Fatalf("download: %v", err)
	}
}

// The application as built (the maintainer's key, the real Repo, a
// release version) on a computer without network, and while the latest
// release has no update.json yet: the first automatic check fails quietly
// (state error: selfupdate.unreachable, or selfupdate.no_manifest when
// GitHub answers 404; the settings say so, the workbench shows nothing for
// a failed check) and is recorded. The automatic checks back off
// (CheckInterval: 1 h, 3 h, 6 h, then a day; every 2 h while the release
// has no update.json), never at every tick, and a restart neither asks
// again before the time nor forgets the failure. One request per check,
// to GitHub only, with nothing but the User-Agent.
func TestBuiltinKeyOfflineBacksOff(t *testing.T) {
	for _, c := range []struct {
		name   string
		code   string
		ladder []time.Duration // the waits between the checks
		answer func(*http.Request) (*http.Response, error)
	}{
		{"no network", CodeUnreachable, []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour},
			func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: lookup github.com: no such host")
			}},
		{"no update.json in the latest release", CodeNoManifest, []time.Duration{2 * time.Hour, 2 * time.Hour, 2 * time.Hour},
			func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: http.Header{},
					Body: io.NopCloser(strings.NewReader("Not Found")), Request: r}, nil
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			var bad []string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				asked = append(asked, r.URL.String())
				if len(r.Header) != 1 || r.Header.Get("User-Agent") != UserAgent {
					bad = append(bad, fmt.Sprint(r.Header))
				}
				mu.Unlock()
				return c.answer(r)
			})}
			count := func() int { mu.Lock(); defer mu.Unlock(); return len(asked) }
			now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
			clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

			kv := newMemKV()
			dataDir := privateDataDir(t)
			start := func() (*Updater, func()) {
				u := New(Options{DataDir: dataDir, Version: "3.2.0", KV: kv, Desktop: true, Platform: PlatformWindowsX64,
					Client: client, Downloader: &HTTPDownloader{Client: client}, Now: clock, Log: quietLog(),
					FirstCheckDelay: time.Millisecond, CheckEvery: 2 * time.Millisecond})
				u.Startup(bg)
				ctx, cancel := context.WithCancel(bg)
				done := make(chan struct{})
				go func() { u.Run(ctx); close(done) }()
				return u, func() { cancel(); <-done; u.Close() }
			}
			waitChecks := func(n int) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for count() < n {
					if time.Now().After(deadline) {
						t.Fatalf("%d requests, want %d", count(), n)
					}
					time.Sleep(time.Millisecond)
				}
				time.Sleep(50 * time.Millisecond) // many ticks: no further request
				if got := count(); got != n {
					t.Fatalf("%d requests, want %d", got, n)
				}
			}
			failed := func(u *Updater, wait time.Duration) {
				t.Helper()
				st := u.Status(bg)
				if st.State != StateError || st.Error == nil || st.Error.Code != c.code || st.Available != nil || st.LastCheck == "" || st.Reason != "" {
					t.Fatalf("status after the failed check %+v %+v", st, st.Error)
				}
				if !strings.Contains(st.Error.Detail, "github: ") {
					t.Fatalf("detail %q does not name the source", st.Error.Detail)
				}
				next, err := time.Parse(time.RFC3339, st.NextCheck)
				last, _ := time.Parse(time.RFC3339, st.LastCheck)
				if err != nil || next.Sub(last) != wait {
					t.Fatalf("next check %q, last %q: want %v later", st.NextCheck, st.LastCheck, wait)
				}
			}

			u, stop := start()
			waitChecks(1)
			failed(u, c.ladder[0])
			var rec lastCheckRecord
			if v, ok, _ := kv.GetKV(bg, KVLastCheck); !ok || json.Unmarshal([]byte(v), &rec) != nil || rec.Result != "error" ||
				rec.Failures != 1 || rec.Error == nil || rec.Error.Code != c.code {
				t.Fatalf("last check %q", v)
			}
			for i, wait := range c.ladder[:len(c.ladder)-1] {
				advance(wait - time.Minute)
				waitChecks(i + 1)
				advance(time.Minute)
				waitChecks(i + 2)
				failed(u, c.ladder[i+1])
			}
			n := len(c.ladder)
			stop()

			// a restart before the next check is due does not ask again,
			// and still shows the failure and when the next check comes
			u, stop = start()
			failed(u, c.ladder[n-1])
			advance(c.ladder[n-1] - time.Minute)
			waitChecks(n)
			advance(time.Minute)
			waitChecks(n + 1)
			stop()

			mu.Lock()
			defer mu.Unlock()
			for _, a := range asked {
				if a != "https://github.com/ShouldBeQuincy/LectureFlow/releases/latest/download/update.json" {
					t.Errorf("asked %s", a)
				}
			}
			if len(bad) != 0 {
				t.Errorf("requests with other headers: %v", bad)
			}
		})
	}
}
