// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/components"
)

// gcloud (kind tool) never gets an offline archive.
func TestToolOfflineRefused(t *testing.T) {
	src := readSrc(t)
	uf, err := loadUserFetched(testAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	if kindOf(src, components.IDGcloud) != components.KindTool {
		t.Fatalf("gcloud kind %q", kindOf(src, components.IDGcloud))
	}
	why := offlineRefusal(src, filepath.Dir(testOut), components.IDGcloud, uf)
	if !strings.Contains(why, "tool") || !strings.Contains(why, "vendor") {
		t.Fatalf("refusal %q", why)
	}
	before, _ := os.ReadFile(testOut)
	outDir := t.TempDir()
	if err := run(testSrc, testOut, "gcloud", outDir, testAllowlist); err == nil || !strings.Contains(err.Error(), "offline gcloud refused") {
		t.Fatalf("run: %v", err)
	}
	if after, _ := os.ReadFile(testOut); !bytes.Equal(before, after) {
		t.Error("components.json was rewritten")
	}
	if es, _ := os.ReadDir(outDir); len(es) != 0 {
		t.Errorf("archives written: %v", es)
	}
	// an offline entry kept from an earlier run is dropped
	m := &components.Manifest{Components: []components.Component{{ID: components.IDGcloud, Kind: components.KindTool,
		Platforms: map[string]*components.Build{"windows-x64": {Offline: &components.Offline{SHA256: strings.Repeat("a", 64), Size: 1}}}}}}
	dropUserFetchedOffline(src, filepath.Dir(testOut), m, uf)
	if m.Components[0].Platforms["windows-x64"].Offline != nil {
		t.Error("gcloud keeps an offline entry")
	}
}

func TestCheckTools(t *testing.T) {
	sizes := map[string]int64{}
	var asked []string
	old := headSize
	headSize = func(u string) (int64, error) {
		asked = append(asked, u)
		n, ok := sizes[u]
		if !ok {
			return 0, errors.New("HEAD " + u + ": HTTP 404")
		}
		return n, nil
	}
	t.Cleanup(func() { headSize = old })
	tool := func(url string, size int64) *components.Manifest {
		return &components.Manifest{Components: []components.Component{
			{ID: "python", Kind: components.KindPythonRuntime, Platforms: map[string]*components.Build{"windows-x64": {
				Files: []components.File{{Name: "py.tar.gz", URL: "{python}/py.tar.gz", Size: 9}}}}},
			{ID: "gcloud", Kind: components.KindTool, Platforms: map[string]*components.Build{"windows-x64": {
				Files: []components.File{{Name: "g.zip", URL: url, Size: size}}}}},
		}}
	}
	sizes["https://dl.example/g.zip"] = 100
	if err := checkTools(tool("https://dl.example/g.zip", 100)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Errorf("HEAD requests %v (only tools are checked)", asked)
	}
	for name, m := range map[string]*components.Manifest{
		"size changed": tool("https://dl.example/g.zip", 99),
		"gone":         tool("https://dl.example/other.zip", 100),
		"not https":    tool("http://dl.example/g.zip", 100),
	} {
		if err := checkTools(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the shipped manifest pins https URLs on dl.google.com with their sizes
	man, err := components.LoadManifest(testOut)
	if err != nil {
		t.Fatal(err)
	}
	c := man.Component(components.IDGcloud)
	if c == nil {
		t.Fatal("no gcloud in components.json")
	}
	for plat, b := range c.Platforms {
		f := b.Files[0]
		if !strings.HasPrefix(f.URL, "https://dl.google.com/") || f.Size != b.DownloadSize {
			t.Errorf("%s: %+v", plat, f)
		}
		sizes[f.URL] = f.Size
	}
	if err := checkTools(man); err != nil {
		t.Errorf("shipped manifest: %v", err)
	}
}
