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

// useDataDir sets the trusted data directory for one test. Outside Windows
// a share path is no data directory (filepath.Abs makes a \\nas\… path
// relative and //nas/… a local one), so it is stored as is there: the rule
// under test (media.ForeignStoredPath) is the same on every system.
func useDataDir(t *testing.T, dir string) {
	t.Helper()
	prev := trustedDataDir.Load()
	t.Cleanup(func() { trustedDataDir.Store(prev) })
	if runtime.GOOS == "windows" {
		SetDataDir(dir)
		return
	}
	trustedDataDir.Store(&dir)
}

// captureLog sends the default logger to a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A data directory on a network share (netpath-datadir): the stored paths
// inside it are the user's own and pass StoredPathUnsafe (nothing logged),
// in either spelling (\\ or \\?\UNC\, / or \); paths on other computers,
// elsewhere on the same share, or escaping with ".." stay unsafe, logged
// once each. Nothing is stat'ed here: only the guard is checked, the share
// is TEST-NET-1 (RFC 5737) and the paths are unique per run.
func TestStoredPathUnsafeDataDirOnShare(t *testing.T) {
	logBuf := captureLog(t)
	u := db.NewID()
	for _, root := range []string{`\\192.0.2.40\share\` + u + `\LF`, `\\?\UNC\192.0.2.40\share\` + u + `\LF`, `//192.0.2.40/share/` + u + `/LF/`} {
		useDataDir(t, root)
		for _, p := range []string{
			`\\192.0.2.40\share\` + u + `\LF\courses\c1\lectures\l1\media.mp4`,
			`//192.0.2.40/share/` + u + `/LF/courses/c1/lectures/l1/media.mp4`,
			`\\?\UNC\192.0.2.40\share\` + u + `\LF\courses\c1\lectures\l1\media.mp4`,
			`\\192.0.2.40\share\` + u + `\LF\courses\c1\materials\m1\slides.pdf`,
		} {
			for _, what := range []string{PathLectureMedia, PathMaterial, PathSyllabus} {
				if StoredPathUnsafe(what, "x", p) {
					t.Errorf("data directory %s: %s inside it is unsafe", root, p)
				}
			}
		}
		if logBuf.Len() != 0 {
			t.Errorf("data directory %s: paths inside it logged:\n%s", root, logBuf.String())
		}
	}

	useDataDir(t, `\\192.0.2.40\share\`+u+`\LF`)
	for i, p := range []string{
		`\\192.0.2.41\share\` + u + `\LF\x-0.mp4`,              // another computer
		`\\192.0.2.40\share\` + u + `\LF2\x-1.mp4`,             // beside the data directory
		`\\192.0.2.40\share\` + u + `\x-2.mp4`,                 // above it
		`\\192.0.2.40\share\` + u + `\LF\..\..\evil\s\x-3.mp4`, // ".." escape
		`\\?\UNC\192.0.2.40\share\` + u + `\LF\..\..\..\192.0.2.42\s\x-4.mp4`,
		`\\.\UNC\192.0.2.40\share\` + u + `\LF\..\..\..\192.0.2.42\s\x-5.mp4`,
		`\\192.0.2.40\share\` + u + `\LF\courses\..\x-6.mp4`, // ".." even when it would stay inside
		`file://192.0.2.40/share/` + u + `/LF/x-7.mp4`,
		`  \\192.0.2.40\share\` + u + `\LF\x-8.mp4`,
	} {
		name := "x-" + string(rune('0'+i)) + ".mp4" // the log quotes the path's backslashes
		for range 3 {
			if !StoredPathUnsafe(PathLectureMedia, "x", p) {
				t.Errorf("%s outside the data directory is not unsafe", p)
			}
		}
		if n := strings.Count(logBuf.String(), name); n != 1 {
			t.Errorf("%s logged %d times, want once:\n%s", p, n, logBuf.String())
		}
	}

	// MediaExists refuses an outside one without a stat (the guard comes first)
	l := model.Lecture{ID: db.NewID(), CourseID: "c1", MediaPath: `\\192.0.2.41\share\` + u + `\LF\x-0.mp4`}
	if MediaExists(t.TempDir(), l) {
		t.Error("media on another computer exists")
	}
}

// A local data directory changes nothing: network paths stay unsafe, local
// ones (also outside the data directory) are found as before.
func TestStoredPathUnsafeLocalDataDir(t *testing.T) {
	logBuf := captureLog(t)
	dataDir := t.TempDir()
	useDataDir(t, dataDir)
	u := db.NewID()
	for _, p := range []string{`\\192.0.2.43\share\` + u + `-a.mp4`, `//192.0.2.43/share/` + u + `-b.mp4`,
		`\\?\UNC\192.0.2.43\share\` + u + `-c.mp4`, `file://192.0.2.43/share/` + u + `-d.mp4`} {
		l := model.Lecture{ID: db.NewID(), CourseID: "c1", MediaPath: p}
		if MediaExists(dataDir, l) || !StoredPathUnsafe(PathLectureMedia, l.ID, p) {
			t.Errorf("%s: not refused with a local data directory", p)
		}
	}
	if n := strings.Count(logBuf.String(), u); n != 4 {
		t.Errorf("want 4 Warn lines, got %d:\n%s", n, logBuf.String())
	}
	logBuf.Reset()

	l := model.Lecture{ID: db.NewID(), CourseID: "c1"}
	dir := model.LectureDir(dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "media.mp4")
	outside := filepath.Join(t.TempDir(), "orig.mp4") // the user's original (A7)
	for _, p := range []string{inside, outside} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		l.MediaPath = p
		if !MediaExists(dataDir, l) || StoredPathUnsafe(PathLectureMedia, l.ID, p) {
			t.Errorf("%s: local path not found", p)
		}
	}
	l.MediaPath = "media.mp4"
	if !MediaExists(dataDir, l) {
		t.Error("relative media path not found")
	}
	if runtime.GOOS == "windows" {
		l.MediaPath = `\\?\` + inside // extended-length local path: still local
		if !MediaExists(dataDir, l) {
			t.Errorf("%s: local extended path not found", l.MediaPath)
		}
	}
	if logBuf.Len() != 0 {
		t.Errorf("local paths logged:\n%s", logBuf.String())
	}
}

// SetDataDir makes the directory absolute (lexically); "" trusts nothing.
// On Windows a share is trusted in any spelling.
func TestSetDataDir(t *testing.T) {
	captureLog(t)
	prev := trustedDataDir.Load()
	t.Cleanup(func() { trustedDataDir.Store(prev) })
	SetDataDir("data")
	want, err := filepath.Abs("data")
	if err != nil {
		t.Fatal(err)
	}
	if got := TrustedDataDir(); got != want {
		t.Errorf("TrustedDataDir() = %q, want %q", got, want)
	}
	u := db.NewID()
	if runtime.GOOS == "windows" {
		for _, root := range []string{`\\192.0.2.45\share\` + u + `\LF\`, `//192.0.2.45/share/` + u + `/LF`, `\\?\UNC\192.0.2.45\share\` + u + `\LF`} {
			SetDataDir(root)
			if p := `\\192.0.2.45\share\` + u + `\LF\courses\x.mp4`; StoredPathUnsafe(PathLectureMedia, "l", p) {
				t.Errorf("data directory %s (recorded as %s): %s is unsafe", root, TrustedDataDir(), p)
			}
		}
	}
	SetDataDir("")
	if got := TrustedDataDir(); got != "" {
		t.Errorf("TrustedDataDir() = %q after SetDataDir(\"\")", got)
	}
	if !StoredPathUnsafe(PathMaterial, "m", `\\192.0.2.45\share\`+u+`\LF\courses\x.pdf`) {
		t.Error("no data directory: a share path is not unsafe")
	}
}
