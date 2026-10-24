// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8420" || c.ASR.MaxParallel != 1 || c.ASR.VRAMPerJobMB != 5000 || c.ASR.VRAMReserveMB != 1000 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.LLM.Roles[RoleTranslate].Window != 3 {
		t.Fatal("translate window default must be 3")
	}
	for _, r := range []string{RoleGlossary, RoleSummary, RoleRepair, RoleTranslate} {
		if m := c.LLM.Roles[r].Model; m != "deepseek-flash" { // SPEC-v2 P12.1
			t.Errorf("default model of %s = %q", r, m)
		}
	}
	p := c.LLM.Providers["deepseek"]
	if p.BaseURL != "https://api.deepseek.com" || p.APIKeyEnv != "DEEPSEEK_API_KEY" || p.MaxConcurrency != 8 || p.RequestsPerMinute != 300 {
		t.Fatalf("bad provider defaults: %+v", p)
	}
	if runtime.GOOS == "windows" && filepath.Ext(c.FFmpeg) != ".exe" {
		t.Fatal("ffmpeg must get .exe on windows")
	}
	if !c.Storage.KeepMedia() {
		t.Fatal("keep_original_media defaults to true")
	}
	if c.ASR.WorkerMemBudgetMB != 8192 { // power management freeze: CLAUDE.md revision 4
		t.Fatalf("worker_mem_budget_mb default %d", c.ASR.WorkerMemBudgetMB)
	}
}

func TestLocalProviderDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte(`
[llm.providers.ollama]
base_url = "http://127.0.0.1:11434/v1"
local = true
[llm.roles]
translate = { provider = "ollama", model = "qwen" }
glossary = { provider = "ollama", model = "qwen" }
summary = { provider = "ollama", model = "qwen" }
repair = { provider = "ollama", model = "qwen" }
`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.Roles[RoleTranslate].Window != 1 || c.LLM.Providers["ollama"].MaxConcurrency != 1 {
		t.Fatalf("local defaults wrong: %+v", c.LLM)
	}
}

func TestResolveComputeType(t *testing.T) {
	c := Default()
	if c.ResolveComputeType(8192) != "int8_float16" || c.ResolveComputeType(12227) != "float16" {
		t.Fatal("auto compute type wrong")
	}
	c.ASR.ComputeType = "float16"
	if c.ResolveComputeType(4000) != "float16" {
		t.Fatal("explicit compute type must win")
	}
	for _, tc := range []struct {
		ct, device string
		total      int
		want       string
	}{
		{"auto", "cuda", 10 * 1024, "float16"},
		{"auto", "cuda", 10*1024 - 1, "int8_float16"},
		{"auto", "cuda", 0, "int8_float16"}, // unknown VRAM: the safer choice
		{"", "cuda", 24000, "float16"},      // empty behaves like auto
		{"auto", "cpu", 24000, "int8"},
		{"int8_float16", "cuda", 24000, "int8_float16"},
		{"float16", "cpu", 0, "float16"},
	} {
		if got := ResolveComputeType(tc.ct, tc.device, tc.total); got != tc.want {
			t.Errorf("ResolveComputeType(%q, %q, %d) = %q, want %q", tc.ct, tc.device, tc.total, got, tc.want)
		}
	}
}

func TestRejectsNonLocalListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte(`listen = "0.0.0.0:8420"`), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadMakesPathsAbsolute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte(`
data_dir = "./data"
python = '.venv\Scripts\python.exe'
ffmpeg = "ffmpeg"
[asr]
fake_result = "testdata/fake_asr.json"
`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"data_dir": c.DataDir, "worker_dir": c.WorkerDir, "fake_result": c.ASR.FakeResult} {
		if !filepath.IsAbs(p) {
			t.Fatalf("%s not absolute: %q", name, p)
		}
	}
	if runtime.GOOS == "windows" && !filepath.IsAbs(c.Python) {
		t.Fatalf("python with a directory part must become absolute: %q", c.Python)
	}
	if filepath.IsAbs(c.FFmpeg) {
		t.Fatalf("bare executable names stay for PATH lookup: %q", c.FFmpeg)
	}
}

func TestMask(t *testing.T) {
	if Mask("sk-abcdef1234") != "sk-…1234" {
		t.Fatal(Mask("sk-abcdef1234"))
	}
}
