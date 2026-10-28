// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package nmhost is LectureFlow's side of the browser extension's Native
// Messaging (SPEC-ext "Native Messaging 宿主", .claude/ext-brief.md §4):
//
//   - the local IPC endpoint (a Windows named pipe that only the current
//     user may open, or a Unix socket in a 0700 directory) that the running
//     app listens on and the native host program dials;
//   - Server, which serves internal/bridge frames on that endpoint;
//   - Host, the relay run by cmd/lectureflow-nmhost: browser stdin/stdout
//     ↔ the endpoint, starting the app when it is not running;
//   - Registrar, which registers the host with Chrome, Edge and Firefox
//     (manifests, HKCU keys or the browsers' NativeMessagingHosts
//     directories, launch.json) and reports the registration state.
//
// Neither side writes message contents to a log: requests carry cookies and
// signed URLs (SPEC-ext 凭据处理).
package nmhost

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HostName is the native messaging host name the extension connects to.
const HostName = "org.lectureflow.nmhost"

// Environment variables. Both override for isolated instances and tests;
// the browsers never set them.
const (
	// EnvEndpoint overrides the IPC endpoint for the app and the host: a
	// named pipe (\\.\pipe\NAME) or a Unix socket path. "off" disables it.
	EnvEndpoint = "LECTUREFLOW_NM_ENDPOINT"
	// EnvDir overrides the directory of the manifests and launch.json
	// ({UserConfigDir}/LectureFlow/nmhost).
	EnvDir = "LECTUREFLOW_NM_DIR"
	// EnvDevExtension = 1 lets a release build admit the unpacked
	// development extension (ChromiumDevID) too: for contributors who load
	// extension/dist/chrome into Chrome or Edge next to an installed
	// LectureFlow (docs/install.md "浏览器扩展").
	EnvDevExtension = "LECTUREFLOW_DEV_EXTENSION"
)

// Browsers, in the order they are reported.
const (
	BrowserChrome  = "chrome"
	BrowserEdge    = "edge"
	BrowserFirefox = "firefox"
)

// Browsers lists every browser the host is registered with.
var Browsers = []string{BrowserChrome, BrowserEdge, BrowserFirefox}

// Allowed extension IDs (maintained by the lead, ext-brief §4). Chromium
// browsers name extensions by origin, Firefox by the add-on id.
const (
	// ChromeWebStoreID is the extension's ID in the Chrome Web Store. Edge
	// installs it from the Chrome Web Store as well, under the same ID.
	ChromeWebStoreID = "cogalnelnedilmcbpcjgffppmlgnmhjd"
	// ChromiumDevID is the unpacked development build's ID, derived from
	// the public "key" of extension/manifest.chrome.json. Anyone can put
	// that key into an unpacked extension of their own and get this ID, so
	// only a development build or EnvDevExtension admits it (security
	// review L4).
	ChromiumDevID = "lbfjnfhpnaafingpjkoefobbjnmheehl"
	// FirefoxAddonID is the add-on id (browser_specific_settings), held on
	// addons.mozilla.org: the development build uses it too.
	FirefoxAddonID = "lectureflow@qu1ncy.dev"
)

// FirefoxExtensionIDs are the add-on ids the host admits.
var FirefoxExtensionIDs = []string{FirefoxAddonID}

// ChromiumExtensionIDs are the Chrome / Edge extension IDs the host admits:
// the store's, and the development build's when dev.
func ChromiumExtensionIDs(dev bool) []string {
	ids := []string{ChromeWebStoreID}
	if dev {
		ids = append(ids, ChromiumDevID)
	}
	return ids
}

// Why the development extension is admitted (DevExtension,
// Status.DevExtension).
const (
	// DevExtensionBuild: a development build (no release version set with
	// -ldflags "-X main.version=…").
	DevExtensionBuild = "build"
	// DevExtensionEnv: LECTUREFLOW_DEV_EXTENSION=1 (EnvDevExtension).
	DevExtensionEnv = "env"
)

// DevBuildVersion is the build version of a binary built without
// -ldflags "-X main.version=…" (cmd/lectureflow's default).
const DevBuildVersion = "dev"

// DevExtension reports why the development extension is admitted ("" =
// it is not): buildVersion is the version the build was given with
// -ldflags (cmd/lectureflow's main.version, never -app-version, which any
// caller may pass), DevBuildVersion or "" for a development build;
// otherwise LECTUREFLOW_DEV_EXTENSION set to 1 (or true).
func DevExtension(buildVersion string, getenv func(string) string) string {
	if v := strings.TrimSpace(buildVersion); v == "" || v == DevBuildVersion {
		return DevExtensionBuild
	}
	if getenv != nil {
		if on, err := strconv.ParseBool(strings.TrimSpace(getenv(EnvDevExtension))); err == nil && on {
			return DevExtensionEnv
		}
	}
	return ""
}

// HostExe is the file name of the host program.
func HostExe() string {
	if isWindows {
		return "lectureflow-nmhost.exe"
	}
	return "lectureflow-nmhost"
}

// ConfigDir is where the manifests and launch.json live:
// LECTUREFLOW_NM_DIR, else {UserConfigDir}/LectureFlow/nmhost (not in the
// data directory, which the user may move).
func ConfigDir(getenv func(string) string) (string, error) {
	if d := getenv(EnvDir); d != "" {
		return filepath.Abs(d)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "LectureFlow", "nmhost"), nil
}

// HostCopyDir is where the portable build and macOS copy the host so that
// the manifests point at a stable path: %LOCALAPPDATA%\LectureFlow\nmhost on
// Windows; on macOS ~/Library/Application Support/LectureFlow/nmhost/bin,
// not ~/Library/Caches, which the system and cleaning tools may empty while
// the app is closed (the browser could then not start the host).
func HostCopyDir() (string, error) {
	if !isWindows {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "LectureFlow", "nmhost", "bin"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "LectureFlow", "nmhost"), nil
}
