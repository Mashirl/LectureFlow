// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/selfupdate"
	"lectureflow/internal/selfupdate/selfupdatetest"
)

// updatesDir is {a new private data directory}/updates, as LectureFlow
// makes them (the system's temporary directory may let other accounts in,
// which -selfupdate-verify refuses).
func updatesDir(t *testing.T) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	if _, err := dirperm.MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "updates")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeRelease lays out version's signed update.json and its installer
// under dir/<version>/ for this system's platform and returns the
// installer's name, content and SHA-256.
func writeRelease(t *testing.T, k *selfupdatetest.Key, dir, version string) (string, []byte, string) {
	t.Helper()
	platform := selfupdate.CurrentPlatform()
	if platform == "" {
		t.Skip("no update packages for this platform")
	}
	pkg := bytes.Repeat([]byte("installer "+version+" "), 4096)
	sum := sha256.Sum256(pkg)
	name := "LectureFlow-Setup-" + version + "-x64.exe"
	m := selfupdate.Manifest{Schema: 1, App: selfupdate.AppName, Version: version, Channel: selfupdate.ChannelStable,
		ReleasedAt: "2026-11-02T10:00:00Z", Platforms: map[string][]selfupdate.Asset{platform: {{Kind: selfupdate.KindInstaller,
			Name: name, URL: "https://example.invalid/" + name, Size: int64(len(pkg)), SHA256: hex.EncodeToString(sum[:])}}}}
	raw, _ := json.Marshal(m)
	vdir := filepath.Join(dir, version)
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "update.json"), raw, 0o644)
	os.WriteFile(filepath.Join(vdir, "update.json.minisig"), k.Sign(raw, selfupdate.TrustedComment(version), true), 0o644)
	os.WriteFile(filepath.Join(vdir, name), pkg, 0o644)
	return name, pkg, hex.EncodeToString(sum[:])
}

// writePending lays out a verified pending update of 3.2.0 for this
// system's platform under dir (signed with a key made for the test) and
// returns the key and the package's path.
func writePending(t *testing.T, dir string) (*selfupdatetest.Key, string) {
	t.Helper()
	k, err := selfupdatetest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	name, pkg, sum := writeRelease(t, k, dir, "3.2.0")
	p := selfupdate.Pending{Schema: 1, Version: "3.2.0", FromVersion: "3.1.0", Channel: selfupdate.ChannelStable, Platform: selfupdate.CurrentPlatform(),
		Kind: selfupdate.KindInstaller, File: "3.2.0/" + name, Size: int64(len(pkg)), SHA256: sum,
		Manifest: "3.2.0/update.json", Signature: "3.2.0/update.json.minisig", Origin: selfupdate.OriginFile}
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(dir, "pending.json"), b, 0o644)
	return k, filepath.Join(dir, "3.2.0", name)
}

func runVerify(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runMain(runEnv{args: args, stdin: strings.NewReader(""), stdout: &out, stderr: &errb, exit: func(int) {}})
	return code, out.String()
}

func useKey(t *testing.T, k *selfupdatetest.Key) {
	orig := selfupdateVerifyKey
	t.Cleanup(func() { selfupdateVerifyKey = orig })
	selfupdateVerifyKey = func() string { return k.PublicKey() }
}

func TestSelfUpdateVerifyCommand(t *testing.T) {
	dir := updatesDir(t)
	k, pkg := writePending(t, dir)
	useKey(t, k)

	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0")
	if code != 0 || !strings.HasPrefix(out, "ok version=3.2.0 kind=installer file=3.2.0/") || strings.Contains(out, "copy=") || strings.Count(out, "\n") != 1 {
		t.Fatalf("valid: %d %q", code, out)
	}
	// not newer than the running version
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_newer: ") {
		t.Fatalf("same version: %d %q", code, out)
	}
	// a changed package
	b, _ := os.ReadFile(pkg)
	b[len(b)/2] ^= 1
	os.WriteFile(pkg, b, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.checksum: ") {
		t.Fatalf("changed package: %d %q", code, out)
	}
	b[len(b)/2] ^= 1
	os.WriteFile(pkg, b, 0o644)
	// no pending update
	if code, out := runVerify(t, "-selfupdate-verify", updatesDir(t)); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_available: ") {
		t.Fatalf("nothing pending: %d %q", code, out)
	}
	// only checks: nothing was written next to the files
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("the updates directory changed: %v", entries)
	}
	// a build without a key
	selfupdateVerifyKey = func() string { return "" }
	if code, out := runVerify(t, "-selfupdate-verify", dir); code != 2 || out != "not_configured: this build has no update public key\n" {
		t.Fatalf("no key: %d %q", code, out)
	}
	// a data directory that does not exist cannot be checked: untrusted
	if code, out := runVerify(t, "-selfupdate-verify", filepath.Join(t.TempDir(), "gone", "updates")); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") {
		t.Fatalf("missing data directory: %d %q", code, out)
	}
}

// -selfupdate-copy: the package is copied into a new private directory and
// named on the ok line; the shell runs that copy (sec-update M3).
func TestSelfUpdateVerifyCopy(t *testing.T) {
	dir := updatesDir(t)
	k, pkg := writePending(t, dir)
	useKey(t, k)
	tmp := t.TempDir()
	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-copy", tmp)
	line := strings.TrimSuffix(out, "\n")
	i := strings.Index(line, " copy=")
	if code != 0 || i < 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("copy: %d %q", code, out)
	}
	cp := line[i+len(" copy="):]
	want, _ := os.ReadFile(pkg)
	got, err := os.ReadFile(cp)
	if err != nil || !bytes.Equal(got, want) || filepath.Dir(filepath.Dir(cp)) != tmp || !strings.HasPrefix(filepath.Base(filepath.Dir(cp)), selfupdate.CopyPrefix) {
		t.Fatalf("the copy %q: %v", cp, err)
	}
	if e, err := dirperm.Check(filepath.Dir(cp)); err != nil || e.Exposed() {
		t.Fatalf("the copy's directory: %+v %v", e, err)
	}
}

// -selfupdate-previous: the package a rollback runs is verified like the
// update (signature, size and SHA-256, older than this version).
func TestSelfUpdateVerifyPrevious(t *testing.T) {
	dir := updatesDir(t)
	k, _ := writePending(t, dir)
	useKey(t, k)
	name, pkg, sum := writeRelease(t, k, dir, "3.1.0")
	prev := selfupdate.Previous{Schema: 1, Version: "3.1.0", Platform: selfupdate.CurrentPlatform(), Kind: selfupdate.KindInstaller,
		File: "3.1.0/" + name, Size: int64(len(pkg)), SHA256: sum, CreatedAt: "2026-11-02T12:00:00Z"}
	b, _ := json.Marshal(prev)
	os.WriteFile(filepath.Join(dir, "previous.json"), b, 0o644)
	tmp := t.TempDir()
	code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0", "-selfupdate-copy", tmp)
	if code != 0 || !strings.HasPrefix(out, "ok version=3.1.0 kind=installer file=3.1.0/"+name+" sha256="+sum+" copy="+tmp) {
		t.Fatalf("previous: %d %q", code, out)
	}
	// not older than the running version
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.1.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.manifest_invalid: ") {
		t.Fatalf("same version: %d %q", code, out)
	}
	// a changed package
	f := filepath.Join(dir, "3.1.0", name)
	pkg[10] ^= 1
	os.WriteFile(f, pkg, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.checksum: ") {
		t.Fatalf("changed package: %d %q", code, out)
	}
	// previous.json without a package (only a backup)
	prev.File, prev.Size, prev.SHA256, prev.Backup = "", 0, "", "backup/3.1.0"
	b, _ = json.Marshal(prev)
	os.WriteFile(filepath.Join(dir, "previous.json"), b, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_available: ") {
		t.Fatalf("backup only: %d %q", code, out)
	}
}
