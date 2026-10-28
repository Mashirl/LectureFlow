// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"lectureflow/internal/sched"
)

// readAll returns the contents of every file in dir by name.
func readAll(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func names(m map[string]string) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tmpFor hands out out.tmp, removing what an earlier run left there.
func TestTmpForRemovesStale(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "audio.wav")
	write(t, out+".tmp", "half of an earlier run")
	tmp, err := tmpFor(out)
	if err != nil || tmp != out+".tmp" {
		t.Fatalf("tmpFor = %q, %v", tmp, err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the earlier run's %s is still there: %v", tmp, err)
	}
}

// replaceFiles puts the new files in place, removes the obsolete ones and
// leaves no temporary file behind.
func TestReplaceFiles(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "x.md"), filepath.Join(dir, "x.vtt")
	write(t, a, "old a")
	write(t, a+".old.tmp", "an old file an earlier run could not remove")
	write(t, filepath.Join(dir, "Old title.md"), "old name")
	var pairs []filePair
	for _, f := range []string{a, b} { // b is new
		tmp, err := tmpFor(f)
		if err != nil {
			t.Fatal(err)
		}
		write(t, tmp, "new "+filepath.Base(f))
		pairs = append(pairs, filePair{tmp: tmp, final: f})
	}
	if err := replaceFiles(pairs, []string{filepath.Join(dir, "Old title.md")}); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, dir)
	if names(got) != "x.md,x.vtt" || got["x.md"] != "new x.md" || got["x.vtt"] != "new x.vtt" {
		t.Fatalf("dir after replace: %v", got)
	}
}

// replaceFile over a missing final is a rename.
func TestReplaceFileNew(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stream.mp4")
	write(t, out+".tmp", "new")
	if err := replaceFile(out+".tmp", out); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, dir); names(got) != "stream.mp4" || got["stream.mp4"] != "new" {
		t.Fatalf("dir: %v", got)
	}
}

// The coded error names the file only (test-chaos 3: no full path in the
// error the user sees).
func TestInUseCoded(t *testing.T) {
	err := inUseCoded(CodeExportFileInUse, &fileInUseError{name: "W01L1 Intro.bilingual.md", err: os.ErrPermission})
	if codeOf(err) != CodeExportFileInUse {
		t.Fatalf("code %q", codeOf(err))
	}
	info := errInfo(err)
	if info == nil || info.Params["name"] != "W01L1 Intro.bilingual.md" || strings.ContainsAny(detailOf(err), `/\`) || sched.IsPermanent(err) {
		t.Fatalf("params %v, detail %q", info.Params, detailOf(err))
	}
	plain := os.ErrNotExist
	if inUseCoded(CodePrepFileInUse, plain) != plain {
		t.Fatal("an error that is no fileInUseError must stay as it is")
	}
}
