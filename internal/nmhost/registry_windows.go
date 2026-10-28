// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// SystemRegistry is HKCU of the current user. Only the app uses it; tests
// use a fake (the default tests never write the real registry).
type SystemRegistry struct{}

func (SystemRegistry) GetDefault(key string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (SystemRegistry) SetDefault(key, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", value)
}

// browserExes are the App Paths names of the browsers.
var browserExes = map[string]string{
	BrowserChrome:  "chrome.exe",
	BrowserEdge:    "msedge.exe",
	BrowserFirefox: "firefox.exe",
}

// BrowserInstalled looks for the browser's App Paths entry (per-user or
// per-machine, both registry views), which every installer writes.
func BrowserInstalled(browser string) bool {
	exe := browserExes[browser]
	if exe == "" {
		return false
	}
	path := `Software\Microsoft\Windows\CurrentVersion\App Paths\` + exe
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		for _, view := range []uint32{0, registry.WOW64_32KEY} {
			k, err := registry.OpenKey(root, path, registry.QUERY_VALUE|view)
			if err == nil {
				k.Close()
				return true
			}
		}
	}
	return false
}

// PlatformOptions fills the system parts of o: HKCU and the browser check.
func PlatformOptions(o *RegistrarOptions) {
	o.Registry = SystemRegistry{}
	o.Installed = BrowserInstalled
}
