// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package components

import (
	"os"
	"path/filepath"
	"testing"
)

// On macOS venv/bin/python* are symlinks to the base interpreter; Relocated
// points them at the moved runtime.
func TestRelocatedRelinksBin(t *testing.T) {
	oldRoot := filepath.Join(t.TempDir(), "old")
	newRoot := filepath.Join(t.TempDir(), "new")
	runtimeBin := filepath.Join("components", "python", "1.0.0", "runtime", "python", "bin")
	bin := filepath.Join(newRoot, "components", "asr-mlx", "1.0.0", "venv", "bin")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(filepath.Join(newRoot, runtimeBin), 0o755)
	os.WriteFile(filepath.Join(newRoot, runtimeBin, "python3.12"), []byte("py"), 0o755)
	os.WriteFile(filepath.Join(bin, "..", "pyvenv.cfg"), []byte("home = "+filepath.Join(oldRoot, runtimeBin)+"\n"), 0o644)
	if err := os.Symlink(filepath.Join(oldRoot, runtimeBin, "python3.12"), filepath.Join(bin, "python")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("python", filepath.Join(bin, "python3")); err != nil { // relative: unchanged
		t.Fatal(err)
	}
	if err := Relocated(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	got, _ := os.Readlink(filepath.Join(bin, "python"))
	if got != filepath.Join(newRoot, runtimeBin, "python3.12") {
		t.Errorf("python -> %s", got)
	}
	if rel, _ := os.Readlink(filepath.Join(bin, "python3")); rel != "python" {
		t.Errorf("python3 -> %s", rel)
	}
	if !venvOK(filepath.Join(bin, "..")) {
		t.Error("venv not usable after Relocated")
	}
}
