// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// v3-brief §1: -listen and -data-dir take precedence over the file.
func TestLoadWithOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:9000\"\ndata_dir = \"filedata\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWith(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || filepath.Base(c.DataDir) != "filedata" || !filepath.IsAbs(c.DataDir) {
		t.Fatalf("file values: listen %q data %q", c.Listen, c.DataDir)
	}
	want := filepath.Join(dir, "flagdata")
	c, err = LoadWith(path, Overrides{Listen: "127.0.0.1:0", DataDir: want})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:0" || c.DataDir != want {
		t.Fatalf("overrides: listen %q data %q", c.Listen, c.DataDir)
	}
	// A relative -data-dir is made absolute like the file's (absPaths).
	c, err = LoadWith(path, Overrides{DataDir: "reldata"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Base(c.DataDir) != "reldata" {
		t.Fatalf("relative override: %q", c.DataDir)
	}
	// The listen rule (SPEC §0) applies to the override too.
	if _, err := LoadWith(path, Overrides{Listen: "0.0.0.0:80"}); err == nil {
		t.Fatal("listen on 0.0.0.0 must be refused")
	}
}

// -resources: worker/, bin/ffmpeg, bin/uv are taken from the directory when
// they exist; otherwise the defaults stay.
func TestResourcesDir(t *testing.T) {
	res := t.TempDir()
	c, err := LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.ResourcesDir != res || c.FFmpeg != exe("ffmpeg") || filepath.Base(c.WorkerDir) != "worker" || c.WorkerDir == filepath.Join(res, "worker") || c.UV != "" {
		t.Fatalf("empty resources must keep the defaults: %+v", c)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(res, "worker"), 0o755))
	must(os.MkdirAll(filepath.Join(res, "bin"), 0o755))
	must(os.WriteFile(filepath.Join(res, "bin", exe("ffmpeg")), nil, 0o755))
	must(os.WriteFile(filepath.Join(res, "bin", exe("uv")), nil, 0o755))
	c, err = LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerDir != filepath.Join(res, "worker") || c.FFmpeg != filepath.Join(res, "bin", exe("ffmpeg")) || c.UV != filepath.Join(res, "bin", exe("uv")) {
		t.Fatalf("resources not used: worker %q ffmpeg %q uv %q", c.WorkerDir, c.FFmpeg, c.UV)
	}
	// Without -resources nothing changes (web mode as before).
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ResourcesDir != "" || c.UV != "" || c.FFmpeg != exe("ffmpeg") {
		t.Fatalf("no resources: %+v", c)
	}
}
