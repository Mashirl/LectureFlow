// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/selfupdate/selfupdatetest"
)

// ---- a test key pair (generated per test; no real key is ever used) ----

type testKey struct {
	*selfupdatetest.Key
	key Key // the same public key, parsed
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	k, err := selfupdatetest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ParsePublicKey(k.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return testKey{Key: k, key: pk}
}

// pubFile is the key as minisign writes a .pub file.
func (k testKey) pubFile() string { return k.PublicKeyFile() }

// sign makes a .minisig file the way minisign does.
func (k testKey) sign(msg []byte, trusted string, prehashed bool) []byte {
	return k.Sign(msg, trusted, prehashed)
}

// ---- kv ----

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemKV() *memKV { return &memKV{m: map[string]string{}} }

func (k *memKV) GetKV(_ context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) SetKV(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

// ---- a fake GitHub (and mirrors) ----

type seenRequest struct {
	path   string
	header http.Header
}

type fakeGH struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	files    map[string][]byte
	status   map[string]int
	redirect map[string]string
	hold     map[string]chan struct{}
	seen     []seenRequest
}

func newFakeGH(t *testing.T) *fakeGH {
	f := &fakeGH{t: t, files: map[string][]byte{}, status: map[string]int{}, redirect: map[string]string{},
		hold: map[string]chan struct{}{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// ServeHTTP serves files by path. /proxy/<absolute URL> serves the URL's
// path of this server (a "{url}" proxy mirror).
func (f *fakeGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(r.RequestURI, "/proxy/") {
		target := strings.TrimPrefix(r.RequestURI, "/proxy/")
		p = "/proxy:" + strings.TrimPrefix(target, f.srv.URL)
	}
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{path: p, header: r.Header.Clone()})
	content, ok := f.files[p]
	st := f.status[p]
	loc := f.redirect[p]
	hold := f.hold[p]
	f.mu.Unlock()
	if strings.HasPrefix(p, "/proxy:") && st == 0 && !ok {
		f.mu.Lock()
		real := strings.TrimPrefix(p, "/proxy:")
		st, loc, hold = f.status[real], f.redirect[real], f.hold[real]
		content, ok = f.files[real]
		f.mu.Unlock()
	}
	switch {
	case st != 0:
		w.WriteHeader(st)
		return
	case loc != "":
		http.Redirect(w, r, loc, http.StatusFound)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	if hold != nil {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.WriteHeader(http.StatusOK)
		w.Write(content[:len(content)/3])
		w.(http.Flusher).Flush()
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		return
	}
	http.ServeContent(w, r, path.Base(p), time.Time{}, bytes.NewReader(content))
}

func (f *fakeGH) set(p string, b []byte) {
	f.mu.Lock()
	f.files[p] = b
	f.mu.Unlock()
}

func (f *fakeGH) setStatus(p string, st int) {
	f.mu.Lock()
	f.status[p] = st
	f.mu.Unlock()
}

func (f *fakeGH) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func (f *fakeGH) count(p string) int {
	n := 0
	for _, r := range f.requests() {
		if r.path == p {
			n++
		}
	}
	return n
}

const (
	latestPath = "/o/r/releases/latest/download/update.json"
	apiPath    = "/api/repos/o/r/releases"
)

func downloadPath(version, name string) string {
	return "/o/r/releases/download/v" + version + "/" + name
}

// release is a published fake release.
type release struct {
	version string
	m       *Manifest
	raw     []byte
	sig     []byte
	pkgs    map[string][]byte // name → content
}

// pkgContent is a deterministic package of about 300 KB.
func pkgContent(name string) []byte {
	seed := sha256.Sum256([]byte(name))
	r := mrand.New(mrand.NewChaCha8(seed))
	b := make([]byte, 300_000+int(seed[0])*7)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func assetNames(version string) map[string][2]string { // name → platform, kind
	return map[string][2]string{
		"LectureFlow-Setup-" + version + "-x64.exe":    {PlatformWindowsX64, KindInstaller},
		"LectureFlow-" + version + "-x64-portable.exe": {PlatformWindowsX64, KindPortable},
		"LectureFlow-" + version + "-arm64.dmg":        {PlatformDarwinArm64, KindDMG},
		"LectureFlow-" + version + "-arm64-mac.zip":    {PlatformDarwinArm64, KindZip},
	}
}

// makeRelease builds a signed update.json for version (mod may change the
// manifest before it is signed); its packages live on f (when f != nil).
func makeRelease(t *testing.T, f *fakeGH, k testKey, version, channel string, mod func(*Manifest)) *release {
	t.Helper()
	base := "https://example.invalid"
	if f != nil {
		base = f.srv.URL
	}
	rel := &release{version: version, pkgs: map[string][]byte{}}
	m := &Manifest{Schema: 1, App: AppName, Version: version, Channel: channel, ReleasedAt: "2026-11-02T10:00:00Z",
		NotesURL: "https://example.invalid/notes", Platforms: map[string][]Asset{}}
	for name, pk := range assetNames(version) {
		c := pkgContent(name + version)
		rel.pkgs[name] = c
		m.Platforms[pk[0]] = append(m.Platforms[pk[0]], Asset{Kind: pk[1], Name: name,
			URL: base + downloadPath(version, name), Size: int64(len(c)), SHA256: sha(c)})
		if f != nil {
			f.set(downloadPath(version, name), c)
		}
	}
	if mod != nil {
		mod(m)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rel.m, rel.raw = m, raw
	rel.sig = k.sign(raw, TrustedComment(version), true)
	if f != nil {
		f.set(downloadPath(version, manifestFile), rel.raw)
		f.set(downloadPath(version, signatureFile), rel.sig)
	}
	return rel
}

// publishLatest makes rel the "latest" release.
func (f *fakeGH) publishLatest(rel *release) {
	f.set(latestPath, rel.raw)
	f.set(latestPath+".minisig", rel.sig)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestUpdater builds an Updater for windows-x64 3.1.0 talking to f.
func newTestUpdater(t *testing.T, f *fakeGH, k testKey, mod func(*Options)) *Updater {
	t.Helper()
	o := Options{DataDir: privateDataDir(t), Version: "3.1.0", KV: newMemKV(), Desktop: true, Platform: PlatformWindowsX64,
		PublicKey: k.pubFile(), Repo: "o/r", Downloader: &HTTPDownloader{Backoff: time.Millisecond},
		Log: quietLog(), RequestTimeout: 5 * time.Second}
	if f != nil {
		o.GitHubWeb = f.srv.URL
		o.GitHubAPI = f.srv.URL + "/api"
	}
	if mod != nil {
		mod(&o)
	}
	u := New(o)
	t.Cleanup(u.Close)
	u.Startup(context.Background())
	return u
}

// privateDataDir is a new data directory only this account may use, as
// LectureFlow creates them (dirperm): the system's temporary directory may
// let other accounts in (CheckTrust would refuse it).
func privateDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := dirperm.MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// waitFor polls the status until ok or fails after 10 s.
func waitFor(t *testing.T, u *Updater, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := u.Status(context.Background())
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; status %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func notState(s string) func(Status) bool { return func(st Status) bool { return st.State != s } }

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if CodeOf(err) != code {
		t.Fatalf("error %v (code %q), want %s", err, CodeOf(err), code)
	}
}
