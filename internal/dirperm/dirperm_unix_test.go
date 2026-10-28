// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package dirperm

import (
	"os"
	"path/filepath"
	"testing"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The walk from / down: others reach a directory only through directories
// they may traverse; a parent they may write without the sticky bit, or
// own, lets them rename it; the user's private group is not another
// account.
func TestExposureOf(t *testing.T) {
	const me = 501
	chain := func(modes ...os.FileMode) []node {
		var out []node
		for i, m := range modes {
			out = append(out, node{path: "/" + string(rune('a'+i)), mode: m, uid: me, group: "staff"})
		}
		out[0].uid = 0
		out[0].path = "/"
		return out
	}
	for _, c := range []struct {
		name        string
		nodes       []node
		write, read bool
	}{
		{"private", chain(0o755, 0o755, 0o700), false, false},
		{"0777 behind ~/Library (0700)", chain(0o755, 0o700, 0o777), false, false},
		{"0755 reachable", chain(0o755, 0o755, 0o755), false, true},
		{"0775 group staff", chain(0o755, 0o755, 0o775), true, true},
		{"0777", chain(0o755, 0o755, 0o777), true, true},
		{"world-writable parent", chain(0o755, 0o777, 0o700), true, false},
		{"sticky parent (/tmp)", chain(0o755, 0o777|os.ModeSticky, 0o700), false, false},
		{"parent owned by another user", func() []node {
			n := chain(0o755, 0o755, 0o700)
			n[1].uid = 502
			return n
		}(), true, false},
		{"owned by another user", func() []node {
			n := chain(0o755, 0o755, 0o700)
			n[2].uid = 502
			return n
		}(), true, false},
		{"user private group", func() []node {
			n := chain(0o755, 0o755, 0o770)
			n[2].group = "elma"
			return n
		}(), false, false},
	} {
		e := exposureOf(c.nodes, me, "elma")
		if e.Write != c.write || e.Read != c.read {
			t.Errorf("%s: %+v, want write %v read %v", c.name, e, c.write, c.read)
		}
	}
}

// Restrict sets 0700 and Save puts the mode back; a directory made here
// is 0700.
func TestRestrictSaveUnix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777)
	restore, err := Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restrict(dir); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, dir); m != 0o700 {
		t.Fatalf("mode %o after Restrict", m)
	}
	if e, err := Check(dir); err != nil || e.Exposed() {
		t.Fatalf("after Restrict: %+v %v", e, err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, dir); m != 0o777 {
		t.Fatalf("restored mode %o", m)
	}

	priv := filepath.Join(t.TempDir(), "x", "data")
	if _, err := MkdirPrivate(priv); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, priv); m != 0o700 {
		t.Fatalf("mode %o", m)
	}
	d, err := MkdirTempPrivate(t.TempDir(), "lectureflow-update-*")
	if err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, d); m != 0o700 {
		t.Fatalf("temp mode %o", m)
	}
}
