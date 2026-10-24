// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		default:
			return copyFile(p, target)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The data directory moves (V1.3): after Relocated, the venvs point at the
// runtime in the new place and nothing refers to the old one.
func TestRelocatedDataDir(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, fwFloat16)
	m.Close()

	oldDir := e.data
	newDir := filepath.Join(t.TempDir(), "moved data")
	copyTree(t, oldDir, newDir)
	if err := os.RemoveAll(oldDir); err != nil {
		t.Fatal(err)
	}
	e.data = newDir

	m2 := e.open()
	if s, _ := statusOf(m2, IDASRCUDA); !s.Installed || !s.Broken {
		t.Errorf("before Relocated the venv must show as broken: %+v", s)
	}
	m2.Close()

	if err := Relocated(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	if err := Relocated(oldDir, newDir); err != nil { // idempotent
		t.Fatal(err)
	}
	m3 := e.open()
	for _, s := range m3.List() {
		if s.Installed && s.Broken {
			t.Errorf("%s broken after Relocated", s.ID)
		}
	}
	py, ok := m3.Python(BackendFasterWhisper)
	if !ok || !strings.HasPrefix(py, newDir) {
		t.Fatalf("python %s %v", py, ok)
	}
	for _, venv := range findVenvs(filepath.Join(newDir, "components")) {
		home := homeOf(t, venv)
		if !strings.HasPrefix(home, newDir) {
			t.Errorf("%s: home %s", venv, home)
		}
		if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
			t.Errorf("%s: home %s missing", venv, home)
		}
	}
	// no text file under components/ names the old directory
	filepath.WalkDir(filepath.Join(newDir, "components"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && (strings.HasSuffix(p, ".cfg") || strings.HasSuffix(p, ".json")) {
			b, _ := os.ReadFile(p)
			if strings.Contains(strings.ToLower(string(b)), strings.ToLower(oldDir)) ||
				strings.Contains(strings.ToLower(string(b)), strings.ToLower(strings.ReplaceAll(oldDir, `\`, `\\`))) {
				t.Errorf("%s still names %s", p, oldDir)
			}
		}
		return nil
	})
	// and installs keep working in the new place
	mustInstall(t, m3, Sources{}, ovModel)
}

func TestRebase(t *testing.T) {
	sep := string(filepath.Separator)
	old := filepath.Join(t.TempDir(), "data")
	nw := filepath.Join(t.TempDir(), "new data")
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{old + sep + "components" + sep + "python", nw + sep + "components" + sep + "python", true},
		{old, nw, true},
		{old + "2" + sep + "x", "", false}, // a sibling with the same prefix
		{filepath.Join(t.TempDir(), "elsewhere"), "", false},
		{"", "", false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct {
				in, want string
				ok       bool
			}{strings.ToUpper(old) + `\c`, nw + `\c`, true},
			struct {
				in, want string
				ok       bool
			}{strings.ReplaceAll(old, `\`, "/") + "/c/d", nw + `\c\d`, true},
		)
	}
	for _, c := range cases {
		got, ok := rebase(c.in, old, nw)
		if ok != c.ok || got != c.want {
			t.Errorf("rebase(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
	cmd := `C:\uv.exe venv --python "` + old + sep + `py" ` + old + sep + "v"
	got, ok := rebaseAll(cmd, old, nw)
	if !ok || strings.Contains(got, old+sep) || strings.Count(got, nw) != 2 {
		t.Errorf("rebaseAll: %q", got)
	}
}
