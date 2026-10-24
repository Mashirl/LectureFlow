// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// v2 phase 2 startup steps (lead rulings 5 and the original_filename
// backfill of phase 2). Written before the implementation.

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

func TestBackfillOriginalFilenames(t *testing.T) {
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := model.Course{ID: db.NewID(), Name: "CS"}
	if err := st.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	mk := func(title, media, original string) string {
		t.Helper()
		l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: title, MediaPath: media, OriginalFilename: original}
		if err := st.CreateLecture(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	upload := mk("CSC1003_2026-09-30_1000", filepath.Join("data", "courses", c.ID, "lectures", "x", "media.mp4"), "")
	noMedia := mk("Week 2", "", "")
	kept := mk("Renamed by the user", "media.mov", "CSC1003_2026-10-07.mov")
	withExt := mk("clip.MP4", "media.mp4", "")

	n, err := backfillOriginalFilenames(ctx, st)
	if err != nil || n != 3 {
		t.Fatalf("backfill = %d, %v; want 3", n, err)
	}
	want := map[string]string{
		upload:  "CSC1003_2026-09-30_1000.mp4", // the upload's title is the file name without extension
		noMedia: "Week 2",
		kept:    "CSC1003_2026-10-07.mov", // a name recorded at upload is never replaced
		withExt: "clip.MP4",               // the title already ends with the extension
	}
	for id, w := range want {
		l, err := st.GetLecture(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if l.OriginalFilename != w {
			t.Errorf("%s: original_filename %q, want %q", l.Title, l.OriginalFilename, w)
		}
	}
	if v, ok, _ := st.GetKV(ctx, model.KVOriginalFilenames); !ok || v != "1" {
		t.Fatalf("kv marker = %q %v", v, ok)
	}
	// Once only: a lecture created later without a name is left alone.
	later := mk("Later", "media.mp4", "")
	if n, err := backfillOriginalFilenames(ctx, st); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v", n, err)
	}
	if l, _ := st.GetLecture(ctx, later); l.OriginalFilename != "" {
		t.Fatalf("second run wrote %q", l.OriginalFilename)
	}
}

// buildApp runs the one-time phase 2 conversions before the scheduler
// starts: stale_reason texts become JSON (P14.3) and lectures from before
// v2 get an original_filename.
func TestBuildAppRunsPhase2Conversions(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg) // never the real API

	app, err := buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := app.Store
	c := model.Course{ID: db.NewID(), Name: "CS"}
	if err := st.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := db.Now()
	l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "Week 1", MediaPath: "media.mkv", LaunchedAt: &now}
	if err := st.CreateLecture(ctx, l); err != nil {
		t.Fatal(err)
	}
	reason := "术语表已更新（v2 → v5）"
	repair, err := st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageRepair)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusDone}} {
		if err := state.TransitionJob(ctx, st, repair.ID, step[0], step[1], db.JobFinish{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.TransitionJob(ctx, st, repair.ID, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &reason}); err != nil {
		t.Fatal(err)
	}
	// As if this database came from before phase 2: the markers are unset
	// (kv has no delete; "" = not done).
	for _, k := range []string{model.KVStaleReasonJSON, model.KVOriginalFilenames} {
		if err := st.SetKV(ctx, k, ""); err != nil {
			t.Fatal(err)
		}
	}
	app.Close()

	app, err = buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	j, err := app.Store.GetJob(ctx, repair.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.StaleReasonJSON(model.StaleCodeGlossary, map[string]any{"from": 2, "to": 5})
	if j.StaleReason == nil || *j.StaleReason != want {
		t.Fatalf("stale_reason = %v, want %s", j.StaleReason, want)
	}
	got, err := app.Store.GetLecture(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginalFilename != "Week 1.mkv" {
		t.Fatalf("original_filename = %q", got.OriginalFilename)
	}
}
