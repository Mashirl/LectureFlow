// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// buildApp records the data directory as the trusted root for stored paths
// (netpath-datadir) before anything reads one: the startup scan, the
// listings and the stages then let the paths inside it through.
func TestBuildAppSetsDataDir(t *testing.T) {
	prev := state.TrustedDataDir()
	t.Cleanup(func() { state.SetDataDir(prev) })
	state.SetDataDir("")

	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg) // never the real API
	app, err := buildApp(context.Background(), cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	want, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.TrustedDataDir(); got != want {
		t.Errorf("trusted data directory %q, want %q", got, want)
	}
}

// With the data directory on a network share the startup scan counts only
// the paths outside it (another computer, beside it on the share, a ".."
// escape); the paths inside it, in either spelling, are the user's own and
// are neither counted nor logged. Outside Windows a share path is no data
// directory (filepath.Abs makes it relative), so nothing is trusted there
// and every network path counts. Nothing is opened: 192.0.2.0/24 is
// TEST-NET-1 (RFC 5737), the paths are unique per run.
func TestScanNetworkPathsDataDirOnShare(t *testing.T) {
	var logBuf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	prev := state.TrustedDataDir()
	t.Cleanup(func() { state.SetDataDir(prev) })

	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	u := db.NewID()
	share := `\\192.0.2.50\share\` + u + `\LF`
	state.SetDataDir(share)
	c1, c2 := model.Course{ID: db.NewID(), Name: "A"}, model.Course{ID: db.NewID(), Name: "B"}
	must(st.CreateCourse(ctx, c1))
	must(st.CreateCourse(ctx, c2))
	inside := model.Lecture{ID: db.NewID(), CourseID: c1.ID, Title: "in", MediaPath: model.LectureDir(share, c1.ID, "l1") + `\in-1.mp4`}
	for _, l := range []model.Lecture{
		inside,
		{ID: db.NewID(), CourseID: c2.ID, Title: "in2", MediaPath: `\\?\UNC\192.0.2.50\share\` + u + `\LF\courses\c2\lectures\l2\in-2.mp4`},
		{ID: db.NewID(), CourseID: c1.ID, Title: "out", MediaPath: `\\192.0.2.51\share\` + u + `\LF\out-1.mp4`},
		{ID: db.NewID(), CourseID: c2.ID, Title: "esc", MediaPath: share + `\..\..\` + u + `-esc\out-2.mp4`},
		{ID: db.NewID(), CourseID: c1.ID, Title: "local", MediaPath: filepath.Join(dir, "local.mp4")},
	} {
		must(st.CreateLecture(ctx, l))
	}
	must(st.CreateMaterial(ctx, model.Material{ID: db.NewID(), CourseID: c1.ID, Filename: "s.pdf", Path: `//192.0.2.50/share/` + u + `/LF/courses/c1/materials/m1/in-3.pdf`}))
	must(st.CreateMaterial(ctx, model.Material{ID: db.NewID(), CourseID: c1.ID, Filename: "t.pdf", Path: `\\192.0.2.50\share\` + u + `\LF2\out-3.pdf`}))
	must(st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c1.ID, Filename: "syl.pdf", Path: share + `\courses\c1\syllabus\in-4.pdf`}))
	must(st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c2.ID, Filename: "syl.pdf", Path: `\\192.0.2.50\share\` + u + `\out-4.pdf`}))

	n := scanNetworkPaths(ctx, st)
	want := networkPaths{lectures: 2, materials: 1, syllabi: 1}
	if runtime.GOOS != "windows" {
		want = networkPaths{lectures: 4, materials: 2, syllabi: 2}
	}
	if n != want {
		t.Fatalf("scan %+v, want %+v", n, want)
	}
	out := logBuf.String()
	wantIn := 0 // inside paths: never logged on Windows, once each elsewhere
	if runtime.GOOS != "windows" {
		wantIn = 1
	}
	for i := 1; i <= 4; i++ {
		if c := strings.Count(out, "out-"+string(rune('0'+i))); c != 1 {
			t.Errorf("outside path %d logged %d times:\n%s", i, c, out)
		}
		if c := strings.Count(out, "in-"+string(rune('0'+i))); c != wantIn {
			t.Errorf("inside path %d logged %d times, want %d:\n%s", i, c, wantIn, out)
		}
	}
}
