// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build minisign

package selfupdate

// Cross-check with the real minisign (not run by default):
//
//	go test -tags minisign -run RealMinisign ./internal/selfupdate/
//
// needs minisign 0.11+ on PATH (or MINISIGN=path). It makes a throwaway key
// pair without a password in a temporary directory, signs a release's
// update.json the way scripts/release/sign-update.* do (prehashed, and
// legacy with -l), and checks that this package accepts the signatures and
// refuses them once update.json changes.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRealMinisign(t *testing.T) {
	bin := os.Getenv("MINISIGN")
	if bin == "" {
		bin = "minisign"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("minisign not found: %v", err)
	}
	dir := t.TempDir()
	pub, sec := filepath.Join(dir, "test.pub"), filepath.Join(dir, "test.key")
	if out, err := exec.Command(bin, "-G", "-W", "-p", pub, "-s", sec).CombinedOutput(); err != nil {
		t.Fatalf("minisign -G: %v %s", err, out)
	}
	pubText, _ := os.ReadFile(pub)
	pk, err := ParsePublicKey(string(pubText))
	if err != nil {
		t.Fatal(err)
	}
	rel := makeRelease(t, nil, newTestKey(t), "3.2.0", ChannelStable, nil)
	manifest := filepath.Join(dir, "update.json")
	os.WriteFile(manifest, rel.raw, 0o644)
	for _, legacy := range []bool{false, true} {
		args := []string{"-S", "-s", sec, "-m", manifest, "-x", manifest + ".minisig", "-t", TrustedComment("3.2.0"), "-c", "test"}
		if legacy {
			args = append(args, "-l")
		}
		if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("minisign -S (legacy=%v): %v %s", legacy, err, out)
		}
		sig, _ := os.ReadFile(manifest + ".minisig")
		if _, err := verifyManifest(pk, rel.raw, sig); err != nil {
			t.Fatalf("legacy=%v: %v\n%s", legacy, err, sig)
		}
		bad := append([]byte{}, rel.raw...)
		bad[5] ^= 1
		if _, err := verifyManifest(pk, bad, sig); CodeOf(err) != CodeSignatureInvalid {
			t.Fatalf("legacy=%v: a changed update.json: %v", legacy, err)
		}
	}
}
