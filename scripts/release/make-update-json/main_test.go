// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
)

func releaseDir(t *testing.T, v string, names ...string) (string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{}
	for i, n := range names {
		b := bytes.Repeat([]byte{byte(i + 1)}, 1000+i)
		files[n] = b
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// other release files are ignored
	os.WriteFile(filepath.Join(dir, "latest.yml"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "LectureFlow-Setup-"+v+"-x64.exe.blockmap"), []byte("x"), 0o644)
	return dir, files
}

func TestBuild(t *testing.T) {
	names := []string{"LectureFlow-Setup-3.2.0-x64.exe", "LectureFlow-3.2.0-x64-portable.exe",
		"LectureFlow-3.2.0-arm64.dmg", "LectureFlow-3.2.0-arm64-mac.zip"}
	dir, files := releaseDir(t, "3.2.0", names...)
	now := time.Date(2026, 11, 2, 10, 0, 0, 123, time.UTC)
	m, b, err := build(options{dir: dir, version: "3.2.0", repo: "OWNER/REPO", minVersion: "3.0.0",
		mirrors: []selfupdate.MirrorRule{{Prefix: "https://github.com/", Replace: "https://mirror.invalid/gh/"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selfupdate.ParseManifest(b); err != nil {
		t.Fatal(err)
	}
	if m.Channel != selfupdate.ChannelStable || m.ReleasedAt != "2026-11-02T10:00:00Z" || m.MinVersion != "3.0.0" ||
		m.NotesURL != "https://github.com/OWNER/REPO/releases/tag/v3.2.0" || len(m.Mirrors) != 1 {
		t.Fatalf("%+v", m)
	}
	kinds := map[string]string{names[0]: "installer", names[1]: "portable", names[2]: "dmg", names[3]: "zip"}
	n := 0
	for p, assets := range m.Platforms {
		for _, a := range assets {
			n++
			sum := sha256.Sum256(files[a.Name])
			if a.Kind != kinds[a.Name] || a.Size != int64(len(files[a.Name])) || a.SHA256 != hex.EncodeToString(sum[:]) ||
				a.URL != "https://github.com/OWNER/REPO/releases/download/v3.2.0/"+a.Name {
				t.Errorf("%s: %+v", p, a)
			}
		}
	}
	if n != 4 || !bytes.HasSuffix(b, []byte("}\n")) {
		t.Fatalf("%d packages", n)
	}

	// a beta, the other macOS zip name, the CLI writing the file
	dir, _ = releaseDir(t, "3.3.0-beta.1", "LectureFlow-Setup-3.3.0-beta.1-x64.exe", "LectureFlow-3.3.0-beta.1-x64-portable.exe",
		"LectureFlow-3.3.0-beta.1-arm64.dmg", "LectureFlow-3.3.0-beta.1-arm64.zip")
	var out, errb bytes.Buffer
	if code := run([]string{"-dir", dir, "-version", "3.3.0-beta.1", "-repo", "o/r", "-tag", "v3.3.0-beta.1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "update.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err = selfupdate.ParseManifest(raw)
	if err != nil || m.Channel != selfupdate.ChannelBeta {
		t.Fatalf("%v %+v", err, m)
	}
	if a, ok := m.Asset(selfupdate.PlatformDarwinArm64, selfupdate.KindZip); !ok || a.Name != "LectureFlow-3.3.0-beta.1-arm64.zip" {
		t.Fatalf("zip %+v", a)
	}
	if !strings.Contains(out.String(), "unsigned") {
		t.Fatal(out.String())
	}
}

func TestBuildRefuses(t *testing.T) {
	dir, _ := releaseDir(t, "3.2.0", "LectureFlow-Setup-3.2.0-x64.exe")
	now := time.Now()
	if _, _, err := build(options{dir: dir, version: "3.2.0", repo: "o/r"}, now); err == nil || !strings.Contains(err.Error(), "portable") {
		t.Fatalf("missing packages: %v", err)
	}
	m, _, err := build(options{dir: dir, version: "3.2.0", repo: "o/r", allowMissing: true}, now)
	if err != nil || len(m.Platforms) != 1 {
		t.Fatalf("allow-missing: %v", err)
	}
	empty := t.TempDir()
	if _, _, err := build(options{dir: empty, version: "3.2.0", repo: "o/r", allowMissing: true}, now); err == nil {
		t.Fatal("no package at all accepted")
	}
	for name, o := range map[string]options{
		"bad repo":     {dir: dir, version: "3.2.0", repo: "https://github.com/o/r", allowMissing: true},
		"bad version":  {dir: dir, version: "v3.2.0", repo: "o/r", allowMissing: true},
		"stable beta":  {dir: dir, version: "3.2.0", repo: "o/r", allowMissing: true, channel: "nightly"},
		"bad min":      {dir: dir, version: "3.2.0", repo: "o/r", allowMissing: true, minVersion: "4.0.0"},
		"bad mirror":   {dir: dir, version: "3.2.0", repo: "o/r", allowMissing: true, mirrors: []selfupdate.MirrorRule{{Prefix: "x", Replace: "y"}}},
		"bad released": {dir: dir, version: "3.2.0", repo: "o/r", allowMissing: true, releasedAt: "today"},
	} {
		if _, _, err := build(o, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-dir", dir}, &out, &errb); code != 2 {
		t.Fatalf("missing flags: exit %d", code)
	}
	if code := run([]string{"-dir", dir, "-version", "3.2.0", "-repo", "o/r", "-mirror", "nope"}, &out, &errb); code != 2 {
		t.Fatalf("bad -mirror: exit %d", code)
	}
}
