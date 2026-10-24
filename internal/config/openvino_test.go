// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// SPEC-v2 I7 (phase 3b freeze): python_openvino defaults to the .venv-ov
// interpreter and is made absolute like python; fake_result_openvino is
// resolved like fake_result.
func TestPythonOpenVINODefault(t *testing.T) {
	c := Default()
	want := filepath.Join(".venv-ov", "bin", "python")
	if runtime.GOOS == "windows" {
		want = filepath.Join(".venv-ov", "Scripts", "python.exe")
	}
	if c.PythonOpenVINO != want || c.ASR.FakeResultOpenVINO != "" {
		t.Fatalf("defaults: python_openvino %q (want %q), fake_result_openvino %q", c.PythonOpenVINO, want, c.ASR.FakeResultOpenVINO)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "python_openvino = \"envs/ov/python\"\n[asr]\nfake_result = \"f.json\"\nfake_result_openvino = \"testdata/fake_asr_openvino.json\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.PythonOpenVINO) || !filepath.IsAbs(c.ASR.FakeResultOpenVINO) ||
		!strings.HasSuffix(c.ASR.FakeResultOpenVINO, filepath.Join("testdata", "fake_asr_openvino.json")) {
		t.Fatalf("not absolute: %q %q", c.PythonOpenVINO, c.ASR.FakeResultOpenVINO)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(c.PythonOpenVINO, ".exe") {
		t.Fatalf("python_openvino without .exe: %q", c.PythonOpenVINO)
	}
}

// config.example.toml (task B adds python_openvino) loads with the same
// interpreters as the defaults.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(c.PythonOpenVINO, filepath.Join(".venv-ov", "Scripts", "python.exe")) ||
			!strings.HasSuffix(c.Python, filepath.Join(".venv", "Scripts", "python.exe")) {
			t.Fatalf("interpreters %q %q", c.Python, c.PythonOpenVINO)
		}
	}
	if !filepath.IsAbs(c.PythonOpenVINO) || c.ASR.FakeResultOpenVINO != "" || c.ASR.FakeResult != "" {
		t.Fatalf("example: %+v", c)
	}
}
