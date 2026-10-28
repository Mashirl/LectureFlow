// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// The startup scan counts the stored paths that name another computer
// (security review 2, M3), logs the numbers and each path once; the
// lecture listing then treats such media as missing without logging it
// again. 192.0.2.0/24 is TEST-NET-1 (RFC 5737); the paths are unique per
// run because each is logged once per process.
func TestScanNetworkPaths(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if n := scanNetworkPaths(ctx, st); n != (networkPaths{}) {
		t.Fatalf("empty database: %+v", n)
	}
	if !strings.Contains(logBuf.String(), "no stored path points to another computer") {
		t.Errorf("no count logged:\n%s", logBuf.String())
	}
	logBuf.Reset()

	u := db.NewID()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	c1, c2 := model.Course{ID: db.NewID(), Name: "A"}, model.Course{ID: db.NewID(), Name: "B"}
	must(st.CreateCourse(ctx, c1))
	must(st.CreateCourse(ctx, c2))
	net1 := model.Lecture{ID: db.NewID(), CourseID: c1.ID, Title: "n1", MediaPath: `\\192.0.2.30\share\` + u + `-1.mp4`}
	lecs := []model.Lecture{net1,
		{ID: db.NewID(), CourseID: c2.ID, Title: "n2", MediaPath: "file://192.0.2.31/" + u + "-2.mp4"},
		{ID: db.NewID(), CourseID: c1.ID, Title: "local", MediaPath: filepath.Join(dir, "local.mp4")},
		{ID: db.NewID(), CourseID: c2.ID, Title: "none"}}
	for _, l := range lecs {
		must(st.CreateLecture(ctx, l))
	}
	must(st.CreateMaterial(ctx, model.Material{ID: db.NewID(), CourseID: c1.ID, Filename: "s.pdf", Path: `//192.0.2.32/share/` + u + `-3.pdf`}))
	must(st.CreateMaterial(ctx, model.Material{ID: db.NewID(), CourseID: c1.ID, Filename: "t.pdf", Path: filepath.Join(dir, "t.pdf")}))
	must(st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c2.ID, Filename: "syl.pdf", Path: `\\192.0.2.33\share\` + u + `-4.pdf`}))
	must(st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c1.ID, Filename: "syl.txt", Path: filepath.Join(dir, "syl.txt")}))

	n := scanNetworkPaths(ctx, st)
	if want := (networkPaths{lectures: 2, materials: 1, syllabi: 1}); n != want {
		t.Fatalf("scan %+v, want %+v", n, want)
	}
	out := logBuf.String()
	if !strings.Contains(out, "total=4 lectures=2 materials=1 syllabi=1") {
		t.Errorf("counts not logged:\n%s", out)
	}
	for i := 1; i <= 4; i++ {
		if c := strings.Count(out, u+"-"+string(rune('0'+i))); c != 1 {
			t.Errorf("path %d logged %d times:\n%s", i, c, out)
		}
	}
	logBuf.Reset()
	if state.MediaExists(dir, net1) {
		t.Error("network media exists")
	}
	if strings.Contains(logBuf.String(), u) {
		t.Errorf("logged again after the scan:\n%s", logBuf.String())
	}
}
