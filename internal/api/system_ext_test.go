// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/nmhost"
)

// fakeAria2cVersion answers aria2c --version for exe (other commands as
// fakeCmd) and counts the calls.
func fakeAria2cVersion(exe *string, calls *int) func(ctx context.Context, name string, args ...string) (string, string, error) {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		if name == *exe && len(args) == 1 && args[0] == "--version" {
			*calls++
			return "aria2 version 1.37.0\nCopyright (C) 2006, 2019 Tatsuhiro Tsujikawa\n", "", nil
		}
		return fakeCmd(ctx, name, args...)
	}
}

// The aria2c row: version and SHA-256; on Windows the bundled file is
// compared with the built-in manifest's core record; macOS and an aria2c
// that is not the bundled one are shown only; a missing aria2c is an error
// in the desktop application and a warning in web mode. Read only: nothing
// is downloaded or started besides aria2c --version.
func TestAria2cCheck(t *testing.T) {
	var exe string
	var calls int
	e := newEnv(t, func(s *Server) { s.runCmd = fakeAria2cVersion(&exe, &calls) })
	// The startup checks run checkAria2c in the background too: let them
	// finish before the configuration points at the test's file, or a late
	// run under load counts one --version more (and races on calls / e.cfg).
	e.srv.diag.waitStartup()
	ctx := context.Background()
	res := filepath.Join(t.TempDir(), "resources")
	os.MkdirAll(filepath.Join(res, "bin"), 0o755)
	content := []byte("not really aria2c")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	// the built-in manifest with this file's hash for bin/aria2c.exe
	man, err := os.ReadFile(filepath.Join("..", "..", "packaging", "components", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	const realSum = "be2099c214f63a3cb4954b09a0becd6e2e34660b886d4c898d260febfe9d70c2"
	if !bytes.Contains(man, []byte(realSum)) {
		t.Fatal("the built-in manifest has no aria2c record")
	}
	manPath := filepath.Join(res, "components.json")
	os.WriteFile(manPath, bytes.ReplaceAll(man, []byte(realSum), []byte(hexSum)), 0o644)
	e.cfg.ComponentsManifest = manPath

	orig := aria2cGOOS
	t.Cleanup(func() { aria2cGOOS = orig })
	for _, goos := range []string{"windows", "darwin"} {
		aria2cGOOS = goos
		name := "aria2c"
		if goos == "windows" {
			name += ".exe"
		}
		exe = filepath.Join(res, "bin", name)
		os.WriteFile(exe, content, 0o755)
		e.cfg.ResourcesDir, e.cfg.Aria2c = res, exe
		c := e.srv.checkAria2c(ctx)
		wantCode := codeAria2cVerified
		if goos == "darwin" {
			wantCode = codeAria2cOK // shown only
		}
		if c.Status != statusOK || c.DetailInfo == nil || c.DetailInfo.Code != wantCode ||
			c.DetailInfo.Params["version"] != "aria2 version 1.37.0" || c.DetailInfo.Params["sha256"] != hexSum {
			t.Fatalf("%s bundled: %+v %+v", goos, c, c.DetailInfo)
		}
		// a changed file
		os.WriteFile(exe, append(content, '!'), 0o755)
		c = e.srv.checkAria2c(ctx)
		if goos == "windows" {
			if c.Status != statusError || c.DetailInfo.Code != codeAria2cMismatch || c.DetailInfo.Params["expected"] != hexSum ||
				c.HintInfo == nil || c.HintInfo.Code != codeHintAria2cDesktop {
				t.Fatalf("windows changed: %+v %+v", c, c.DetailInfo)
			}
		} else if c.Status != statusOK || c.DetailInfo.Code != codeAria2cOK {
			t.Fatalf("darwin changed: %+v", c)
		}
	}
	// not the bundled aria2c (web mode: PATH or config.toml): shown only
	aria2cGOOS = "windows"
	other := filepath.Join(t.TempDir(), "aria2c.exe")
	os.WriteFile(other, []byte("someone's aria2c"), 0o755)
	exe, e.cfg.Aria2c, e.cfg.ResourcesDir = other, other, ""
	c := e.srv.checkAria2c(ctx)
	if c.Status != statusOK || c.DetailInfo.Code != codeAria2cExternal || c.DetailInfo.Params["path"] != other {
		t.Fatalf("external: %+v %+v", c, c.DetailInfo)
	}
	// missing: a warning in web mode, an error in the desktop application
	e.cfg.Aria2c = filepath.Join(t.TempDir(), "aria2c.exe")
	c = e.srv.checkAria2c(ctx)
	if c.Status != statusWarn || c.DetailInfo.Code != codeAria2cMissing || c.HintInfo.Code != codeHintAria2cDev {
		t.Fatalf("missing (web): %+v", c)
	}
	e.srv.diag.desktop = true
	c = e.srv.checkAria2c(ctx)
	if c.Status != statusError || c.HintInfo.Code != codeHintAria2cDesktop {
		t.Fatalf("missing (desktop): %+v", c)
	}
	if calls != 5 {
		t.Fatalf("aria2c --version ran %d times, want once per found file (5)", calls)
	}
}

// fakeNM is the Native Messaging service as the diagnostics see it.
type fakeNM struct {
	st        nmhost.Status
	registers int
}

func (f *fakeNM) Status() nmhost.Status { return f.st }
func (f *fakeNM) Register() (nmhost.Status, error) {
	f.registers++
	for i := range f.st.Browsers {
		if f.st.Browsers[i].State == nmhost.StateNotRegistered {
			f.st.Browsers[i].State = nmhost.StateRegistered
		}
	}
	f.st.LastRegister = &nmhost.RegisterResult{At: time.Now()}
	return f.st, nil
}

func browsers(states ...string) []nmhost.BrowserStatus {
	names := []string{nmhost.BrowserChrome, nmhost.BrowserEdge, nmhost.BrowserFirefox}
	out := make([]nmhost.BrowserStatus, len(states))
	for i, s := range states {
		out[i] = nmhost.BrowserStatus{Browser: names[i], State: s}
	}
	return out
}

func TestBrowserExtCheck(t *testing.T) {
	e := newEnv(t)
	at := time.Date(2026, 10, 27, 9, 30, 0, 0, time.Local)
	reg, not, none, gone := nmhost.StateRegistered, nmhost.StateNotRegistered, nmhost.StateNotInstalled, nmhost.StateHostMissing
	base := func(bs []nmhost.BrowserStatus) nmhost.Status {
		return nmhost.Status{Managed: true, Listening: true, Browsers: bs, Host: `C:\LF\resources\bin\lectureflow-nmhost.exe`, HostPresent: true}
	}
	cases := []struct {
		name   string
		st     *nmhost.Status // nil: no service
		status string
		code   string
		hint   string
		params map[string]any
	}{
		{"no service", nil, statusWarn, codeBrowserExtUnavailable, codeHintBrowserExtRestart, nil},
		{"off", &nmhost.Status{ListenError: nmhost.ListenOff}, statusOK, codeBrowserExtOff, "", nil},
		{"in use", &nmhost.Status{ListenError: nmhost.ListenInUse}, statusWarn, codeBrowserExtInUse, codeHintBrowserExtInUse, nil},
		{"listen failed", &nmhost.Status{ListenError: nmhost.ListenFailed}, statusWarn, codeBrowserExtListenFailed, codeHintBrowserExtRestart, nil},
		{"unmanaged", &nmhost.Status{Listening: true, Browsers: []nmhost.BrowserStatus{}}, statusOK, codeBrowserExtUnmanaged, "", nil},
		{"no browser", stPtr(base(browsers(none, none, none))), statusOK, codeBrowserExtNoBrowser, "", nil},
		{"not registered", stPtr(base(browsers(reg, none, not))), statusWarn, codeBrowserExtNotRegistered, codeHintBrowserExtDev,
			map[string]any{"browsers": "Firefox"}},
		{"host missing", stPtr(base(browsers(gone, reg, none))), statusWarn, codeBrowserExtHostMissing, codeHintBrowserExtDev,
			map[string]any{"path": `C:\LF\resources\bin\lectureflow-nmhost.exe`}},
		{"registered", stPtr(base(browsers(reg, reg, none))), statusOK, codeBrowserExtRegistered, "", map[string]any{"browsers": "Chrome, Edge"}},
		{"connected", func() *nmhost.Status {
			s := base(browsers(reg, none, reg))
			s.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserFirefox, ExtensionVersion: "0.1.0", Protocol: 1, Accepted: true, At: at}
			return &s
		}(), statusOK, codeBrowserExtConnected, "", map[string]any{"browsers": "Chrome, Firefox", "browser": "Firefox", "version": "0.1.0", "at": "2026-10-27 09:30"}},
		{"old extension", func() *nmhost.Status {
			s := base(browsers(reg, none, none))
			s.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserChrome, ExtensionVersion: "0.0.1", Protocol: 0, Accepted: false, At: at}
			return &s
		}(), statusWarn, codeBrowserExtProtocol, codeHintBrowserExtUpdate, map[string]any{"browser": "Chrome", "version": "0.0.1"}},
		// macOS (mac-fix): the system refused a browser's directory (before
		// anything else: registering again does not help yet), a browser
		// that has not made its directory, an app on a disk image.
		{"blocked", stPtr(base(browsers(nmhost.StateBlocked, gone, reg))), statusWarn, codeBrowserExtBlocked, codeHintBrowserExtFullDisk,
			map[string]any{"browsers": "Chrome"}},
		{"not ready", stPtr(base(browsers(nmhost.StateNotReady, nmhost.StateNotReady, reg))), statusWarn, codeBrowserExtNotReady, codeHintBrowserExtOpen,
			map[string]any{"browsers": "Chrome, Edge"}},
		{"not registered before not ready", stPtr(base(browsers(nmhost.StateNotReady, none, not))), statusWarn, codeBrowserExtNotRegistered, codeHintBrowserExtDev,
			map[string]any{"browsers": "Firefox"}},
		{"only blocked", stPtr(base(browsers(nmhost.StateBlocked, none, none))), statusWarn, codeBrowserExtBlocked, codeHintBrowserExtFullDisk, nil},
		{"disk image", func() *nmhost.Status {
			s := base(browsers(reg, none, reg))
			s.LauncherBlocked = nmhost.LauncherDiskImage
			s.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserFirefox, ExtensionVersion: "0.1.0", Protocol: 1, Accepted: true, At: at}
			return &s
		}(), statusWarn, codeBrowserExtAppLocation, codeHintBrowserExtMoveApp, nil},
	}
	for _, c := range cases {
		if c.st == nil {
			e.srv.SetNMHost(nil)
		} else {
			e.srv.SetNMHost(&fakeNM{st: *c.st})
		}
		got := e.srv.checkBrowserExt()
		if got.Name != checkBrowserExt || got.Status != c.status || got.DetailInfo == nil || got.DetailInfo.Code != c.code {
			t.Errorf("%s: %+v %+v", c.name, got, got.DetailInfo)
			continue
		}
		if (got.HintInfo == nil) != (c.hint == "") || (got.HintInfo != nil && got.HintInfo.Code != c.hint) {
			t.Errorf("%s: hint %+v, want %q", c.name, got.HintInfo, c.hint)
		}
		for k, v := range c.params {
			if got.DetailInfo.Params[k] != v {
				t.Errorf("%s: param %s = %v, want %v", c.name, k, got.DetailInfo.Params[k], v)
			}
		}
		if strings.Contains(got.Detail, "{") {
			t.Errorf("%s: unfilled text %q", c.name, got.Detail)
		}
	}
	// the desktop application's hint is "重新登记"; why the last
	// registration failed is the detail
	e.srv.diag.desktop = true
	st := base(browsers(not, none, none))
	st.LastRegister = &nmhost.RegisterResult{Error: "registry write denied"}
	e.srv.SetNMHost(&fakeNM{st: st})
	got := e.srv.checkBrowserExt()
	if got.HintInfo == nil || got.HintInfo.Code != codeHintBrowserExtRegister || got.DetailInfo.Detail != "registry write denied" {
		t.Fatalf("desktop: %+v %+v", got.HintInfo, got.DetailInfo)
	}
	// The hints of the macOS states fill their parameters; the place of an
	// app that cannot be started is the detail.
	st = base(browsers(nmhost.StateNotReady, none, none))
	e.srv.SetNMHost(&fakeNM{st: st})
	got = e.srv.checkBrowserExt()
	if got.HintInfo.Params["browsers"] != "Chrome" || got.Hint != "先打开一次 Chrome，再点击“重新登记”" {
		t.Fatalf("not ready hint: %+v %q", got.HintInfo, got.Hint)
	}
	st = base(browsers(reg, none, none))
	st.LauncherBlocked = nmhost.LauncherTranslocated
	e.srv.SetNMHost(&fakeNM{st: st})
	if got = e.srv.checkBrowserExt(); !strings.Contains(got.DetailInfo.Detail, "App Translocation") {
		t.Fatalf("translocated: %+v", got.DetailInfo)
	}
	// An endpoint another program holds (nm-harden): the row says so, even
	// with every browser registered, and never that the extension works
	// through the other program.
	st = base(browsers(reg, reg, reg))
	st.Listening, st.ListenError = false, nmhost.ListenInUse
	st.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserChrome, ExtensionVersion: "0.1.0", Protocol: 1, Accepted: true, At: at}
	e.srv.SetNMHost(&fakeNM{st: st})
	got = e.srv.checkBrowserExt()
	if got.Status != statusWarn || got.DetailInfo.Code != codeBrowserExtInUse || !strings.HasPrefix(got.Detail, "另一个程序占用了 LectureFlow 的扩展连接") ||
		strings.Contains(got.Detail, "连到那一个") || got.HintInfo == nil || got.HintInfo.Code != codeHintBrowserExtInUse {
		t.Fatalf("in use: %+v %q %+v", got.DetailInfo, got.Detail, got.HintInfo)
	}
}

func stPtr(v nmhost.Status) *nmhost.Status { return &v }

// In GET /api/system aria2c follows ffmpeg and browser_ext comes before the
// providers (where the mock server puts them); "重新登记"
// (POST /api/nmhost/register) shows at once, not after the cache.
func TestBrowserExtRowInSystem(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	nm := &fakeNM{st: nmhost.Status{Managed: true, Listening: true, Browsers: browsers(nmhost.StateNotRegistered,
		nmhost.StateNotInstalled, nmhost.StateNotInstalled), HostPresent: true}}
	e.srv.SetNMHost(nm)
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	names := checkNames(sys.Checks)
	i := slices.Index(names, checkBrowserExt)
	if i < 1 || !strings.HasPrefix(names[i+1], "api_key:") || names[slices.Index(names, "ffmpeg")+1] != checkAria2c {
		t.Fatalf("checks %v", names)
	}
	if c := checksByName(sys)[checkBrowserExt]; c.Status != statusWarn {
		t.Fatalf("before: %+v", c)
	}
	e.call("POST", "/api/nmhost/register", nil, http.StatusOK, nil)
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	if c := checksByName(sys)[checkBrowserExt]; c.Status != statusOK || c.DetailInfo.Code != codeBrowserExtRegistered || nm.registers != 1 {
		t.Fatalf("after registering: %+v (%d registrations)", c, nm.registers)
	}
}
