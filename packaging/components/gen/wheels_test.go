// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"testing"
)

func TestWheelScore(t *testing.T) {
	win := wheelTarget{pyMinor: 12, platform: "windows-x64"}
	mac := wheelTarget{pyMinor: 12, platform: "darwin-arm64", macMajor: 13, macMinor: 0}
	lin := wheelTarget{pyMinor: 12, platform: "linux-x64"}
	cases := []struct {
		file string
		t    wheelTarget
		ok   bool
	}{
		{"numpy-2.5.4-cp312-cp312-win_amd64.whl", win, true},
		{"numpy-2.5.4-cp313-cp313-win_amd64.whl", win, false},
		{"numpy-2.5.4-cp312-cp312-win32.whl", win, false},
		{"cryptography-50.0.2-cp311-abi3-win_amd64.whl", win, true},
		{"cryptography-50.0.2-cp314-abi3-win_amd64.whl", win, false},
		{"pypdf-6.21.2-py3-none-any.whl", win, true},
		{"six-1.16.0-py2.py3-none-any.whl", mac, true},
		{"mlx-0.29.3-cp312-cp312-macosx_13_0_arm64.whl", mac, true},
		{"mlx-0.29.3-cp312-cp312-macosx_14_0_arm64.whl", mac, false},
		{"numpy-2.5.4-cp312-cp312-macosx_11_0_arm64.whl", mac, true},
		{"x-1.0-cp312-cp312-macosx_10_9_universal2.whl", mac, true},
		{"x-1.0-cp312-cp312-macosx_10_9_x86_64.whl", mac, false},
		{"x-1.0.tar.gz", win, false},
		// linux-x64 (ssh-mvp): manylinux up to glibc 2.28, x86_64 only
		{"ctranslate2-4.8.3-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", lin, true},
		{"av-15.1.0-cp312-cp312-manylinux_2_28_x86_64.whl", lin, true},
		{"x-1.0-cp312-cp312-manylinux_2_34_x86_64.whl", lin, false},
		{"x-1.0-cp312-cp312-manylinux_2_28_aarch64.whl", lin, false},
		{"x-1.0-cp312-cp312-musllinux_1_2_x86_64.whl", lin, false},
		{"x-1.0-cp312-cp312-linux_x86_64.whl", lin, false},
		{"cffi-2.1.1-cp312-cp312-manylinux1_x86_64.whl", lin, true},
		{"numpy-2.5.4-cp312-cp312-win_amd64.whl", lin, false},
	}
	for _, c := range cases {
		if got := wheelScore(c.file, c.t) > 0; got != c.ok {
			t.Errorf("%s: %v", c.file, got)
		}
	}
	if wheelScore("a-1-cp312-cp312-win_amd64.whl", win) <= wheelScore("a-1-py3-none-any.whl", win) {
		t.Error("a platform wheel must win over a pure one")
	}
}

func TestParseLock(t *testing.T) {
	pkgs, err := parseLock("# header\nanyio==4.15.1 \\\n    --hash=sha256:aa \\\n    --hash=sha256:bb\n    # via x\nav==15.1.0 \\\n    --hash=sha256:cc\n")
	if err != nil || len(pkgs) != 2 || pkgs[0].Name != "anyio" || len(pkgs[0].Hashes) != 2 || pkgs[1].Version != "15.1.0" {
		t.Fatalf("%+v %v", pkgs, err)
	}
	if _, err := parseLock("unpinned>=1\n"); err == nil {
		t.Error("unpinned accepted")
	}
	if _, err := parseLock("a==1\n"); err == nil {
		t.Error("missing hash accepted")
	}
}

func TestDeterministicZip(t *testing.T) {
	e := map[string][]byte{"b": []byte("2"), "a": []byte("1"), "wheels/x.whl": []byte("w")}
	z1, _ := deterministicZip(e)
	z2, _ := deterministicZip(map[string][]byte{"wheels/x.whl": []byte("w"), "a": []byte("1"), "b": []byte("2")})
	if !bytes.Equal(z1, z2) {
		t.Error("archives differ")
	}
}
