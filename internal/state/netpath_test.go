// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// A media_path naming another computer (security review 2, M3) is a
// missing file for MediaExists: not stat'ed, logged once per path. Plain
// paths work as before. 192.0.2.0/24 is TEST-NET-1 (RFC 5737); the paths
// are unique per run because each is logged once per process.
func TestMediaExistsNetworkPath(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dataDir := t.TempDir()
	u := db.NewID()
	l := model.Lecture{ID: db.NewID(), CourseID: "c1"}
	for _, p := range []string{`\\192.0.2.1\share\` + u + `.mp4`, `//192.0.2.2/share/` + u + `.mp4`, `\\.\UNC\192.0.2.3\s\` + u + `.mp4`,
		`file://192.0.2.4/share/` + u + `.mp4`, `smb://192.0.2.5/share/` + u + `.mp4`} {
		l.MediaPath = p
		for range 3 {
			if MediaExists(dataDir, l) {
				t.Errorf("%s exists", p)
			}
		}
		if !StoredPathUnsafe(PathLectureMedia, l.ID, p) {
			t.Errorf("%s not unsafe", p)
		}
		if n := strings.Count(logBuf.String(), u+".mp4"); n != 1 {
			t.Errorf("%s logged %d times, want once", p, n)
		}
		logBuf.Reset()
	}

	// plain paths: absolute, relative to the lecture directory
	dir := model.LectureDir(dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "media.mp4")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{abs, "media.mp4"} {
		l.MediaPath = p
		if !MediaExists(dataDir, l) || StoredPathUnsafe(PathLectureMedia, l.ID, p) {
			t.Errorf("%s: plain path not found", p)
		}
	}
	if runtime.GOOS == "windows" {
		l.MediaPath = `\\?\` + abs // extended-length local path: still local
		if !MediaExists(dataDir, l) {
			t.Errorf("%s: local extended path not found", l.MediaPath)
		}
	} else {
		// a URL that is also a valid relative path here: still not opened
		rel := "file://192.0.2.6/" + u + ".mp4"
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		l.MediaPath = rel
		if MediaExists(dataDir, l) {
			t.Errorf("%s: a URL was opened as a relative path", rel)
		}
	}
	if logBuf.Len() != 0 && runtime.GOOS == "windows" {
		t.Errorf("plain paths logged: %s", logBuf.String())
	}
}
