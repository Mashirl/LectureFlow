// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A private directory made here is reported as private; its parents are
// created as os.MkdirAll would; an existing directory is left alone.
func TestMkdirPrivate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "data")
	created, err := MkdirPrivate(dir)
	if err != nil || !created {
		t.Fatalf("MkdirPrivate: %v %v", created, err)
	}
	if e, err := Check(dir); err != nil || e.Exposed() {
		t.Fatalf("a new private directory is exposed: %+v %v", e, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !fi.IsDir() {
		t.Fatalf("parents: %v", err)
	}
	// what is created inside inherits the permissions
	sub := filepath.Join(dir, "updates", "3.2.0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "pending.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e, err := Check(filepath.Join(dir, "updates")); err != nil || e.Exposed() {
		t.Fatalf("a directory inside: %+v %v", e, err)
	}
	if created, err := MkdirPrivate(dir); err != nil || created {
		t.Fatalf("again: %v %v", created, err)
	}
	f := filepath.Join(root, "file")
	os.WriteFile(f, nil, 0o644)
	if _, err := MkdirPrivate(f); err == nil {
		t.Fatal("a file is not a directory")
	}
	if _, err := MkdirPrivate(""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestMkdirTempPrivate(t *testing.T) {
	parent := t.TempDir()
	seen := map[string]bool{}
	for range 3 {
		d, err := MkdirTempPrivate(parent, "lectureflow-update-*")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(d) != parent || !strings.HasPrefix(filepath.Base(d), "lectureflow-update-") || seen[d] {
			t.Fatalf("temp dir %q", d)
		}
		seen[d] = true
		if e, err := Check(d); err != nil || e.Exposed() {
			t.Fatalf("%s exposed: %+v %v", d, e, err)
		}
	}
	if _, err := MkdirTempPrivate(parent, "a/b*"); err == nil {
		t.Fatal("a separator in the pattern is accepted")
	}
}

func TestCheckMissing(t *testing.T) {
	if _, err := Check(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing directory: %v", err)
	}
	if err := Restrict(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Restrict of a missing directory")
	}
}
