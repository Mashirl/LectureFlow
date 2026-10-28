// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package nmhost

import (
	"os"
	"path/filepath"
	"runtime"
)

// PlatformOptions fills the system parts of o: on macOS the browsers'
// NativeMessagingHosts directories, a browser counting as installed when
// its application is in /Applications or ~/Applications or its own data
// directory exists (MacInstalled; only stat'ed, App Data protection).
// Other systems are not supported (nothing is registered).
func PlatformOptions(o *RegistrarOptions) {
	if runtime.GOOS != "darwin" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	o.BrowserDirs = MacBrowserDirs(home)
	if o.FS == nil {
		o.FS = OSBrowserFS{}
	}
	o.Installed = MacInstalled(MacBrowserDataDirs(home), o.FS, []string{"/Applications", filepath.Join(home, "Applications")})
}
