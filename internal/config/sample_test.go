// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// SPEC-v3 V4.3: SampleDir is resources/sample when it has the sample, else
// assets/sample of the working directory, else empty.
func TestSampleDir(t *testing.T) {
	// The package directory has no assets/sample: empty.
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.SampleDir != "" {
		t.Fatalf("no sample: %q", c.SampleDir)
	}

	// Development: assets/sample relative to the working directory, absolute.
	prev := DevSampleDir
	t.Cleanup(func() { DevSampleDir = prev })
	DevSampleDir = filepath.Join("..", "..", "assets", "sample")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(DevSampleDir)
	if c.SampleDir != want {
		t.Fatalf("development sample dir %q, want %q", c.SampleDir, want)
	}

	// Packaged: resources/sample wins when it has the audio file.
	res := t.TempDir()
	c, err = LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.SampleDir != want {
		t.Fatalf("resources without sample: %q", c.SampleDir)
	}
	if err := os.MkdirAll(filepath.Join(res, "sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res, "sample", SampleAudioFile), []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.SampleDir != filepath.Join(res, "sample") {
		t.Fatalf("resources sample dir %q", c.SampleDir)
	}

	// A configured directory without the audio file: empty.
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(path, []byte("sample_dir = \"nosample\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err != nil || c.SampleDir != "" {
		t.Fatalf("missing configured sample: %q %v", c.SampleDir, err)
	}
}

func TestPythonMLXDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.PythonMLX) || filepath.Base(filepath.Dir(filepath.Dir(c.PythonMLX))) != ".venv-mlx" {
		t.Fatalf("python_mlx %q", c.PythonMLX)
	}
}
