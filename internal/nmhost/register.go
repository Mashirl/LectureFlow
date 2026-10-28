// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Registration (ext-brief §4): the app runs Register at every start (dmg
// and the portable build have no installer) and from the diagnostics
// page's "重新登记"; it is idempotent and writes only what changed.
//
//   - Two manifests in Dir: org.lectureflow.nmhost.chromium.json
//     (allowed_origins) and org.lectureflow.nmhost.firefox.json
//     (allowed_extensions), whose "path" is the host program.
//   - Windows: HKCU\Software\{Google\Chrome,Microsoft\Edge,Mozilla}\
//     NativeMessagingHosts\org.lectureflow.nmhost, default value = the
//     manifest's path.
//   - macOS: the manifest as org.lectureflow.nmhost.json in each installed
//     browser's NativeMessagingHosts directory (MacInstalled). macOS 27's
//     App Data protection covers the Chrome, Edge and Firefox data
//     directories as a whole (creating or reading anything there asks the
//     user or fails with EPERM), but not the NativeMessagingHosts directory
//     Chrome and Edge create at their first start: writing there needs no
//     permission. So nothing outside NativeMessagingHosts is touched (the
//     browsers' own directories are only stat'ed): Chrome's and Edge's
//     directory is never created (missing, the browser is StateNotReady:
//     "open it once, then register again"); Firefox's
//     ~/Library/Application Support/Mozilla/NativeMessagingHosts belongs to
//     no application and is created. A permission error all the same (EPERM
//     or EACCES) is StateBlocked, with Full Disk Access as the way out.
//     A browser that creates its directory while the app runs (opened for
//     the first time) is registered by itself (AutoRegister: the service
//     looks every DefaultWatchEvery, Status as well; at most once per
//     browser every AutoRegisterEvery).
//   - The Chromium manifest admits the Chrome Web Store ID only, plus the
//     unpacked development build's ID in a development build or with
//     LECTUREFLOW_DEV_EXTENSION=1 (DevExtension; security review L4).
//   - launch.json in Dir when the desktop app passed its launcher, unless
//     the app runs from a place that will disappear (LauncherBlocked:
//     App Translocation or the disk image it was opened from).
//   - The portable build and macOS first copy the host to a stable place
//     (the portable build unpacks to a new temporary directory each run; a
//     macOS app may be moved or translocated).

// Registry is the part of HKCU the registration uses (tests: a fake).
type Registry interface {
	// GetDefault reads the default value of HKCU\key; ok is false when the
	// key or the value is missing.
	GetDefault(key string) (value string, ok bool, err error)
	SetDefault(key, value string) error
}

// RegistryKey is the HKCU key of a browser's registration.
func RegistryKey(browser string) string {
	switch browser {
	case BrowserChrome:
		return `Software\Google\Chrome\NativeMessagingHosts\` + HostName
	case BrowserEdge:
		return `Software\Microsoft\Edge\NativeMessagingHosts\` + HostName
	case BrowserFirefox:
		return `Software\Mozilla\NativeMessagingHosts\` + HostName
	}
	return ""
}

// MacBrowserDirs are the browsers' NativeMessagingHosts directories under
// home (macOS), the only place of a browser the registration reads or
// writes.
func MacBrowserDirs(home string) map[string]string {
	as := filepath.Join(home, "Library", "Application Support")
	return map[string]string{
		BrowserChrome:  filepath.Join(as, "Google", "Chrome", "NativeMessagingHosts"),
		BrowserEdge:    filepath.Join(as, "Microsoft Edge", "NativeMessagingHosts"),
		BrowserFirefox: filepath.Join(as, "Mozilla", "NativeMessagingHosts"),
	}
}

// MacBrowserDataDirs are the browsers' own data directories under home
// (macOS), guarded by App Data protection: MacInstalled only stats them,
// never looks inside.
func MacBrowserDataDirs(home string) map[string]string {
	as := filepath.Join(home, "Library", "Application Support")
	return map[string]string{
		BrowserChrome:  filepath.Join(as, "Google", "Chrome"),
		BrowserEdge:    filepath.Join(as, "Microsoft Edge"),
		BrowserFirefox: filepath.Join(as, "Firefox"),
	}
}

// macBrowserApps are the browsers' application bundles (MacInstalled).
var macBrowserApps = map[string]string{
	BrowserChrome:  "Google Chrome.app",
	BrowserEdge:    "Microsoft Edge.app",
	BrowserFirefox: "Firefox.app",
}

// macCreatesDir: the browsers whose NativeMessagingHosts directory the
// registration may create (macOS). Firefox's is under
// ~/Library/Application Support/Mozilla, which belongs to no application
// (Firefox itself never creates it); Chrome and Edge create theirs at their
// first start, inside their protected data directory.
var macCreatesDir = map[string]bool{BrowserFirefox: true}

// BrowserFS is how the registration reaches the browsers' NativeMessagingHosts
// directories (macOS). Tests inject the permission errors macOS answers
// without Full Disk Access.
type BrowserFS interface {
	Stat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	// WriteFile writes b through a temporary file and a rename unless the
	// file holds b already (writeIfChanged).
	WriteFile(name string, b []byte, perm fs.FileMode) error
}

// OSBrowserFS is the real file system.
type OSBrowserFS struct{}

func (OSBrowserFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
func (OSBrowserFS) ReadFile(name string) ([]byte, error)  { return os.ReadFile(name) }
func (OSBrowserFS) WriteFile(name string, b []byte, perm fs.FileMode) error {
	_, err := writeIfChanged(name, b, perm)
	return err
}

// IsBlocked reports whether err is the system refusing access (EPERM or
// EACCES; on macOS 27 the answer of App Data protection to a process that
// may not ask the user).
func IsBlocked(err error) bool { return err != nil && errors.Is(err, fs.ErrPermission) }

// MacInstalled is the macOS "installed" test: the browser's application
// bundle is in one of appDirs (/Applications, ~/Applications), or its own
// data directory (dataDirs, MacBrowserDataDirs) exists. Only stat: nothing
// inside a data directory is read (App Data protection).
func MacInstalled(dataDirs map[string]string, fsys BrowserFS, appDirs []string) func(string) bool {
	return func(b string) bool {
		if app := macBrowserApps[b]; app != "" {
			for _, a := range appDirs {
				if fi, err := fsys.Stat(filepath.Join(a, app)); err == nil && fi.IsDir() {
					return true
				}
			}
		}
		if d := dataDirs[b]; d != "" {
			if fi, err := fsys.Stat(d); err == nil && fi.IsDir() {
				return true
			}
		}
		return false
	}
}

// Why LectureFlow cannot be started from where it runs (macOS): the
// values of RegistrarOptions.LauncherBlocked and Status.LauncherBlocked.
const (
	// LauncherTranslocated: Gatekeeper's App Translocation runs the app
	// from a random read-only path that disappears.
	LauncherTranslocated = "translocated"
	// LauncherDiskImage: the app runs from a read-only volume under
	// /Volumes, the disk image it was opened from.
	LauncherDiskImage = "dmg"
)

// ValidLauncherBlocked reports whether v is one of the Launcher* reasons.
func ValidLauncherBlocked(v string) bool {
	return v == LauncherTranslocated || v == LauncherDiskImage
}

// UnstableLauncher returns why exe, the desktop app's launcher, cannot be
// written to launch.json on goos ("" = it can): on macOS a translocated
// app (…/AppTranslocation/…) and an app on a read-only volume under
// /Volumes (readOnly; the disk image) have paths that are gone once the
// app quits or the image is ejected.
func UnstableLauncher(goos, exe string, readOnly func(dir string) bool) string {
	if goos != "darwin" || exe == "" {
		return ""
	}
	p := filepath.ToSlash(exe)
	if strings.Contains(p, "/AppTranslocation/") {
		return LauncherTranslocated
	}
	if rest, ok := strings.CutPrefix(p, "/Volumes/"); ok && readOnly != nil {
		name, _, _ := strings.Cut(rest, "/")
		if name != "" && readOnly("/Volumes/"+name) {
			return LauncherDiskImage
		}
	}
	return ""
}

// Manifest file names in Dir.
const (
	ChromiumManifest = HostName + ".chromium.json"
	FirefoxManifest  = HostName + ".firefox.json"
)

// manifest is a native messaging host manifest.
type manifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// MarshalJSON writes exactly one of allowed_extensions (Firefox: when
// AllowedExtensions is set) and allowed_origins (Chromium), even when the
// list is empty: Chrome refuses a manifest without allowed_origins.
func (m manifest) MarshalJSON() ([]byte, error) {
	type common struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Path        string `json:"path"`
		Type        string `json:"type"`
	}
	c := common{Name: m.Name, Description: m.Description, Path: m.Path, Type: m.Type}
	if m.AllowedExtensions != nil {
		return json.Marshal(struct {
			common
			AllowedExtensions []string `json:"allowed_extensions"`
		}{c, m.AllowedExtensions})
	}
	return json.Marshal(struct {
		common
		AllowedOrigins []string `json:"allowed_origins"`
	}{c, nonNil(m.AllowedOrigins)})
}

// RegistrarOptions configure a Registrar.
type RegistrarOptions struct {
	// Dir holds the manifests and launch.json (ConfigDir).
	Dir string
	// HostSource is the host program shipped with the app
	// ({resources}/bin/lectureflow-nmhost(.exe)).
	HostSource string
	// CopyHost: copy HostSource into CopyDir and register the copy
	// (-portable, and always on macOS).
	CopyHost bool
	CopyDir  string
	// Launcher is the program that starts the desktop app (-launcher);
	// "" (web mode) leaves launch.json as it is.
	Launcher string
	// LauncherBlocked is why the desktop app has no Launcher although it
	// is the desktop app (LauncherTranslocated, LauncherDiskImage; macOS):
	// launch.json is left as it is and the status says so.
	LauncherBlocked string
	// Registry is HKCU (Windows); nil elsewhere.
	Registry Registry
	// BrowserDirs are the browsers' NativeMessagingHosts directories
	// (macOS, MacBrowserDirs); nil on Windows.
	BrowserDirs map[string]string
	// FS reaches BrowserDirs; nil = OSBrowserFS.
	FS BrowserFS
	// Installed reports whether a browser is installed; nil = unknown
	// (never "not installed").
	Installed func(browser string) bool
	// DevExtension is why the development extension is admitted
	// (DevExtension: DevExtensionBuild, DevExtensionEnv; "" = only the
	// store's). It picks the default ChromiumIDs and is reported by Status.
	DevExtension string
	// Allowed extension IDs; nil = the package's
	// ChromiumExtensionIDs(DevExtension != "") / FirefoxExtensionIDs.
	ChromiumIDs []string
	FirefoxIDs  []string
	Now         func() time.Time
}

// Registrar performs and reports the registration.
type Registrar struct {
	o  RegistrarOptions
	mu sync.Mutex
	// last is the outcome of the latest Register.
	last *RegisterResult
	// autoAt: when the registration last ran by itself for a browser
	// (AutoRegister).
	autoAt map[string]time.Time
}

// RegisterResult is the outcome of one Register.
type RegisterResult struct {
	At time.Time `json:"at"`
	// Error is a short English reason when the registration failed as a
	// whole (e.g. the host program is missing); "" on success.
	Error string `json:"error,omitempty"`
	// Notes are non-fatal problems (a browser whose key could not be
	// written, a host copy that is in use and kept).
	Notes []string `json:"notes,omitempty"`
	// Blocked are the browsers (Browser*) whose NativeMessagingHosts
	// directory the system refused (IsBlocked; macOS). Each also has a
	// note.
	Blocked []string `json:"blocked,omitempty"`
	// NotReady are the installed browsers that have no
	// NativeMessagingHosts directory yet and must create it themselves
	// (Chrome, Edge on macOS: open the browser once, register again).
	NotReady []string `json:"not_ready,omitempty"`
}

// NewRegistrar builds a registrar.
func NewRegistrar(o RegistrarOptions) *Registrar {
	if o.DevExtension != DevExtensionBuild && o.DevExtension != DevExtensionEnv {
		o.DevExtension = ""
	}
	if o.ChromiumIDs == nil {
		o.ChromiumIDs = ChromiumExtensionIDs(o.DevExtension != "")
	}
	if o.FirefoxIDs == nil {
		o.FirefoxIDs = FirefoxExtensionIDs
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FS == nil {
		o.FS = OSBrowserFS{}
	}
	if !ValidLauncherBlocked(o.LauncherBlocked) {
		o.LauncherBlocked = ""
	}
	if o.LauncherBlocked != "" {
		o.Launcher = ""
	}
	return &Registrar{o: o}
}

// watches: the registration writes into the browsers' own directories
// (macOS), where a browser may appear while the app runs (AutoRegister).
func (r *Registrar) watches() bool { return r.o.BrowserDirs != nil }

// installed: the browser is installed, or nothing says it is not.
func (r *Registrar) installed(b string) bool { return r.o.Installed == nil || r.o.Installed(b) }

// ErrHostMissing: the app has no host program to register (a development
// build without it). Nothing is written.
var ErrHostMissing = errors.New("nmhost: the host program is missing")

// Register writes the registration. It returns ErrHostMissing (and writes
// nothing) without a host program; per-browser failures are notes, and an
// error is returned when no browser could be registered at all.
func (r *Registrar) Register() (RegisterResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := RegisterResult{At: r.o.Now()}
	err := r.register(&res)
	if err != nil {
		res.Error = err.Error()
	}
	r.last = &res
	return res, err
}

func (r *Registrar) register(res *RegisterResult) error {
	if r.o.Dir == "" {
		return errors.New("nmhost: no registration directory")
	}
	if !isFile(r.o.HostSource) {
		return ErrHostMissing
	}
	host := r.o.HostSource
	if r.o.CopyHost {
		dst, note, err := copyHost(r.o.HostSource, r.o.CopyDir)
		if err != nil {
			return fmt.Errorf("nmhost: copy the host program: %w", err)
		}
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		host = dst
	}
	if abs, err := filepath.Abs(host); err == nil {
		host = abs
	}
	chromium := manifest{Name: HostName, Description: "LectureFlow", Path: host, Type: "stdio", AllowedOrigins: origins(r.o.ChromiumIDs)}
	firefox := manifest{Name: HostName, Description: "LectureFlow", Path: host, Type: "stdio", AllowedExtensions: nonNil(r.o.FirefoxIDs)}
	files := map[string]manifest{ChromiumManifest: chromium, FirefoxManifest: firefox}
	for name, m := range files {
		if _, err := writeIfChanged(filepath.Join(r.o.Dir, name), marshalFile(m), 0o644); err != nil {
			return fmt.Errorf("nmhost: write %s: %w", name, err)
		}
	}
	if r.o.Launcher != "" {
		lc := LaunchConfig{Exe: r.o.Launcher, Args: []string{BackgroundArg}}
		if _, err := writeIfChanged(filepath.Join(r.o.Dir, LaunchFile), marshalFile(lc), 0o644); err != nil {
			return fmt.Errorf("nmhost: write %s: %w", LaunchFile, err)
		}
	}
	registered, failed := 0, 0
	for _, b := range Browsers {
		mname := ChromiumManifest
		m := chromium
		if b == BrowserFirefox {
			mname, m = FirefoxManifest, firefox
		}
		var err error
		switch {
		case r.o.Registry != nil:
			want := filepath.Join(r.o.Dir, mname)
			cur, ok, gerr := r.o.Registry.GetDefault(RegistryKey(b))
			if gerr == nil && ok && samePath(cur, want) {
				registered++
				continue
			}
			err = r.o.Registry.SetDefault(RegistryKey(b), want)
		case r.o.BrowserDirs != nil:
			dir := r.o.BrowserDirs[b]
			if dir == "" || !r.installed(b) {
				continue
			}
			fi, serr := r.o.FS.Stat(dir)
			switch {
			case serr == nil && fi.IsDir(), errors.Is(serr, fs.ErrNotExist) && macCreatesDir[b]:
				err = r.o.FS.WriteFile(filepath.Join(dir, HostName+".json"), marshalFile(m), 0o644)
			case errors.Is(serr, fs.ErrNotExist):
				// Chrome / Edge create it at their first start; creating
				// it here would reach into their protected directory.
				res.NotReady = append(res.NotReady, b)
				continue
			case serr == nil:
				err = fmt.Errorf("%s is not a directory", dir)
			default:
				err = serr
			}
			if IsBlocked(err) {
				res.Blocked = append(res.Blocked, b)
			}
		default:
			continue
		}
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: %v", b, err))
			failed++
			continue
		}
		registered++
	}
	if registered == 0 && failed > 0 {
		if len(res.Blocked) == failed {
			return errors.New("nmhost: the system denied access to the browsers' directories (macOS: allow LectureFlow under Privacy & Security › Full Disk Access)")
		}
		return errors.New("nmhost: no browser could be registered")
	}
	return nil
}

// Browser states of BrowserStatus.State.
const (
	StateRegistered    = "registered"
	StateNotRegistered = "not_registered"
	StateHostMissing   = "host_missing"
	StateNotInstalled  = "not_installed"
	// StateBlocked: not registered because the system refuses access to
	// the browser's NativeMessagingHosts directory (macOS: reading it now,
	// or writing it at the latest Register); Full Disk Access lifts it.
	StateBlocked = "blocked"
	// StateNotReady: the browser is installed but has not created its
	// NativeMessagingHosts directory yet (Chrome, Edge on macOS, before
	// their first start): open it once, then register again.
	StateNotReady = "not_ready"
)

// BrowserStatus is the registration state of one browser.
type BrowserStatus struct {
	Browser string `json:"browser"`
	State   string `json:"state"`
	// Manifest is the manifest the browser would read ("" when none).
	Manifest string `json:"manifest,omitempty"`
}

// Status is what the diagnostics page shows (GET /api/nmhost).
type Status struct {
	// Managed is false when this instance does not manage the registration
	// (an isolated instance on its own endpoint): Browsers is then empty
	// and "重新登记" is not offered.
	Managed  bool            `json:"managed"`
	Browsers []BrowserStatus `json:"browsers"`
	// Host is the host program the registration points to; HostPresent
	// whether it exists.
	Host        string `json:"host"`
	HostPresent bool   `json:"host_present"`
	// AutoStart: launch.json exists, so the host can start the app.
	AutoStart bool `json:"auto_start"`
	// LauncherBlocked: this desktop app runs from a place the host could
	// not start it from (LauncherTranslocated, LauncherDiskImage; macOS),
	// so it did not write launch.json: move it to Applications.
	LauncherBlocked string `json:"launcher_blocked,omitempty"`
	// DevExtension: the registration admits the unpacked development
	// extension as well (DevExtensionBuild: a development build;
	// DevExtensionEnv: LECTUREFLOW_DEV_EXTENSION=1); "" = only the store's.
	DevExtension string `json:"dev_extension,omitempty"`
	// Listening: this app serves the IPC endpoint; ListenError is
	// "off", "in_use" or "failed" otherwise. "in_use": another program
	// holds the endpoint (a second LectureFlow, or a squatter), so the
	// extension cannot reach this LectureFlow.
	Listening   bool   `json:"listening"`
	ListenError string `json:"listen_error,omitempty"`
	// LastConnection is the latest hello since the app started.
	LastConnection *HelloInfo `json:"last_connection"`
	// LastRegister is the outcome of the latest registration run.
	LastRegister *RegisterResult `json:"last_register"`
}

// AutoRegisterEvery is how often at most the registration runs again by
// itself for one browser (macOS, AutoRegister).
const AutoRegisterEvery = 5 * time.Minute

// Status reads the registration as the browsers would see it. On macOS a
// browser that has created its NativeMessagingHosts directory since the
// latest registration (Chrome or Edge opened for the first time while
// LectureFlow runs) is registered first (AutoRegister).
func (r *Registrar) Status() Status {
	st, due := r.status()
	if due {
		r.Register() // the outcome is in LastRegister
		st, _ = r.status()
	}
	return st
}

// AutoRegister runs the registration again when a browser's
// NativeMessagingHosts directory exists without our manifest (macOS: the
// browser was opened for the first time, or something removed the
// manifest), at most once per browser every AutoRegisterEvery. It reports
// whether it registered. The service calls it periodically; Status does
// too.
func (r *Registrar) AutoRegister() bool {
	if _, due := r.status(); !due {
		return false
	}
	r.Register()
	return true
}

// autoDue: browser b's directory exists, our manifest in it does not, and
// the latest automatic registration for b is AutoRegisterEvery ago (it is
// stamped now). r.mu is held.
func (r *Registrar) autoDue(b string) bool {
	if r.o.Dir == "" || !isFile(r.o.HostSource) {
		return false // registering cannot help
	}
	now := r.o.Now()
	if at, ok := r.autoAt[b]; ok && now.Sub(at) < AutoRegisterEvery && now.Sub(at) >= 0 {
		return false
	}
	if r.autoAt == nil {
		r.autoAt = map[string]time.Time{}
	}
	r.autoAt[b] = now
	return true
}

// status is Status without the automatic registration; due reports that
// one is due (autoDue).
func (r *Registrar) status() (st Status, due bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st = Status{Browsers: make([]BrowserStatus, 0, len(Browsers)), LauncherBlocked: r.o.LauncherBlocked, DevExtension: r.o.DevExtension}
	if r.last != nil {
		last := *r.last
		last.Notes = append([]string(nil), last.Notes...)
		last.Blocked = append([]string(nil), last.Blocked...)
		last.NotReady = append([]string(nil), last.NotReady...)
		st.LastRegister = &last
	}
	if r.o.Dir != "" {
		_, err := ReadLaunch(r.o.Dir)
		st.AutoStart = err == nil
		if m, err := readManifest(os.ReadFile, filepath.Join(r.o.Dir, ChromiumManifest)); err == nil {
			st.Host = m.Path
			st.HostPresent = isFile(m.Path)
		}
	}
	for _, b := range Browsers {
		bs := BrowserStatus{Browser: b, State: StateNotRegistered}
		var path string
		read := os.ReadFile
		blocked, notReady, missing := false, false, false
		switch {
		case r.o.Registry != nil:
			if v, ok, err := r.o.Registry.GetDefault(RegistryKey(b)); err == nil && ok && v != "" {
				path = v
			}
		case r.o.BrowserDirs != nil:
			read = r.o.FS.ReadFile
			if dir := r.o.BrowserDirs[b]; dir != "" {
				// Only NativeMessagingHosts and our manifest in it.
				if _, err := r.o.FS.Stat(dir); errors.Is(err, fs.ErrNotExist) {
					notReady = !macCreatesDir[b]
				} else if IsBlocked(err) {
					blocked = true
				} else {
					p := filepath.Join(dir, HostName+".json")
					fi, err := r.o.FS.Stat(p)
					switch {
					case err == nil && fi.Mode().IsRegular():
						path = p
					case IsBlocked(err):
						blocked = true
					case errors.Is(err, fs.ErrNotExist):
						missing = true
					}
				}
			}
			// What the latest Register could not write stays blocked
			// until a Register gets through (the directory may be
			// readable but not writable).
			blocked = blocked || (r.last != nil && slices.Contains(r.last.Blocked, b))
			// The directory is there and our manifest is not: the browser
			// has created it since (opened for the first time), register.
			if missing && !blocked && r.installed(b) && r.autoDue(b) {
				due = true
			}
		}
		if path != "" {
			bs.Manifest = path
			m, err := readManifest(read, path)
			switch {
			case err == nil && m.Name == HostName && r.allows(b, m):
				bs.State = StateRegistered
				if !isFile(m.Path) {
					bs.State = StateHostMissing
				}
			case r.o.BrowserDirs != nil && IsBlocked(err):
				blocked = true
			}
		}
		switch {
		case bs.State != StateNotRegistered:
		case blocked:
			bs.State = StateBlocked
		case notReady:
			bs.State = StateNotReady
		}
		if !r.installed(b) {
			bs.State = StateNotInstalled
		}
		st.Browsers = append(st.Browsers, bs)
	}
	return st, due
}

// allows: the manifest admits exactly our extensions. Another program's
// manifest under our name, one from an older version without the current
// IDs, and one that admits more (the development ID written by a
// development build, read by a release build) count as not registered.
func (r *Registrar) allows(browser string, m manifest) bool {
	if browser == BrowserFirefox {
		return sameSet(m.AllowedExtensions, r.o.FirefoxIDs)
	}
	return sameSet(m.AllowedOrigins, origins(r.o.ChromiumIDs))
}

func sameSet(have, want []string) bool {
	return containsAll(have, want) && containsAll(want, have)
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func origins(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "chrome-extension://"+id+"/")
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func readManifest(read func(string) ([]byte, error), path string) (manifest, error) {
	var m manifest
	b, err := read(path)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// copyHost copies src to dir unless an identical copy is there; the copy
// goes through a temporary file and a rename. A copy that differs but is
// in use (a host is running from it) is kept: it speaks the same protocol,
// and the next start replaces it.
func copyHost(src, dir string) (dst, note string, err error) {
	if dir == "" {
		return "", "", errors.New("no directory for the copy")
	}
	dst = filepath.Join(dir, filepath.Base(src))
	if same, err := sameContent(src, dst); err == nil && same {
		return dst, "", nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", "", err
	}
	if _, err := writeIfChanged(dst, b, 0o755); err != nil {
		if isFile(dst) {
			return dst, "the host copy is in use and was kept: " + err.Error(), nil
		}
		return "", "", err
	}
	return dst, "", nil
}

func sameContent(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() {
		return false, nil
	}
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

func fileHash(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func isFile(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func samePath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if isWindows {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}
