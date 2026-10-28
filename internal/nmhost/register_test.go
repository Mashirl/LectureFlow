// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests never touch the real registry or a real browser directory:
// the registry is a map and every directory is temporary.

type fakeRegistry struct {
	mu   sync.Mutex
	vals map[string]string
	sets int
	fail map[string]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{vals: map[string]string{}, fail: map[string]bool{}}
}

func (r *fakeRegistry) GetDefault(key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vals[key]
	return v, ok, nil
}

func (r *fakeRegistry) SetDefault(key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail[key] {
		return errors.New("access denied")
	}
	r.sets++
	r.vals[key] = value
	return nil
}

type regFixture struct {
	dir, bin, host string
	reg            *fakeRegistry
	installed      map[string]bool
}

func newRegFixture(t *testing.T) *regFixture {
	t.Helper()
	f := &regFixture{dir: filepath.Join(t.TempDir(), "nmhost"), bin: t.TempDir(), reg: newFakeRegistry(),
		installed: map[string]bool{BrowserChrome: true, BrowserEdge: true, BrowserFirefox: true}}
	f.host = filepath.Join(f.bin, HostExe())
	if err := os.WriteFile(f.host, []byte("host v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *regFixture) opts() RegistrarOptions {
	return RegistrarOptions{
		Dir:         f.dir,
		HostSource:  f.host,
		Registry:    f.reg,
		Installed:   func(b string) bool { return f.installed[b] },
		ChromiumIDs: []string{"abcdefghijklmnopabcdefghijklmnop"},
		FirefoxIDs:  []string{"lectureflow@qu1ncy.dev"},
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func states(st Status) map[string]string {
	m := map[string]string{}
	for _, b := range st.Browsers {
		m[b.Browser] = b.State
	}
	return m
}

func TestRegisterWindows(t *testing.T) {
	f := newRegFixture(t)
	o := f.opts()
	o.Launcher = filepath.Join(f.bin, "LectureFlow.exe")
	r := NewRegistrar(o)

	before := r.Status()
	for _, b := range before.Browsers {
		if b.State != StateNotRegistered {
			t.Fatalf("before: %+v", before.Browsers)
		}
	}
	if before.LastRegister != nil || before.AutoStart {
		t.Fatalf("before: %+v", before)
	}

	res, err := r.Register()
	if err != nil || res.Error != "" || len(res.Notes) > 0 {
		t.Fatalf("register: %+v %v", res, err)
	}
	var chromium, firefox map[string]any
	readJSON(t, filepath.Join(f.dir, ChromiumManifest), &chromium)
	readJSON(t, filepath.Join(f.dir, FirefoxManifest), &firefox)
	if chromium["name"] != HostName || chromium["type"] != "stdio" || chromium["path"] != f.host {
		t.Fatalf("chromium manifest: %v", chromium)
	}
	if o := chromium["allowed_origins"].([]any); len(o) != 1 || o[0] != "chrome-extension://abcdefghijklmnopabcdefghijklmnop/" || chromium["allowed_extensions"] != nil {
		t.Fatalf("chromium allowed_origins: %v", chromium)
	}
	if e := firefox["allowed_extensions"].([]any); len(e) != 1 || e[0] != "lectureflow@qu1ncy.dev" || firefox["allowed_origins"] != nil {
		t.Fatalf("firefox allowed_extensions: %v", firefox)
	}
	want := map[string]string{
		`Software\Google\Chrome\NativeMessagingHosts\org.lectureflow.nmhost`:  filepath.Join(f.dir, ChromiumManifest),
		`Software\Microsoft\Edge\NativeMessagingHosts\org.lectureflow.nmhost`: filepath.Join(f.dir, ChromiumManifest),
		`Software\Mozilla\NativeMessagingHosts\org.lectureflow.nmhost`:        filepath.Join(f.dir, FirefoxManifest),
	}
	if len(f.reg.vals) != 3 {
		t.Fatalf("registry: %v", f.reg.vals)
	}
	for k, v := range want {
		if f.reg.vals[k] != v {
			t.Fatalf("registry %s = %q, want %q", k, f.reg.vals[k], v)
		}
	}
	var lc LaunchConfig
	readJSON(t, filepath.Join(f.dir, LaunchFile), &lc)
	if lc.Exe != o.Launcher || len(lc.Args) != 1 || lc.Args[0] != "--background" {
		t.Fatalf("launch.json: %+v", lc)
	}

	st := r.Status()
	for b, s := range states(st) {
		if s != StateRegistered {
			t.Fatalf("%s: %s", b, s)
		}
	}
	if !st.HostPresent || st.Host != f.host || !st.AutoStart || st.LastRegister == nil || st.LastRegister.At.IsZero() {
		t.Fatalf("status: %+v", st)
	}

	// Idempotent: nothing is written again.
	mt := modTimes(t, f.dir)
	time.Sleep(20 * time.Millisecond)
	sets := f.reg.sets
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	if f.reg.sets != sets {
		t.Fatalf("registry written again (%d → %d)", sets, f.reg.sets)
	}
	if got := modTimes(t, f.dir); !equalMaps(got, mt) {
		t.Fatalf("files rewritten: %v → %v", mt, got)
	}

	// The host program disappears: the manifests point at nothing.
	os.Remove(f.host)
	if s := states(r.Status()); s[BrowserChrome] != StateHostMissing || s[BrowserFirefox] != StateHostMissing {
		t.Fatalf("host gone: %v", s)
	}
	os.WriteFile(f.host, []byte("host v1"), 0o755)

	// A browser that is not installed.
	f.installed[BrowserFirefox] = false
	if s := states(r.Status()); s[BrowserFirefox] != StateNotInstalled || s[BrowserChrome] != StateRegistered {
		t.Fatalf("firefox missing: %v", s)
	}
	f.installed[BrowserFirefox] = true

	// Another program took our key, or an old manifest lacks the
	// extension ID: not registered (until the next Register).
	f.reg.vals[RegistryKey(BrowserEdge)] = filepath.Join(f.bin, "other.json")
	os.WriteFile(filepath.Join(f.bin, "other.json"), []byte(`{"name":"org.lectureflow.nmhost","path":"x","allowed_origins":["chrome-extension://zzz/"]}`), 0o644)
	if s := states(r.Status()); s[BrowserEdge] != StateNotRegistered {
		t.Fatalf("foreign manifest: %v", s)
	}
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	if s := states(r.Status()); s[BrowserEdge] != StateRegistered {
		t.Fatalf("after registering again: %v", s)
	}
}

func TestRegisterWithoutHostWritesNothing(t *testing.T) {
	f := newRegFixture(t)
	o := f.opts()
	o.HostSource = filepath.Join(f.bin, "missing.exe")
	o.Launcher = "x"
	r := NewRegistrar(o)
	res, err := r.Register()
	if !errors.Is(err, ErrHostMissing) || res.Error == "" {
		t.Fatalf("register: %+v %v", res, err)
	}
	if _, err := os.Stat(f.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the directory was created")
	}
	if len(f.reg.vals) != 0 {
		t.Fatalf("registry written: %v", f.reg.vals)
	}
	if st := r.Status(); st.LastRegister == nil || st.LastRegister.Error == "" {
		t.Fatalf("status does not report the failure: %+v", st)
	}
}

func TestRegisterWebModeLeavesLaunchJSON(t *testing.T) {
	f := newRegFixture(t)
	os.MkdirAll(f.dir, 0o755)
	old := []byte(`{"exe":"C:\\LectureFlow\\LectureFlow.exe","args":["--background"]}`)
	os.WriteFile(filepath.Join(f.dir, LaunchFile), old, 0o644)
	r := NewRegistrar(f.opts()) // no Launcher: web mode
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, LaunchFile)); string(b) != string(old) {
		t.Fatalf("launch.json changed: %s", b)
	}
}

func TestRegisterPartialFailure(t *testing.T) {
	f := newRegFixture(t)
	f.reg.fail[RegistryKey(BrowserEdge)] = true
	r := NewRegistrar(f.opts())
	res, err := r.Register()
	if err != nil || len(res.Notes) != 1 {
		t.Fatalf("register: %+v %v", res, err)
	}
	s := states(r.Status())
	if s[BrowserEdge] != StateNotRegistered || s[BrowserChrome] != StateRegistered || s[BrowserFirefox] != StateRegistered {
		t.Fatalf("states %v", s)
	}
	// Nothing could be registered at all: an error.
	for _, b := range Browsers {
		f.reg.fail[RegistryKey(b)] = true
	}
	f.reg.vals = map[string]string{}
	if _, err := r.Register(); err == nil {
		t.Fatal("no error when no browser could be registered")
	}
}

func TestRegisterCopiesHost(t *testing.T) {
	f := newRegFixture(t)
	o := f.opts()
	o.CopyHost = true
	o.CopyDir = filepath.Join(t.TempDir(), "cache", "nmhost")
	r := NewRegistrar(o)
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(o.CopyDir, HostExe())
	if b, _ := os.ReadFile(cp); string(b) != "host v1" {
		t.Fatalf("copy: %q", b)
	}
	var m manifest
	readJSON(t, filepath.Join(f.dir, ChromiumManifest), &m)
	if m.Path != cp {
		t.Fatalf("manifest path %s, want the copy %s", m.Path, cp)
	}
	if st := r.Status(); st.Host != cp || !st.HostPresent {
		t.Fatalf("status: %+v", st)
	}
	// Same content: not copied again.
	fi1, _ := os.Stat(cp)
	time.Sleep(20 * time.Millisecond)
	r.Register()
	fi2, _ := os.Stat(cp)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("identical host copied again")
	}
	// A new version replaces the copy; no temporary file stays.
	os.WriteFile(f.host, []byte("host v2"), 0o755)
	r.Register()
	if b, _ := os.ReadFile(cp); string(b) != "host v2" {
		t.Fatalf("copy after update: %q", b)
	}
	entries, _ := os.ReadDir(o.CopyDir)
	if len(entries) != 1 {
		t.Fatalf("copy dir: %v", entries)
	}
}

func TestRegisterMacBrowserDirs(t *testing.T) {
	f := newRegFixture(t)
	home := t.TempDir()
	dirs := MacBrowserDirs(home)
	// Chrome and Firefox have run here (Chrome made its
	// NativeMessagingHosts directory at its first start, Firefox only its
	// own data directory); Edge never has. (mac-fix: Chrome's directory is
	// no longer created by the registration; register_mac_test.go.)
	os.MkdirAll(dirs[BrowserChrome], 0o700)
	os.MkdirAll(MacBrowserDataDirs(home)[BrowserFirefox], 0o755)
	o := f.opts()
	o.Registry = nil
	o.BrowserDirs = dirs
	o.Installed = MacInstalled(MacBrowserDataDirs(home), OSBrowserFS{}, nil)
	r := NewRegistrar(o)
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	var cm, fm map[string]any
	readJSON(t, filepath.Join(dirs[BrowserChrome], HostName+".json"), &cm)
	readJSON(t, filepath.Join(dirs[BrowserFirefox], HostName+".json"), &fm)
	if cm["allowed_origins"] == nil || fm["allowed_extensions"] == nil || cm["path"] != f.host {
		t.Fatalf("manifests: %v %v", cm, fm)
	}
	if _, err := os.Stat(filepath.Dir(dirs[BrowserEdge])); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a directory was created for a browser that is not installed")
	}
	s := states(r.Status())
	if s[BrowserChrome] != StateRegistered || s[BrowserFirefox] != StateRegistered || s[BrowserEdge] != StateNotInstalled {
		t.Fatalf("states %v", s)
	}
}

func TestRegisterKeys(t *testing.T) {
	var keys []string
	for _, b := range Browsers {
		keys = append(keys, RegistryKey(b))
	}
	sort.Strings(keys)
	want := []string{
		`Software\Google\Chrome\NativeMessagingHosts\org.lectureflow.nmhost`,
		`Software\Microsoft\Edge\NativeMessagingHosts\org.lectureflow.nmhost`,
		`Software\Mozilla\NativeMessagingHosts\org.lectureflow.nmhost`,
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys %v", keys)
		}
	}
}

func modTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	m := map[string]time.Time{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, _ := e.Info()
		m[e.Name()] = fi.ModTime()
	}
	return m
}

func equalMaps(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !b[k].Equal(v) {
			return false
		}
	}
	return true
}

// Until the extension's ID is known the Chromium manifest still carries an
// (empty) allowed_origins: Chrome rejects a manifest without it.
func TestManifestKeysAlwaysPresent(t *testing.T) {
	f := newRegFixture(t)
	o := f.opts()
	o.ChromiumIDs = []string{}
	r := NewRegistrar(o)
	if _, err := r.Register(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, ChromiumManifest))
	var m map[string]any
	json.Unmarshal(b, &m)
	if o, ok := m["allowed_origins"].([]any); !ok || len(o) != 0 || m["allowed_extensions"] != nil {
		t.Fatalf("chromium manifest %s", b)
	}
	b, _ = os.ReadFile(filepath.Join(f.dir, FirefoxManifest))
	m = nil
	json.Unmarshal(b, &m)
	if _, ok := m["allowed_extensions"].([]any); !ok || m["allowed_origins"] != nil {
		t.Fatalf("firefox manifest %s", b)
	}
}

// Security review L4 (nm-harden): the development extension's ID comes from
// a public key, so a release build admits only the Chrome Web Store ID;
// a development build, or LECTUREFLOW_DEV_EXTENSION=1, admits both. The
// Firefox add-on id is the same in every build.
func TestRegisterExtensionIDs(t *testing.T) {
	store := "chrome-extension://" + ChromeWebStoreID + "/"
	dev := "chrome-extension://" + ChromiumDevID + "/"
	cases := []struct {
		name, why string
		origins   []string
	}{
		{"release", "", []string{store}},
		{"development build", DevExtensionBuild, []string{store, dev}},
		{"LECTUREFLOW_DEV_EXTENSION", DevExtensionEnv, []string{store, dev}},
		{"unknown reason is release", "maybe", []string{store}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRegFixture(t)
			o := f.opts()
			o.ChromiumIDs, o.FirefoxIDs = nil, nil // the package's lists
			o.DevExtension = c.why
			r := NewRegistrar(o)
			if _, err := r.Register(); err != nil {
				t.Fatal(err)
			}
			var cm, fm manifest
			readJSON(t, filepath.Join(f.dir, ChromiumManifest), &cm)
			readJSON(t, filepath.Join(f.dir, FirefoxManifest), &fm)
			if !slices.Equal(cm.AllowedOrigins, c.origins) {
				t.Fatalf("allowed_origins %v, want %v", cm.AllowedOrigins, c.origins)
			}
			if !slices.Equal(fm.AllowedExtensions, []string{"lectureflow@qu1ncy.dev"}) {
				t.Fatalf("allowed_extensions %v", fm.AllowedExtensions)
			}
			st := r.Status()
			wantWhy := c.why
			if wantWhy != DevExtensionBuild && wantWhy != DevExtensionEnv {
				wantWhy = ""
			}
			if st.DevExtension != wantWhy {
				t.Fatalf("status dev_extension %q, want %q", st.DevExtension, wantWhy)
			}
			for b, s := range states(st) {
				if s != StateRegistered {
					t.Fatalf("%s: %s", b, s)
				}
			}
		})
	}

	// A manifest a development build wrote (admitting the development
	// extension) is not a release build's registration: the status says
	// so, and the release build's Register takes the ID out.
	f := newRegFixture(t)
	o := f.opts()
	o.ChromiumIDs, o.DevExtension = nil, DevExtensionBuild
	if _, err := NewRegistrar(o).Register(); err != nil {
		t.Fatal(err)
	}
	o.DevExtension = ""
	release := NewRegistrar(o)
	if s := states(release.Status()); s[BrowserChrome] != StateNotRegistered || s[BrowserEdge] != StateNotRegistered || s[BrowserFirefox] != StateRegistered {
		t.Fatalf("release build reading a development manifest: %v", s)
	}
	if _, err := release.Register(); err != nil {
		t.Fatal(err)
	}
	var cm manifest
	readJSON(t, filepath.Join(f.dir, ChromiumManifest), &cm)
	if !slices.Equal(cm.AllowedOrigins, []string{store}) {
		t.Fatalf("after the release build registered: %v", cm.AllowedOrigins)
	}
	if s := states(release.Status()); s[BrowserChrome] != StateRegistered {
		t.Fatalf("after the release build registered: %v", s)
	}
}

// DevExtension: a build without a release version is a development build;
// a release build admits the development extension only with
// LECTUREFLOW_DEV_EXTENSION set to 1 (or true).
func TestDevExtension(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvDevExtension {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		version, env, want string
	}{
		{"dev", "", DevExtensionBuild},
		{"", "", DevExtensionBuild},
		{" dev ", "0", DevExtensionBuild},
		{"3.1.0", "", ""},
		{"3.1.0", "1", DevExtensionEnv},
		{"3.1.0", " true ", DevExtensionEnv},
		{"3.1.0", "0", ""},
		{"3.1.0", "false", ""},
		{"3.1.0", "yes", ""},
		{"3.1.0-beta.1", "1", DevExtensionEnv},
	}
	for _, c := range cases {
		if got := DevExtension(c.version, env(c.env)); got != c.want {
			t.Errorf("DevExtension(%q, %s=%q) = %q, want %q", c.version, EnvDevExtension, c.env, got, c.want)
		}
	}
	if got := DevExtension("3.1.0", nil); got != "" {
		t.Errorf("no getenv: %q", got)
	}
	if ids := ChromiumExtensionIDs(false); !slices.Equal(ids, []string{ChromeWebStoreID}) {
		t.Errorf("release IDs %v", ids)
	}
	if ids := ChromiumExtensionIDs(true); !slices.Equal(ids, []string{ChromeWebStoreID, ChromiumDevID}) {
		t.Errorf("development IDs %v", ids)
	}
	// Chrome extension IDs are 32 letters a–p.
	for _, id := range []string{ChromeWebStoreID, ChromiumDevID} {
		if len(id) != 32 || strings.Trim(id, "abcdefghijklmnop") != "" {
			t.Errorf("%q is not a Chrome extension ID", id)
		}
	}
}
