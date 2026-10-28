// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The macOS registration (mac-fix, 2026-10-27) against a fake file system
// that behaves like macOS 27's App Data protection, on temporary
// directories: the Chrome, Edge and Firefox data directories are guarded
// as a whole (anything inside them answers EPERM and counts as a
// violation), their NativeMessagingHosts directories are not. The tests run
// on every system; nothing touches a real browser directory.

// macFS is the fake file system.
type macFS struct {
	mu sync.Mutex
	// guarded are the browsers' data directories; exempt the
	// NativeMessagingHosts directories inside them.
	guarded, exempt []string
	// deny: every operation on these paths and below answers EPERM;
	// denyWrite: writes below these directories do.
	deny, denyWrite map[string]bool
	// violations are the guarded paths the code under test reached.
	violations []string
}

func newMacFS(home string) *macFS {
	f := &macFS{deny: map[string]bool{}, denyWrite: map[string]bool{}}
	nmh := MacBrowserDirs(home)
	for _, b := range Browsers {
		f.guarded = append(f.guarded, MacBrowserDataDirs(home)[b])
		if b != BrowserFirefox {
			f.exempt = append(f.exempt, nmh[b])
		}
	}
	return f
}

func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

func eperm(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: syscall.EPERM} }

// check answers EPERM for a guarded or denied path (recording a guarded
// one as a violation).
func (f *macFS) check(op, p string, write bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for d := range f.deny {
		if within(p, d) {
			return eperm(op, p)
		}
	}
	if write {
		for d := range f.denyWrite {
			if within(p, d) {
				return eperm(op, p)
			}
		}
	}
	for _, g := range f.guarded {
		if !within(p, g) || filepath.Clean(p) == filepath.Clean(g) {
			continue // outside, or the data directory itself (a stat)
		}
		for _, e := range f.exempt {
			if within(p, e) {
				return nil
			}
		}
		f.violations = append(f.violations, op+" "+p)
		return eperm(op, p)
	}
	return nil
}

func (f *macFS) Stat(p string) (fs.FileInfo, error) {
	if err := f.check("stat", p, false); err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (f *macFS) ReadFile(p string) ([]byte, error) {
	if err := f.check("open", p, false); err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WriteFile is writeIfChanged; creating a missing directory is checked
// like a write into it (mkdir in a guarded directory is refused).
func (f *macFS) WriteFile(p string, b []byte, perm fs.FileMode) error {
	if err := f.check("open", p, true); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Dir(p)); errors.Is(err, fs.ErrNotExist) {
		if err := f.check("mkdir", filepath.Dir(p), true); err != nil {
			return err
		}
	}
	_, err := writeIfChanged(p, b, perm)
	return err
}

func (f *macFS) assertNoViolations(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.violations) > 0 {
		t.Fatalf("reached into a protected browser directory: %v", f.violations)
	}
}

// macFixture is a Mac home with an Applications directory.
type macFixture struct {
	*regFixture
	home, apps string
	fs         *macFS
	r          *Registrar
}

func newMacFixture(t *testing.T) *macFixture {
	t.Helper()
	f := &macFixture{regFixture: newRegFixture(t), home: t.TempDir(), apps: t.TempDir()}
	f.fs = newMacFS(f.home)
	return f
}

func (f *macFixture) opts() RegistrarOptions {
	o := f.regFixture.opts()
	o.Registry = nil
	o.BrowserDirs = MacBrowserDirs(f.home)
	o.FS = f.fs
	o.Installed = MacInstalled(MacBrowserDataDirs(f.home), f.fs, []string{f.apps, filepath.Join(f.home, "Applications")})
	return o
}

func (f *macFixture) registrar() *Registrar {
	if f.r == nil {
		f.r = NewRegistrar(f.opts())
	}
	return f.r
}

// app installs a browser's application bundle; started runs it once (its
// data directory, and for Chrome and Edge its NativeMessagingHosts).
func (f *macFixture) app(t *testing.T, b string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.apps, macBrowserApps[b], "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *macFixture) started(t *testing.T, b string) {
	t.Helper()
	os.MkdirAll(MacBrowserDataDirs(f.home)[b], 0o700)
	if b != BrowserFirefox {
		os.MkdirAll(MacBrowserDirs(f.home)[b], 0o700)
	}
}

func (f *macFixture) manifestOf(b string) string {
	return filepath.Join(MacBrowserDirs(f.home)[b], HostName+".json")
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Chrome has run (its NativeMessagingHosts exists): the manifest goes
// there. Edge is installed but has never run: its directory is not
// created, it is not_ready until it has run and the user registers again.
// Firefox is only an application (Firefox never makes the Mozilla
// directory): Mozilla/NativeMessagingHosts is created and Firefox is
// registered. Nothing inside a protected data directory is reached.
func TestMacRegisterNativeMessagingHostsDirs(t *testing.T) {
	f := newMacFixture(t)
	f.app(t, BrowserChrome)
	f.started(t, BrowserChrome)
	f.app(t, BrowserEdge)
	f.app(t, BrowserFirefox)
	r := f.registrar()

	res, err := r.Register()
	if err != nil || res.Error != "" || len(res.Notes) > 0 || len(res.Blocked) > 0 {
		t.Fatalf("register: %+v %v", res, err)
	}
	if !slices.Equal(res.NotReady, []string{BrowserEdge}) {
		t.Fatalf("not ready: %v", res.NotReady)
	}
	var cm, fm manifest
	readJSON(t, f.manifestOf(BrowserChrome), &cm)
	readJSON(t, f.manifestOf(BrowserFirefox), &fm)
	if cm.Path != f.host || len(cm.AllowedOrigins) != 1 || len(fm.AllowedExtensions) != 1 {
		t.Fatalf("manifests: %+v %+v", cm, fm)
	}
	if exists(MacBrowserDirs(f.home)[BrowserEdge]) || exists(MacBrowserDataDirs(f.home)[BrowserEdge]) {
		t.Fatal("Edge's directory was created")
	}
	st := r.Status()
	if s := states(st); s[BrowserChrome] != StateRegistered || s[BrowserEdge] != StateNotReady || s[BrowserFirefox] != StateRegistered {
		t.Fatalf("states %v", s)
	}
	if !slices.Equal(st.LastRegister.NotReady, []string{BrowserEdge}) {
		t.Fatalf("last register: %+v", st.LastRegister)
	}

	// The user opens Edge once: its directory exists, the manifest not yet.
	// The status registers it by itself (nm-harden; before, it was not
	// registered until "重新登记").
	f.started(t, BrowserEdge)
	st = r.Status()
	if s := states(st); s[BrowserEdge] != StateRegistered {
		t.Fatalf("after Edge ran: %v", s)
	}
	if st.LastRegister == nil || len(st.LastRegister.NotReady) > 0 || !exists(f.manifestOf(BrowserEdge)) {
		t.Fatalf("after Edge ran: %+v", st.LastRegister)
	}
	// "重新登记" still works and changes nothing.
	res, err = r.Register()
	if err != nil || len(res.NotReady) > 0 {
		t.Fatalf("register again: %+v %v", res, err)
	}
	if s := states(r.Status()); s[BrowserEdge] != StateRegistered {
		t.Fatalf("after registering again: %v", s)
	}
	f.fs.assertNoViolations(t)
}

// nm-harden: a browser opened for the first time while LectureFlow runs
// creates its NativeMessagingHosts directory; the registration follows by
// itself (Status, AutoRegister), at most once per browser every
// AutoRegisterEvery, never for a browser the system blocks, and never
// reaching into a protected directory.
func TestMacAutoRegister(t *testing.T) {
	f := newMacFixture(t)
	now := time.Date(2026, 10, 27, 4, 0, 0, 0, time.UTC)
	o := f.opts()
	o.Now = func() time.Time { return now }
	f.r = NewRegistrar(o)
	r := f.r
	f.app(t, BrowserChrome) // installed, never opened
	f.app(t, BrowserEdge)
	f.started(t, BrowserEdge)
	f.fs.mu.Lock()
	f.fs.denyWrite[MacBrowserDirs(f.home)[BrowserEdge]] = true // the system refuses Edge's directory
	f.fs.mu.Unlock()

	res, err := r.Register()
	if err == nil || !slices.Equal(res.NotReady, []string{BrowserChrome}) || !slices.Equal(res.Blocked, []string{BrowserEdge}) {
		t.Fatalf("register: %+v %v", res, err)
	}
	at := res.At
	if r.AutoRegister() {
		t.Fatal("registered again although nothing changed")
	}
	if s := states(r.Status()); s[BrowserChrome] != StateNotReady || s[BrowserEdge] != StateBlocked {
		t.Fatalf("before Chrome ran: %v", s)
	}

	// Chrome runs for the first time: the next look registers it.
	now = now.Add(time.Minute)
	f.started(t, BrowserChrome)
	if !r.AutoRegister() {
		t.Fatal("Chrome's new directory was not registered")
	}
	st := r.Status()
	if s := states(st); s[BrowserChrome] != StateRegistered || s[BrowserEdge] != StateBlocked {
		t.Fatalf("after Chrome ran: %v", s)
	}
	if !st.LastRegister.At.After(at) || !exists(f.manifestOf(BrowserChrome)) {
		t.Fatalf("last register %+v", st.LastRegister)
	}

	// Something removes the manifest: within AutoRegisterEvery of the last
	// automatic registration nothing happens, after it the status
	// registers again.
	os.Remove(f.manifestOf(BrowserChrome))
	now = now.Add(AutoRegisterEvery - time.Second)
	if r.AutoRegister() {
		t.Fatal("registered again within AutoRegisterEvery")
	}
	if s := states(r.Status()); s[BrowserChrome] != StateNotRegistered {
		t.Fatalf("manifest removed: %v", s)
	}
	now = now.Add(time.Second)
	if s := states(r.Status()); s[BrowserChrome] != StateRegistered {
		t.Fatalf("after AutoRegisterEvery: %v", s)
	}
	// Blocked Edge was never tried by itself: only the explicit Register
	// and the two automatic ones ran (each records Edge as blocked again).
	if s := states(r.Status()); s[BrowserEdge] != StateBlocked {
		t.Fatalf("edge: %v", s)
	}
	f.fs.assertNoViolations(t)
}

// The service looks for new browser directories by itself (macOS-style
// registrar only) and stops looking when closed.
func TestServiceWatchesBrowserDirs(t *testing.T) {
	f := newMacFixture(t)
	f.app(t, BrowserChrome)
	r := f.registrar()
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	svc := StartService(ServiceOptions{Endpoint: "", Handler: newFakeHandler(), Registrar: r, WatchEvery: 10 * time.Millisecond})
	f.started(t, BrowserChrome)
	waitFor(t, func() bool { return exists(f.manifestOf(BrowserChrome)) })
	svc.Close()
	// closed: a removed manifest stays removed
	os.Remove(f.manifestOf(BrowserChrome))
	time.Sleep(50 * time.Millisecond)
	if exists(f.manifestOf(BrowserChrome)) {
		t.Fatal("the service registered after Close")
	}
	f.fs.assertNoViolations(t)

	// The Windows registration (registry) is not watched.
	w := StartService(ServiceOptions{Endpoint: "", Handler: newFakeHandler(), Registrar: NewRegistrar(newRegFixture(t).opts()), WatchEvery: time.Millisecond})
	defer w.Close()
	if w.stop != nil {
		t.Fatal("a registry registration is watched")
	}
}

// A browser counts as installed by its application (in /Applications or
// ~/Applications) or by its own data directory, which is only stat'ed.
func TestMacInstalled(t *testing.T) {
	f := newMacFixture(t)
	inst := f.opts().Installed
	for _, b := range Browsers {
		if inst(b) {
			t.Fatalf("%s installed on an empty Mac", b)
		}
	}
	// ~/Applications
	os.MkdirAll(filepath.Join(f.home, "Applications", "Firefox.app"), 0o755)
	// a data directory without the application where we look (Chrome
	// installed elsewhere)
	os.MkdirAll(MacBrowserDataDirs(f.home)[BrowserChrome], 0o700)
	if !inst(BrowserFirefox) || !inst(BrowserChrome) || inst(BrowserEdge) {
		t.Fatalf("chrome %v edge %v firefox %v", inst(BrowserChrome), inst(BrowserEdge), inst(BrowserFirefox))
	}
	// A file named like the application is not one.
	os.WriteFile(filepath.Join(f.apps, "Microsoft Edge.app"), []byte("x"), 0o644)
	if inst(BrowserEdge) {
		t.Fatal("a file counts as Edge's application")
	}
	f.fs.assertNoViolations(t)
}

// Writing into Chrome's NativeMessagingHosts is refused (EPERM, a fallback
// case): Chrome is blocked — in the result and in the status, although its
// directory can be read — and Firefox is registered all the same. Once
// nothing but blocked browsers is left, Register fails with the Full Disk
// Access hint. Allowed again, "重新登记" registers it.
func TestMacRegisterBlocked(t *testing.T) {
	f := newMacFixture(t)
	f.app(t, BrowserChrome)
	f.started(t, BrowserChrome)
	f.app(t, BrowserFirefox)
	nmh := MacBrowserDirs(f.home)[BrowserChrome]
	f.fs.denyWrite[nmh] = true
	r := f.registrar()

	res, err := r.Register()
	if err != nil || !slices.Equal(res.Blocked, []string{BrowserChrome}) || len(res.Notes) != 1 || !strings.HasPrefix(res.Notes[0], "chrome: ") {
		t.Fatalf("register: %+v %v", res, err)
	}
	if s := states(r.Status()); s[BrowserChrome] != StateBlocked || s[BrowserFirefox] != StateRegistered || s[BrowserEdge] != StateNotInstalled {
		t.Fatalf("states %v", s)
	}
	// Even listing the directory is refused now.
	f.fs.deny[nmh] = true
	if s := states(r.Status()); s[BrowserChrome] != StateBlocked {
		t.Fatalf("stat refused: %v", s)
	}

	// Firefox gone: only the blocked Chrome is left.
	os.RemoveAll(filepath.Join(f.apps, "Firefox.app"))
	res, err = r.Register()
	if err == nil || !strings.Contains(res.Error, "Full Disk Access") || !slices.Equal(res.Blocked, []string{BrowserChrome}) {
		t.Fatalf("register with Chrome blocked only: %+v %v", res, err)
	}

	// The user allows LectureFlow (Full Disk Access) and registers again.
	delete(f.fs.deny, nmh)
	delete(f.fs.denyWrite, nmh)
	res, err = r.Register()
	if err != nil || len(res.Blocked) > 0 {
		t.Fatalf("register after allowing: %+v %v", res, err)
	}
	if s := states(r.Status()); s[BrowserChrome] != StateRegistered {
		t.Fatalf("after allowing: %v", s)
	}
	f.fs.assertNoViolations(t)
}

// A manifest that is there but cannot be read is blocked, not "not
// registered" (registering again would not help).
func TestMacStatusUnreadableManifest(t *testing.T) {
	f := newMacFixture(t)
	f.app(t, BrowserEdge)
	f.started(t, BrowserEdge)
	r := f.registrar()
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	f.fs.mu.Lock()
	f.fs.deny[f.manifestOf(BrowserEdge)] = true
	f.fs.mu.Unlock()
	if s := states(r.Status()); s[BrowserEdge] != StateBlocked {
		t.Fatalf("states %v", s)
	}
}

// Chrome installed but never started, and nothing else: no error (there is
// nothing wrong to report), the browser is not_ready.
func TestMacRegisterOnlyNotReady(t *testing.T) {
	f := newMacFixture(t)
	f.app(t, BrowserChrome)
	r := f.registrar()
	res, err := r.Register()
	if err != nil || res.Error != "" || !slices.Equal(res.NotReady, []string{BrowserChrome}) {
		t.Fatalf("register: %+v %v", res, err)
	}
	if exists(MacBrowserDataDirs(f.home)[BrowserChrome]) {
		t.Fatal("Chrome's data directory was created")
	}
	if s := states(r.Status()); s[BrowserChrome] != StateNotReady {
		t.Fatalf("states %v", s)
	}
	f.fs.assertNoViolations(t)
}

// An app that runs from a place that disappears writes no launch.json (an
// existing one stays) and says why; an unknown reason is ignored.
func TestRegisterLauncherBlocked(t *testing.T) {
	f := newRegFixture(t)
	os.MkdirAll(f.dir, 0o755)
	old := []byte(`{"exe":"/Applications/LectureFlow.app/Contents/MacOS/LectureFlow","args":["--background"]}`)
	os.WriteFile(filepath.Join(f.dir, LaunchFile), old, 0o644)
	o := f.opts()
	o.Launcher = "/private/var/folders/x/T/AppTranslocation/ABC/d/LectureFlow.app/Contents/MacOS/LectureFlow"
	o.LauncherBlocked = LauncherTranslocated
	r := NewRegistrar(o)
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, LaunchFile)); string(b) != string(old) {
		t.Fatalf("launch.json changed: %s", b)
	}
	if st := r.Status(); st.LauncherBlocked != LauncherTranslocated {
		t.Fatalf("status: %+v", st)
	}

	o.LauncherBlocked = "elsewhere"
	o.Launcher = filepath.Join(f.bin, "LectureFlow.exe")
	r = NewRegistrar(o)
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	var lc LaunchConfig
	readJSON(t, filepath.Join(f.dir, LaunchFile), &lc)
	if lc.Exe != o.Launcher || r.Status().LauncherBlocked != "" {
		t.Fatalf("unknown reason: %+v %+v", lc, r.Status())
	}
}

func TestUnstableLauncher(t *testing.T) {
	ro := map[string]bool{"/Volumes/LectureFlow 3.2.0-arm64": true}
	readOnly := func(dir string) bool { return ro[dir] }
	for _, c := range []struct {
		goos, exe, want string
	}{
		{"darwin", "/Applications/LectureFlow.app/Contents/MacOS/LectureFlow", ""},
		{"darwin", "/private/var/folders/ab/T/AppTranslocation/1F2E/d/LectureFlow.app/Contents/MacOS/LectureFlow", LauncherTranslocated},
		{"darwin", "/Volumes/LectureFlow 3.2.0-arm64/LectureFlow.app/Contents/MacOS/LectureFlow", LauncherDiskImage},
		{"darwin", "/Volumes/External/Apps/LectureFlow.app/Contents/MacOS/LectureFlow", ""}, // a writable disk: the user's choice
		{"darwin", "/Volumes/", ""},
		{"darwin", "", ""},
		{"windows", `C:\Users\u\AppData\Local\Programs\LectureFlow\LectureFlow.exe`, ""},
		{"windows", "/private/var/folders/ab/T/AppTranslocation/1F2E/d/LectureFlow.app/Contents/MacOS/LectureFlow", ""},
	} {
		if got := UnstableLauncher(c.goos, c.exe, readOnly); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.goos, c.exe, got, c.want)
		}
	}
	if UnstableLauncher("darwin", "/Volumes/LectureFlow 3.2.0-arm64/LectureFlow.app/Contents/MacOS/LectureFlow", nil) != "" {
		t.Error("no read-only test, yet a disk image")
	}
	if !ValidLauncherBlocked(LauncherTranslocated) || !ValidLauncherBlocked(LauncherDiskImage) || ValidLauncherBlocked("") || ValidLauncherBlocked("x") {
		t.Error("ValidLauncherBlocked")
	}
}
