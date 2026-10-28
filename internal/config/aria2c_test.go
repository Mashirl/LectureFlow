// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ext-brief §6.1: aria2c comes from resources/bin when the installer has
// it, else from config.toml ("aria2c"), else the bare name (PATH).
func TestAria2cPath(t *testing.T) {
	want := "aria2c"
	if runtime.GOOS == "windows" {
		want = "aria2c.exe"
	}
	if got := Default().Aria2c; got != want {
		t.Fatalf("default aria2c %q, want %q", got, want)
	}

	// Without -resources and without the key: PATH lookup, never made absolute.
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Aria2c != want {
		t.Fatalf("no resources: aria2c %q", c.Aria2c)
	}

	// A resources directory without bin/aria2c keeps the default.
	res := t.TempDir()
	c, err = LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.Aria2c != want {
		t.Fatalf("resources without aria2c: %q", c.Aria2c)
	}

	// config.toml: a path is made absolute, ".exe" is added on Windows.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(cfg, []byte("aria2c = \"tools/aria2/aria2c\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = LoadWith(cfg, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Aria2c) || filepath.Base(c.Aria2c) != want {
		t.Fatalf("configured aria2c %q", c.Aria2c)
	}

	// The bundled binary takes precedence over the configured value, like
	// ffmpeg and uv.
	if err := os.MkdirAll(filepath.Join(res, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundled := filepath.Join(res, "bin", want)
	if err := os.WriteFile(bundled, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err = LoadWith(cfg, Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.Aria2c != bundled {
		t.Fatalf("bundled aria2c not used: %q, want %q", c.Aria2c, bundled)
	}

	// A directory named like the binary is not the binary.
	res2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(res2, "bin", want), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err = LoadWith("", Overrides{ResourcesDir: res2})
	if err != nil {
		t.Fatal(err)
	}
	if c.Aria2c != want {
		t.Fatalf("directory taken for aria2c: %q", c.Aria2c)
	}
}
