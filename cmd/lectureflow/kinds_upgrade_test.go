// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// buildApp records, once, a renumbering notice for the courses whose
// exported citations the past-lecture rule renumbered (lecture kinds
// review, item 3): appended to the course's notices (kv renumbered_auto),
// none for a course never exported, nothing again on the next start.
func TestBuildAppRecordsPastRuleNotices(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg) // never the real API

	app, err := buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := app.Store
	// two courses with the same lectures: week 2 of the term and a
	// recording of the year before (W??-… before the rule, P1 now)
	mkCourse := func(name string) (model.Course, string) {
		t.Helper()
		c := model.Course{ID: db.NewID(), Name: name}
		if err := st.CreateCourse(ctx, c); err != nil {
			t.Fatal(err)
		}
		ts := "2026-09-28"
		if err := st.SetCourseCalendar(ctx, c.ID, &ts, nil); err != nil {
			t.Fatal(err)
		}
		var past string
		for _, at := range []string{"2026-10-06T09:00", "2025-10-07T09:00"} {
			now := db.Now()
			l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "L " + at, MediaPath: "media.mp4", LaunchedAt: &now}
			if err := st.CreateLecture(ctx, l); err != nil {
				t.Fatal(err)
			}
			at := at
			if err := st.SetLectureRecordedAt(ctx, l.ID, &at, model.RecordedAtFilename); err != nil {
				t.Fatal(err)
			}
			past = l.ID
		}
		return c, past
	}
	exported, pastID := mkCourse("Exported")
	other, _ := mkCourse("Never exported")
	if err := stages.MarkCiteExported(ctx, st, exported.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// a notice the title task recorded earlier stays first
	earlier := []stages.RenumberNotice{{ErrorInfo: model.ErrorInfo{Code: stages.CodeRenumberedAuto,
		Params: map[string]any{"lecture": "x", "n": 1, "changes": []any{}}}, At: "2026-10-20T10:00:00Z"}}
	b, _ := json.Marshal(earlier)
	if err := st.SetKV(ctx, stages.KVRenumberedAutoPrefix+exported.ID, string(b)); err != nil {
		t.Fatal(err)
	}
	// as if this database came from before the rule (kv has no delete)
	if err := st.SetKV(ctx, state.KVPastRuleChecked, ""); err != nil {
		t.Fatal(err)
	}
	app.Close()

	app, err = buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := stages.RenumberNotices(ctx, app.Store, exported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0].Code != stages.CodeRenumberedAuto || ns[1].Code != state.CodeRenumberedUpgrade || ns[1].At == "" {
		t.Fatalf("notices %+v", ns)
	}
	changes, _ := ns[1].Params["changes"].([]any)
	if len(changes) != 1 || ns[1].Params["n"] != float64(1) {
		t.Fatalf("params %+v", ns[1].Params)
	}
	if ch, _ := changes[0].(map[string]any); ch["lecture_id"] != pastID || ch["to"] != "P1" {
		t.Fatalf("change %+v", changes[0])
	}
	if ns, _ := stages.RenumberNotices(ctx, app.Store, other.ID); len(ns) != 0 {
		t.Fatalf("a course never exported got %+v", ns)
	}
	app.Close()

	// the next start does not record it again
	app, err = buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if ns, _ := stages.RenumberNotices(ctx, app.Store, exported.ID); len(ns) != 2 {
		t.Fatalf("second start: %d notices", len(ns))
	}
}
