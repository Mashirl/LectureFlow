// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
	"lectureflow/internal/state"
)

// quietLog drops the default logger's output for one test.
func quietLog(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// With the process's data directory on a share (state.SetDataDir,
// netpath-datadir), replacing a draft's media removes the old media inside
// it like on a local disk — no orphan is left on the share — while a stored
// path elsewhere (another computer, beside the data directory, a ".."
// escape) is still neither stat'ed nor removed. Nothing touches the disk:
// replacedMediaToRemove only decides (the share is TEST-NET-1). Outside
// Windows a share path is no data directory (filepath.Abs makes it
// relative), so nothing on the share is trusted or removed there.
func TestReplacedMediaToRemoveDataDirOnShare(t *testing.T) {
	quietLog(t)
	prev := state.TrustedDataDir()
	t.Cleanup(func() { state.SetDataDir(prev) })
	u := db.NewID()
	share := &Server{dataDir: `\\192.0.2.60\share\` + u + `\LF`}
	state.SetDataDir(share.dataDir)
	win := runtime.GOOS == "windows"

	l := model.Lecture{ID: db.NewID(), CourseID: "c1"}
	lecDir := `\\192.0.2.60\share\` + u + `\LF\courses\c1\lectures\` + l.ID
	newPath := lecDir + `\media.mkv`
	old := lecDir + `\media.mp4`
	type tcase struct{ media, want string }
	cases := []tcase{
		{newPath, ""},
		{`\\192.0.2.61\share\` + u + `\LF\courses\c1\media.mp4`, ""},     // another computer
		{`\\192.0.2.60\share\` + u + `\LF2\media.mp4`, ""},               // beside the data directory
		{`\\192.0.2.60\share\` + u + `\LF\..\..\` + u + `.mp4`, ""},      // ".." escape
		{`\\192.0.2.60\share\` + u + `\LF\courses\..\` + u + `.mp4`, ""}, // ".." at all
		{`\\.\UNC\192.0.2.60\share\` + u + `\LF\x.mp4`, ""},
		{"file://192.0.2.60/share/" + u + "/LF/x.mp4", ""},
	}
	slashed := `//192.0.2.60/share/` + u + `/LF/courses/c1/lectures/` + l.ID + `/media.mp4`
	if win {
		cases = append(cases, tcase{old, old}, tcase{slashed, slashed},
			tcase{"media.mp4", old}) // relative to the lecture directory
	} else {
		cases = append(cases, tcase{old, ""}, tcase{slashed, ""})
	}
	for _, tc := range cases {
		l.MediaPath = tc.media
		if got := share.replacedMediaToRemove(l, newPath); got != tc.want {
			t.Errorf("media_path %q: remove %q, want %q", tc.media, got, tc.want)
		}
	}
}

// The data import and the app at run time agree (one rule,
// media.ForeignStoredPath): with the imported data directory on a share,
// the import refuses exactly the stored paths StoredPathUnsafe would refuse
// with that directory as the data directory. The database is local; the
// paths in it are only read.
func TestImportPathsMatchRunTimeRule(t *testing.T) {
	quietLog(t)
	prev := state.TrustedDataDir()
	t.Cleanup(func() { state.SetDataDir(prev) })
	src := devDataDir(t)
	u := db.NewID()
	share := `\\192.0.2.62\share\` + u + `\LF`
	paths := []string{
		share + `\courses\C1\lectures\LA\media.mp4`,
		`//192.0.2.62/share/` + u + `/LF/courses/C1/lectures/LB/media.mp4`,
		`\\?\UNC\192.0.2.62\share\` + u + `\LF\courses\C1\lectures\LC\media.mp4`,
		`\\192.0.2.63\share\` + u + `\LF\courses\x.mp4`,
		share + `\..\..\` + u + `-esc\x.mp4`,
		share + `\courses\..\x.mp4`,
		`\\192.0.2.62\share\` + u + `\LF2\x.mp4`,
		`\\.\UNC\192.0.2.62\share\` + u + `\LF\x.mp4`,
		"file://192.0.2.62/share/" + u + "/LF/x.mp4",
		filepath.Join(src, "courses", "C1", "lectures", "LZ", "media.mp4"),
	}
	addStoredPaths(t, src, paths, share+`\courses\C1\materials\M1\slides.pdf`)
	dbPath := filepath.Join(src, relocate.DBName)

	state.SetDataDir(share)
	want := 0
	for _, p := range append([]string{share + `\courses\C1\materials\M1\slides.pdf`}, paths...) {
		if state.StoredPathUnsafe(state.PathLectureMedia, "x", p) {
			want++
		}
	}
	if runtime.GOOS == "windows" && want != 6 {
		t.Fatalf("setup: %d paths unsafe at run time, want 6", want)
	}
	got, _, err := foreignStoredPaths(context.Background(), dbPath, share)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("import refuses %d paths, the app at run time %d", got, want)
	}

	// a local data directory: every network path is refused, as before
	// (the 9 lectures and the material; not the two local media)
	if got, _, err := foreignStoredPaths(context.Background(), dbPath, src); err != nil || got != 10 {
		t.Errorf("import from a local directory: %d refused (%v), want 10", got, err)
	}
}
