// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

const builtinManifest = "../../packaging/components/components.json"

// The shipped manifest (packaging/components/components.json) parses,
// validates, and its lock files are the ones it pins.
func TestBuiltinManifest(t *testing.T) {
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Compatible("3.0.0") || m.Compatible("4.0.0") {
		t.Errorf("compat %q", m.Compat)
	}
	for _, c := range m.Components {
		for plat, b := range c.Platforms {
			if b == nil || b.Lock == nil {
				continue
			}
			p := filepath.Join(filepath.Dir(builtinManifest), filepath.FromSlash(b.Lock.Path))
			sum, _, err := sha256File(p, nil)
			if err != nil || sum != b.Lock.SHA256 {
				t.Errorf("%s %s: lock %s: sha256 %s, manifest %s (%v); run packaging/components/gen",
					c.ID, plat, b.Lock.Path, sum, b.Lock.SHA256, err)
			}
			data, _ := os.ReadFile(p)
			reqs := 0
			for _, line := range strings.Split(string(data), "\n") {
				if line != "" && line[0] != ' ' && line[0] != '#' && strings.Contains(line, "==") {
					reqs++
					if !strings.HasSuffix(strings.TrimSpace(line), `\`) {
						t.Errorf("%s: %q has no hashes", b.Lock.Path, line)
					}
				}
			}
			if reqs != b.Lock.Packages {
				t.Errorf("%s: %d packages, manifest says %d", b.Lock.Path, reqs, b.Lock.Packages)
			}
		}
	}
	// the components of .claude/v3-brief.md §4 and the ids hwdetect recommends
	want := map[string]string{
		IDCore:                                  "",
		IDPython:                                PlatformWindowsX64,
		IDASRCUDA:                               PlatformWindowsX64,
		IDASROpenVINO:                           PlatformWindowsX64,
		IDASRMLX:                                PlatformDarwinARM64,
		"model:faster-whisper:large-v3:float16": PlatformWindowsX64,
		"model:faster-whisper:large-v3:int8_float16":       PlatformWindowsX64,
		"model:faster-whisper:large-v3-turbo:int8_float16": PlatformWindowsX64,
		"model:faster-whisper:medium:int8_float16":         PlatformWindowsX64,
		"model:openvino:large-v3-turbo:fp16":               PlatformWindowsX64,
		"model:openvino:large-v3-turbo:int8":               PlatformWindowsX64,
		"model:openvino:small:int8":                        PlatformWindowsX64,
		"model:mlx:large-v3-turbo:fp16":                    PlatformDarwinARM64,
		"model:mlx:small:fp16":                             PlatformDarwinARM64,
	}
	for id, plat := range want {
		c := m.Component(id)
		if c == nil {
			t.Errorf("missing %s", id)
			continue
		}
		if plat == "" {
			continue
		}
		if r := c.availableFor(plat, "3.0.0"); r != "" {
			t.Errorf("%s is not available on %s: %s", id, plat, r)
		}
	}
	if m.Component(IDPython).Build(PlatformDarwinARM64) == nil {
		t.Error("python needs a macOS build")
	}
	// every model names its license and source (V7)
	for _, c := range m.Components {
		if c.Kind == KindModel && (c.License == "" || c.Source == "" || c.Revision == "") {
			t.Errorf("%s: license, source and revision are required", c.ID)
		}
	}
	// presets cover every role of the application
	for _, tier := range m.Presets.Tiers {
		if tier.TranscribeOnly {
			continue
		}
		for _, role := range model.Roles {
			if _, ok := tier.Roles[role]; !ok {
				t.Errorf("tier %s lacks role %s", tier.ID, role)
			}
		}
		for role := range tier.Roles {
			if !contains(model.Roles, role) {
				t.Errorf("tier %s: unknown role %s", tier.ID, role)
			}
		}
	}
	if m.Presets.Default != "standard" {
		t.Errorf("default tier %s", m.Presets.Default)
	}
}

// A manager opens with the shipped manifest.
func TestBuiltinManifestOpens(t *testing.T) {
	abs, _ := filepath.Abs(builtinManifest)
	m, err := New(Options{DataDir: t.TempDir(), AppVersion: "3.0.0", Platform: PlatformWindowsX64, BuiltinManifest: abs})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	p, err := m.Plan([]string{"model:faster-whisper:large-v3:float16"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 3 || p.Items[0].ID != IDPython || p.Items[1].ID != IDASRCUDA {
		t.Errorf("plan %+v", p.Items)
	}
	if s := m.SuggestSources("cn"); s.PyPI != "tuna" || s.Models != "hf-mirror" {
		t.Errorf("cn sources %+v", s)
	}
}

// Every preset provider names its limits and where they come from; DeepSeek
// has those of the development configuration (config.example.toml: 64
// concurrent requests, no per-minute limit; perf-llm, NOTES.md — was 8 / 300
// since V12.2 acceptance issue 2). rpm 0 = no per-minute limit.
func TestShippedPresetLimits(t *testing.T) {
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Presets.Providers {
		if p.MaxConcurrency < 1 || p.RPM < 0 || len(p.Refs) == 0 {
			t.Errorf("%s: max_concurrency %d, rpm %d, refs %v", p.ID, p.MaxConcurrency, p.RPM, p.Refs)
		}
		if p.ID == "deepseek" && (p.MaxConcurrency != 64 || p.RPM != 0) {
			t.Errorf("deepseek: %d / %d, want 64 / 0 (no per-minute limit)", p.MaxConcurrency, p.RPM)
		}
		if p.ID != "deepseek" && p.RPM < 1 {
			t.Errorf("%s: free channels have per-minute limits (rpm %d)", p.ID, p.RPM)
		}
		if p.Free && p.MaxConcurrency > 2 {
			t.Errorf("%s: a free channel should stay conservative (max_concurrency %d)", p.ID, p.MaxConcurrency)
		}
	}
}

// The installed sizes of the Windows environments match a real installation
// (V12.2 acceptance, Windows 11, the bytes state.json recorded right after
// installing): not below it (the disk check would let an install run out of
// space) and at most 10 % above it.
func TestShippedInstalledSizesMatchMeasurement(t *testing.T) {
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	measured := map[string]int64{IDPython: 79170228, IDASRCUDA: 2377577201, IDASROpenVINO: 315083908}
	for id, n := range measured {
		c := m.Component(id)
		if c == nil || c.Build(PlatformWindowsX64) == nil {
			t.Errorf("%s: no windows-x64 build", id)
			continue
		}
		if got := c.Build(PlatformWindowsX64).InstalledSize; got < n || float64(got) > float64(n)*1.1 {
			t.Errorf("%s: installed_size %d, measured %d", id, got, n)
		}
	}
}
