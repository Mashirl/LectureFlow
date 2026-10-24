// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/components"
)

const (
	testAllowlist = "../../licenses/allowlist.json"
	testSrc       = "../manifest.src.json"
	testOut       = "../components.json" // its directory has locks/
)

func readSrc(t *testing.T) *srcManifest {
	t.Helper()
	data, err := os.ReadFile(testSrc)
	if err != nil {
		t.Fatal(err)
	}
	var src srcManifest
	if err := json.Unmarshal(data, &src); err != nil {
		t.Fatal(err)
	}
	return &src
}

func TestNormName(t *testing.T) {
	for in, want := range map[string]string{
		"NVIDIA_cuBLAS.cu12": "nvidia-cublas-cu12",
		"ctranslate2":        "ctranslate2",
		" Av ":               "av",
		"a--b__c..d":         "a-b-c-d",
	} {
		if got := normName(in); got != want {
			t.Errorf("normName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserFetchedFromAllowlist(t *testing.T) {
	uf, err := loadUserFetched(testAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"nvidia-cublas-cu12", "nvidia-cudnn-cu12", "av", "ctranslate2", "regex"} {
		if !uf.has(lockedPkg{Name: n, Version: "1.0"}) {
			t.Errorf("%s is not user-fetched", n)
		}
	}
	for _, n := range []string{"pypdf", "cryptography", "modernc.org/memory", "sax"} {
		if uf.has(lockedPkg{Name: n, Version: "1.0"}) {
			t.Errorf("%s counted as user-fetched", n)
		}
	}
	// a version-specific entry
	uf2 := userFetched{"pkg": {"2.0"}}
	if uf2.has(lockedPkg{Name: "Pkg", Version: "1.0"}) || !uf2.has(lockedPkg{Name: "PKG", Version: "2.0"}) {
		t.Error("version-specific entry")
	}
}

// The shipped lock files: asr-cuda and asr-mlx carry user-fetched packages
// and get no offline archive; python, asr-openvino and models may.
func TestOfflineRefusal(t *testing.T) {
	src := readSrc(t)
	uf, err := loadUserFetched(testAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(testOut)
	for id, refused := range map[string]bool{
		"asr-cuda": true, "asr-mlx": true,
		"python": false, "asr-openvino": false, "model:openvino:small:int8": false,
	} {
		why := offlineRefusal(src, dir, id, uf)
		if (why != "") != refused {
			t.Errorf("%s: refusal %q, want refused=%v", id, why, refused)
		}
	}
	why := offlineRefusal(src, dir, "asr-cuda", uf)
	if !strings.Contains(why, "nvidia-cublas-cu12==") || !strings.Contains(why, "user-fetched") {
		t.Errorf("asr-cuda: %s", why)
	}
	// a lock that cannot be read is refused, not waved through
	if why := offlineRefusal(src, t.TempDir(), "python", uf); !strings.Contains(why, "cannot check") {
		t.Errorf("missing lock: %q", why)
	}
}

// -offline asr-cuda fails before anything is fetched or written.
func TestRunRefusesUserFetchedOffline(t *testing.T) {
	before, err := os.ReadFile(testOut)
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	err = run(testSrc, testOut, "python, asr-cuda", outDir, testAllowlist)
	if err == nil || !strings.Contains(err.Error(), "offline asr-cuda refused") || strings.Contains(err.Error(), "offline python") {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(testOut)
	if !bytes.Equal(before, after) {
		t.Error("components.json was rewritten")
	}
	if es, _ := os.ReadDir(outDir); len(es) != 0 {
		t.Errorf("archives written: %v", es)
	}
	// without the allowlist -offline does not run at all
	err = run(testSrc, testOut, "python", outDir, filepath.Join(t.TempDir(), "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("missing allowlist: %v", err)
	}
}

// An offline entry kept from an earlier components.json is dropped for a
// component with user-fetched packages.
func TestDropUserFetchedOffline(t *testing.T) {
	src := readSrc(t)
	uf, err := loadUserFetched(testAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	off := func() *components.Offline {
		return &components.Offline{Name: "x.zip", SHA256: strings.Repeat("a", 64), Size: 1}
	}
	m := &components.Manifest{Components: []components.Component{
		{ID: "asr-cuda", Platforms: map[string]*components.Build{"windows-x64": {Offline: off()}}},
		{ID: "python", Platforms: map[string]*components.Build{"windows-x64": {Offline: off()}}},
	}}
	dropUserFetchedOffline(src, filepath.Dir(testOut), m, uf)
	if m.Component("asr-cuda").Platforms["windows-x64"].Offline != nil {
		t.Error("asr-cuda keeps its offline entry")
	}
	if m.Component("python").Platforms["windows-x64"].Offline == nil {
		t.Error("python lost its offline entry")
	}
}
