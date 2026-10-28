// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// Regression tests of what the fuzz tests found (NOTES "fuzz").

// A version names updates/<version>/: SemVer's identifiers only, so no
// "/", "\" or ":" can take it elsewhere.
func TestParseVersionStrict(t *testing.T) {
	for _, s := range []string{"1.0.0+x/../../../evil", `1.0.0-a\b`, "1.0.0+a:b", "1.0.0-a..b", "1.0.0+", "1.0.0-é"} {
		if _, err := parseVersion(s); err == nil {
			t.Errorf("parseVersion(%q) accepted", s)
		}
	}
	for _, s := range []string{"3.2.0", "3.2.0-beta.1", "1.0.0-rc-1+build.5.x-y", "0.0.0"} {
		if _, err := parseVersion(s); err != nil {
			t.Errorf("parseVersion(%q): %v", s, err)
		}
	}
	if _, err := ParseManifest(fuzzManifest("1.0.0+x/../../../evil", ChannelStable)); CodeOf(err) != CodeManifestInvalid {
		t.Fatalf("a version with a path: %v", err)
	}
}

// A package named like a Windows device is refused, with an extension too.
func TestManifestDeviceName(t *testing.T) {
	for _, name := range []string{"CON.exe", "nul", "Aux.zip", "COM1.exe", "lpt¹.dmg", "CONIN$"} {
		raw := bytes.Replace(fuzzManifest("3.2.0", ChannelStable), []byte(`"LectureFlow-3.2.0-x64-portable.exe"`), []byte(`"`+name+`"`), 1)
		if _, err := ParseManifest(raw); err == nil {
			t.Errorf("asset %q accepted", name)
		}
	}
	if isDeviceName("Console.exe") || isDeviceName("COM10.exe") || !isDeviceName("con .exe") {
		t.Fatal("isDeviceName")
	}
}

// pending.json cannot name a device or another file's stream.
func TestAbsPathDevicesAndStreams(t *testing.T) {
	dir := t.TempDir()
	bad := []string{"a/b:c"}
	if runtime.GOOS == "windows" {
		// (with an extension a device name is one on Windows 10 only:
		// filepath.IsLocal asks the system)
		bad = append(bad, "NUL", "3.2.0/CON", "3.2.0/aux ")
	}
	for _, rel := range bad {
		if runtime.GOOS != "windows" && rel == "a/b:c" {
			continue // a plain name elsewhere
		}
		if p, ok := absPath(dir, rel); ok {
			t.Errorf("absPath(%q) = %q", rel, p)
		}
	}
	if p, ok := absPath(dir, "3.2.0/update.json"); !ok || p != filepath.Join(dir, "3.2.0", "update.json") {
		t.Fatalf("%q %v", p, ok)
	}
}

// A directory source with a bare "?" or "#" is refused: every URL made
// from it would carry it.
func TestParseSourceBareQueryOrFragment(t *testing.T) {
	for _, raw := range []string{"https://m.invalid/x/a.json?", "https://m.invalid/x/#", "https://m.invalid/x/a.json#", "https://m.invalid/?/x"} {
		if s, err := parseSource(raw, ""); err == nil {
			t.Errorf("parseSource(%q) = %+v", raw, s)
		}
	}
}

// A long name is cut at the start of a character.
func TestStagingNameKeepsUTF8(t *testing.T) {
	name := strings.Repeat("长", 100) + ".exe"
	got := StagingName(1, name)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, ".exe") || len(got) > len("1-")+120 {
		t.Fatalf("%q", got)
	}
}
