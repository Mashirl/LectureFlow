// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/nmhost"
)

// The diagnostics rows of the browser extension (SPEC-ext 主程序配套改动 2
// and 3; NOTES "决定（ext aria2c 随包…）" 4 and "决定（ext Native Messaging
// 宿主…）" 11). Both are read only: nothing is downloaded, registered or
// started.
//
//	aria2c       the bundled aria2c (config.Aria2c): the first line of
//	             aria2c --version and the SHA-256 of the file, computed
//	             with the other checks (startup, "重新检查"). On Windows
//	             the bundled file is compared with the core component of
//	             the built-in manifest (platforms["windows-x64"].files
//	             name "bin/aria2c.exe"): a difference is an error. macOS
//	             builds are not reproducible and are re-signed when packed,
//	             so there the hash is shown only; so is an aria2c that is
//	             not the bundled one (web mode: PATH or config.toml).
//	browser_ext  the Native Messaging registration and the latest
//	             extension connection (nmhost.Status, GET /api/nmhost),
//	             live like disk, before the provider rows (where the
//	             mock server puts it too): each browser's registration, the host
//	             program, the IPC endpoint. A missing registration is a
//	             warning whose hint is check.hint_browser_ext_register (the
//	             page offers "重新登记": POST /api/nmhost/register).
//	             macOS (mac-fix): a browser whose NativeMessagingHosts
//	             directory the system refused (blocked: Full Disk Access),
//	             one that has not created it yet (not_ready: open it once),
//	             and an app that runs from a disk image or a translocated
//	             path (launcher_blocked: move it to Applications) are
//	             warnings with their own hints. An endpoint another
//	             program holds (listen_error in_use: a second LectureFlow,
//	             or one that took the name first) is a warning that the
//	             extension cannot reach this LectureFlow (nm-harden); the
//	             page itself notes when the development extension may
//	             connect (status.dev_extension).

// Names of the rows.
const (
	checkAria2c     = "aria2c"
	checkBrowserExt = "browser_ext"
)

// Codes of the rows (part of the API's catalog; init adds them).
const (
	codeAria2cOK                 = "check.aria2c_ok"
	codeAria2cVerified           = "check.aria2c_verified"
	codeAria2cExternal           = "check.aria2c_external"
	codeAria2cMismatch           = "check.aria2c_mismatch"
	codeAria2cMissing            = "check.aria2c_missing"
	codeHintAria2cDesktop        = "check.hint_aria2c_desktop"
	codeHintAria2cDev            = "check.hint_aria2c_dev"
	codeBrowserExtUnavailable    = "check.browser_ext_unavailable"
	codeBrowserExtOff            = "check.browser_ext_off"
	codeBrowserExtInUse          = "check.browser_ext_in_use"
	codeBrowserExtListenFailed   = "check.browser_ext_listen_failed"
	codeBrowserExtUnmanaged      = "check.browser_ext_unmanaged"
	codeBrowserExtProtocol       = "check.browser_ext_protocol"
	codeBrowserExtNoBrowser      = "check.browser_ext_no_browser"
	codeBrowserExtHostMissing    = "check.browser_ext_host_missing"
	codeBrowserExtNotRegistered  = "check.browser_ext_not_registered"
	codeBrowserExtRegistered     = "check.browser_ext_registered"
	codeBrowserExtConnected      = "check.browser_ext_connected"
	codeBrowserExtBlocked        = "check.browser_ext_blocked"
	codeBrowserExtNotReady       = "check.browser_ext_not_ready"
	codeBrowserExtAppLocation    = "check.browser_ext_app_location"
	codeHintBrowserExtFullDisk   = "check.hint_browser_ext_full_disk"
	codeHintBrowserExtOpen       = "check.hint_browser_ext_open_browser"
	codeHintBrowserExtMoveApp    = "check.hint_browser_ext_move_app"
	codeHintBrowserExtRegister   = "check.hint_browser_ext_register"
	codeHintBrowserExtDev        = "check.hint_browser_ext_dev"
	codeHintBrowserExtRestart    = "check.hint_browser_ext_restart"
	codeHintBrowserExtInUse      = "check.hint_browser_ext_in_use"
	codeHintBrowserExtUpdate     = "check.hint_browser_ext_update"
	browserExtCacheFor           = time.Second
	aria2cManifestFile           = "bin/aria2c.exe"
	aria2cManifestPlatform       = "windows-x64"
	aria2cManifestComponent      = "core"
	browserExtTimeLayout         = "2006-01-02 15:04"
	browserExtUnknownBrowserName = "?"
)

var extCheckCodes = map[string]errDef{
	codeAria2cOK:                {"{version}，SHA-256 {sha256}", []string{"version", "sha256"}},
	codeAria2cVerified:          {"{version}，SHA-256 {sha256}（与安装包的清单一致）", []string{"version", "sha256"}},
	codeAria2cExternal:          {"{version}，SHA-256 {sha256}（不是 LectureFlow 自带的 aria2c：{path}）", []string{"version", "sha256", "path"}},
	codeAria2cMismatch:          {"aria2c 与安装包的清单不一致：SHA-256 {sha256}，应为 {expected}", []string{"sha256", "expected"}},
	codeAria2cMissing:           {"没有找到 aria2c（{path}）。无法从录播平台刮削录像", []string{"path"}},
	codeHintAria2cDesktop:       {"LectureFlow 自带的 aria2c 缺失或被改动，请重新安装 LectureFlow", nil},
	codeHintAria2cDev:           {"安装 aria2 并把 aria2c 加入 PATH，或在 config.toml 中设置 aria2c 的完整路径，然后点击“重新检查”", nil},
	codeBrowserExtUnavailable:   {"浏览器扩展的连接服务没有启动", nil},
	codeBrowserExtOff:           {"浏览器扩展的连接已关闭（启动参数 -nm-endpoint off）", nil},
	codeBrowserExtInUse:         {"另一个程序占用了 LectureFlow 的扩展连接，浏览器扩展无法连接到这个 LectureFlow", nil},
	codeBrowserExtListenFailed:  {"无法建立浏览器扩展的连接", nil},
	codeBrowserExtUnmanaged:     {"这个 LectureFlow 使用单独的连接端点，不登记浏览器", nil},
	codeBrowserExtProtocol:      {"最近一次连接的 {browser} 扩展（{version}）与这个版本的 LectureFlow 不兼容", []string{"browser", "version"}},
	codeBrowserExtNoBrowser:     {"没有找到 Chrome、Edge 或 Firefox", nil},
	codeBrowserExtHostMissing:   {"浏览器登记的扩展宿主程序不存在：{path}", []string{"path"}},
	codeBrowserExtNotRegistered: {"{browsers} 没有登记 LectureFlow，扩展无法连接", []string{"browsers"}},
	codeBrowserExtRegistered:    {"已在 {browsers} 中登记；这次启动后还没有扩展连接过", []string{"browsers"}},
	codeBrowserExtConnected:     {"已在 {browsers} 中登记；最近一次扩展连接：{browser} 扩展 {version}，{at}", []string{"browsers", "browser", "version", "at"}},
	codeHintBrowserExtRegister:  {"点击“重新登记”；仍然不行时重新安装 LectureFlow", nil},
	codeHintBrowserExtDev:       {"开发版没有扩展宿主程序：先构建 lectureflow-nmhost（make.ps1 build），或安装 LectureFlow 桌面版", nil},
	codeHintBrowserExtRestart:   {"重新启动 LectureFlow", nil},
	codeHintBrowserExtInUse:     {"关闭占用它的程序（例如另一个正在运行的 LectureFlow），然后重新启动 LectureFlow；仍然如此时重新启动电脑", nil},
	codeHintBrowserExtUpdate:    {"把浏览器扩展和 LectureFlow 都更新到最新版本", nil},
	codeBrowserExtBlocked:       {"macOS 不允许 LectureFlow 在 {browsers} 中登记，扩展无法连接", []string{"browsers"}},
	codeBrowserExtNotReady:      {"{browsers} 还没有打开过，LectureFlow 无法在其中登记", []string{"browsers"}},
	codeBrowserExtAppLocation:   {"LectureFlow 正从磁盘映像或临时位置运行，关闭后浏览器扩展无法自动打开它", nil},
	codeHintBrowserExtFullDisk:  {"在“系统设置 › 隐私与安全性 › 完全磁盘访问权限”里允许 LectureFlow，然后点击“重新登记”", nil},
	codeHintBrowserExtOpen:      {"先打开一次 {browsers}，再点击“重新登记”", []string{"browsers"}},
	codeHintBrowserExtMoveApp:   {"请把 LectureFlow 移到“应用程序”文件夹后再打开", nil},
}

func init() {
	for code, def := range extCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// ---- aria2c ----

// aria2cGOOS is the system whose rule the aria2c check follows (tests).
var aria2cGOOS = runtime.GOOS

// checkAria2c is the aria2c row.
func (s *Server) checkAria2c(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkAria2c, Status: statusOK}
	exe := s.cfg.Aria2c
	if exe == "" {
		exe = "aria2c"
	}
	path := exe
	if !strings.ContainsAny(exe, `/\`) {
		p, err := exec.LookPath(exe)
		if err != nil {
			path = ""
		} else {
			path = p
		}
	}
	if fi, err := os.Stat(path); path == "" || err != nil || !fi.Mode().IsRegular() {
		c.Status = statusWarn
		if s.desktopDiag() {
			c.Status = statusError // the installation lacks its own aria2c
		}
		c.setDetail(codeAria2cMissing, map[string]any{"path": exe})
		c.setHint(s.devHint(codeHintAria2cDev, codeHintAria2cDesktop), nil)
		return c
	}
	out, _, err := s.cmd(ctx, cmdTimeout, path, "--version")
	if err != nil {
		c.Status = statusError
		c.setDetail("check.run_failed", map[string]any{"program": path, "error": err.Error()})
		c.setHint(s.devHint(codeHintAria2cDev, codeHintAria2cDesktop), nil)
		return c
	}
	version := firstLine(out)
	sum, err := fileSHA256(path)
	if err != nil {
		c.Status = statusError
		c.setDetail("check.run_failed", map[string]any{"program": path, "error": err.Error()})
		return c
	}
	params := map[string]any{"version": version, "sha256": sum}
	if !s.bundledAria2c(path) {
		params["path"] = path
		c.setDetail(codeAria2cExternal, params)
		return c
	}
	if aria2cGOOS != "windows" {
		c.setDetail(codeAria2cOK, params) // macOS: shown only
		return c
	}
	want := s.manifestAria2cSHA256()
	switch {
	case want == "":
		c.setDetail(codeAria2cOK, params) // no record to compare with
	case !strings.EqualFold(want, sum):
		c.Status = statusError
		c.setDetail(codeAria2cMismatch, map[string]any{"sha256": sum, "expected": strings.ToLower(want)})
		c.setHint(codeHintAria2cDesktop, nil)
	default:
		c.setDetail(codeAria2cVerified, params)
	}
	return c
}

// bundledAria2c: path is resources/bin/aria2c(.exe) of the desktop
// application's resources.
func (s *Server) bundledAria2c(path string) bool {
	if s.cfg.ResourcesDir == "" {
		return false
	}
	name := "aria2c"
	if aria2cGOOS == "windows" {
		name += ".exe"
	}
	want, err1 := filepath.Abs(filepath.Join(s.cfg.ResourcesDir, "bin", name))
	got, err2 := filepath.Abs(path)
	return err1 == nil && err2 == nil && samePathOS(want, got)
}

// manifestAria2cSHA256 is the SHA-256 the built-in manifest records for
// the bundled aria2c on Windows ("" = none or unreadable).
func (s *Server) manifestAria2cSHA256() string {
	if s.cfg.ComponentsManifest == "" {
		return ""
	}
	m, err := components.LoadManifest(s.cfg.ComponentsManifest)
	if err != nil {
		return ""
	}
	for _, comp := range m.Components {
		if comp.ID != aria2cManifestComponent {
			continue
		}
		if b := comp.Platforms[aria2cManifestPlatform]; b != nil {
			for _, f := range b.Files {
				if f.Name == aria2cManifestFile {
					return f.SHA256
				}
			}
		}
	}
	return ""
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- browser_ext ----

// browserExtCache keeps the row for browserExtCacheFor: GET /api/system is
// polled, the status reads the registry and a few files.
type browserExtCache struct {
	mu  sync.Mutex
	at  time.Time
	row SystemCheck
}

var browserExtRows sync.Map // *Server → *browserExtCache

// browserExtCheck is the browser_ext row (cached for a second).
func (s *Server) browserExtCheck() SystemCheck {
	v, _ := browserExtRows.LoadOrStore(s, &browserExtCache{})
	c := v.(*browserExtCache)
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < browserExtCacheFor && c.row.Name != "" {
		return c.row
	}
	c.row, c.at = s.checkBrowserExt(), time.Now()
	return c.row
}

// forgetBrowserExtRow drops the cached row (after "重新登记").
func (s *Server) forgetBrowserExtRow() {
	if v, ok := browserExtRows.Load(s); ok {
		c := v.(*browserExtCache)
		c.mu.Lock()
		c.at = time.Time{}
		c.mu.Unlock()
	}
}

func browserName(b string) string {
	switch b {
	case nmhost.BrowserChrome:
		return "Chrome"
	case nmhost.BrowserEdge:
		return "Edge"
	case nmhost.BrowserFirefox:
		return "Firefox"
	case "":
		return browserExtUnknownBrowserName
	}
	return b
}

func browserNames(bs []nmhost.BrowserStatus) string {
	names := make([]string, 0, len(bs))
	for _, b := range bs {
		names = append(names, browserName(b.Browser))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// launcherPlace names nmhost.Status.LauncherBlocked for "查看详情".
var launcherPlace = map[string]string{
	nmhost.LauncherTranslocated: "a translocated path (App Translocation)",
	nmhost.LauncherDiskImage:    "a read-only disk image",
}

// checkBrowserExt builds the browser_ext row from nmhost.Status.
func (s *Server) checkBrowserExt() SystemCheck {
	c := SystemCheck{Name: checkBrowserExt, Status: statusOK}
	n := s.nmHost()
	if n == nil {
		c.Status = statusWarn
		c.setDetail(codeBrowserExtUnavailable, nil)
		c.setHint(codeHintBrowserExtRestart, nil)
		return c
	}
	st := n.Status()
	if !st.Listening {
		switch st.ListenError {
		case nmhost.ListenOff:
			c.setDetail(codeBrowserExtOff, nil)
		case nmhost.ListenInUse:
			c.Status = statusWarn
			c.setDetail(codeBrowserExtInUse, nil)
			c.setHint(codeHintBrowserExtInUse, nil)
		default:
			c.Status = statusWarn
			c.setDetail(codeBrowserExtListenFailed, nil)
			c.setHint(codeHintBrowserExtRestart, nil)
		}
		return c
	}
	if !st.Managed {
		c.setDetail(codeBrowserExtUnmanaged, nil)
		return c
	}
	last := st.LastConnection
	if last != nil && !last.Accepted {
		c.Status = statusWarn
		c.setDetail(codeBrowserExtProtocol, map[string]any{"browser": browserName(last.Browser), "version": last.ExtensionVersion})
		c.setHint(codeHintBrowserExtUpdate, nil)
		return c
	}
	var installed, bad, missing, blocked, notReady []nmhost.BrowserStatus
	for _, b := range st.Browsers {
		switch b.State {
		case nmhost.StateNotInstalled:
			continue
		case nmhost.StateHostMissing:
			missing = append(missing, b)
		case nmhost.StateNotRegistered:
			bad = append(bad, b)
		case nmhost.StateBlocked:
			blocked = append(blocked, b)
		case nmhost.StateNotReady:
			notReady = append(notReady, b)
		}
		installed = append(installed, b)
	}
	if len(installed) == 0 {
		c.setDetail(codeBrowserExtNoBrowser, nil)
		return c
	}
	registerHint := s.devHint(codeHintBrowserExtDev, codeHintBrowserExtRegister)
	regErr := ""
	if st.LastRegister != nil {
		regErr = st.LastRegister.Error
	}
	switch {
	case len(blocked) > 0: // registering again does not help before the user allows it
		c.Status = statusWarn
		c.setDetail(codeBrowserExtBlocked, map[string]any{"browsers": browserNames(blocked)})
		c.setHint(codeHintBrowserExtFullDisk, nil)
	case len(missing) > 0 || (st.Host != "" && !st.HostPresent):
		c.Status = statusWarn
		c.setDetail(codeBrowserExtHostMissing, map[string]any{"path": st.Host})
		c.setHint(registerHint, nil)
	case len(bad) > 0:
		c.Status = statusWarn
		c.setDetail(codeBrowserExtNotRegistered, map[string]any{"browsers": browserNames(bad)})
		c.setHint(registerHint, nil)
	case len(notReady) > 0:
		names := browserNames(notReady)
		c.Status = statusWarn
		c.setDetail(codeBrowserExtNotReady, map[string]any{"browsers": names})
		c.setHint(codeHintBrowserExtOpen, map[string]any{"browsers": names})
	case st.LauncherBlocked != "":
		c.Status = statusWarn
		c.setDetail(codeBrowserExtAppLocation, nil)
		c.setHint(codeHintBrowserExtMoveApp, nil)
		c.DetailInfo.Detail = "launch.json not written: the app runs from " + launcherPlace[st.LauncherBlocked]
	case last != nil:
		c.setDetail(codeBrowserExtConnected, map[string]any{"browsers": browserNames(installed),
			"browser": browserName(last.Browser), "version": last.ExtensionVersion, "at": last.At.Local().Format(browserExtTimeLayout)})
	default:
		c.setDetail(codeBrowserExtRegistered, map[string]any{"browsers": browserNames(installed)})
	}
	if c.Status != statusOK && regErr != "" && c.DetailInfo != nil {
		c.DetailInfo.Detail = regErr // "查看详情": why the last registration failed
	}
	return c
}

// insertBeforeProviders returns rows with row inserted before the first
// provider row (api_key:…, api:…, role_model:…) or disk, the end of the
// local rows.
func insertBeforeProviders(rows []SystemCheck, row SystemCheck) []SystemCheck {
	at := len(rows)
	for i, r := range rows {
		if strings.HasPrefix(r.Name, "api_key:") || strings.HasPrefix(r.Name, "api:") ||
			strings.HasPrefix(r.Name, "role_model:") || r.Name == "disk" {
			at = i
			break
		}
	}
	out := make([]SystemCheck, 0, len(rows)+1)
	out = append(out, rows[:at]...)
	out = append(out, row)
	return append(out, rows[at:]...)
}
