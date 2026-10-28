// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// Week overrides (migration 011, course-ux): a new table; the lecture rows
// of 010 are read unchanged after the upgrade, and the CHECK constraints
// hold the week range and the two kinds.
func TestMigration011Upgrade(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 10); err != nil {
		t.Fatal(err)
	}
	old := New(d)
	c := model.Course{ID: NewID(), Name: "CSC3631"}
	if err := old.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	// a lecture written with the columns of 010 (the store of 011 would
	// read lecture_week_override, which does not exist yet)
	if _, err := d.ExecContext(ctx, `INSERT INTO lecture (id, course_id, title, media_path, created_at, seq, recorded_at, kind, kind_source)
		VALUES ('l1', ?, 'Stream Ciphers', 'm.mp4', '2026-10-06T12:00:00Z', 1, '2026-10-06T12:35', 'lecture', 'filename')`, c.ID); err != nil {
		t.Fatal(err)
	}
	// the store reads lectures of a database below 011 (no overrides)
	if l, err := old.GetLecture(ctx, "l1"); err != nil || l.WeekOverride != nil || l.Title != "Stream Ciphers" {
		t.Fatalf("GetLecture before 011: %+v %v", l, err)
	}
	if ls, err := old.ListLectures(ctx, c.ID); err != nil || len(ls) != 1 || ls[0].WeekOverride != nil {
		t.Fatalf("ListLectures before 011: %+v %v", ls, err)
	}
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v < 11 {
		t.Fatalf("user_version = %d, want >= 11", v)
	}
	st := New(d)
	l, err := st.GetLecture(ctx, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Title != "Stream Ciphers" || l.Kind != "lecture" || l.KindSource != "filename" || l.RecordedAt == nil || *l.RecordedAt != "2026-10-06T12:35" || l.WeekOverride != nil {
		t.Fatalf("lecture after upgrade: %+v", l)
	}
	for _, bad := range []string{
		`INSERT INTO lecture_week_override VALUES ('l1', 0, 'makeup', 'x')`,
		`INSERT INTO lecture_week_override VALUES ('l1', 61, 'makeup', 'x')`,
		`INSERT INTO lecture_week_override VALUES ('l1', 3, 'moved', 'x')`,
		`INSERT INTO lecture_week_override VALUES ('nope', 3, 'makeup', 'x')`, // foreign key
	} {
		if _, err := d.ExecContext(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

func TestWeekOverrideCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	a, b := mkLecture(t, st, c.ID), mkLecture(t, st, c.ID)

	if err := st.SetWeekOverride(ctx, a.ID, model.WeekOverride{Week: 3, Kind: model.WeekOverrideSupplement, CreatedAt: "2026-10-27T01:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetLecture(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := &model.WeekOverride{Week: 3, Kind: model.WeekOverrideSupplement, CreatedAt: "2026-10-27T01:00:00Z"}
	if !reflect.DeepEqual(got.WeekOverride, want) {
		t.Fatalf("GetLecture override %+v, want %+v", got.WeekOverride, want)
	}
	// the rest of the row is what it was
	got.WeekOverride = nil
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("lecture row changed:\n%+v\n%+v", got, a)
	}

	// replace (one row per lecture); CreatedAt "" → now
	if err := st.SetWeekOverride(ctx, a.ID, model.WeekOverride{Week: 5, Kind: model.WeekOverrideMakeup}); err != nil {
		t.Fatal(err)
	}
	ls, err := st.ListLectures(ctx, c.ID)
	if err != nil || len(ls) != 2 {
		t.Fatalf("ListLectures %v %v", ls, err)
	}
	byID := map[string]model.Lecture{ls[0].ID: ls[0], ls[1].ID: ls[1]}
	if o := byID[a.ID].WeekOverride; o == nil || o.Week != 5 || o.Kind != model.WeekOverrideMakeup || o.CreatedAt == "" || o.CreatedAt == want.CreatedAt {
		t.Fatalf("replaced override %+v", o)
	}
	if byID[b.ID].WeekOverride != nil {
		t.Fatalf("other lecture has %+v", byID[b.ID].WeekOverride)
	}
	var n int
	st.(*sqlStore).q.QueryRowContext(ctx, `SELECT count(*) FROM lecture_week_override`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}

	// invalid input, missing lecture
	for _, o := range []model.WeekOverride{{Week: 0, Kind: model.WeekOverrideMakeup}, {Week: model.MaxBindWeek + 1, Kind: model.WeekOverrideMakeup}, {Week: 2, Kind: "moved"}} {
		if err := st.SetWeekOverride(ctx, b.ID, o); !errors.Is(err, ErrWeekOverrideInvalid) {
			t.Errorf("%+v: err %v, want ErrWeekOverrideInvalid", o, err)
		}
	}
	if err := st.SetWeekOverride(ctx, "missing", model.WeekOverride{Week: 2, Kind: model.WeekOverrideMakeup}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lecture: %v", err)
	}

	// delete
	if err := st.DeleteWeekOverride(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWeekOverride(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if got, _ := st.GetLecture(ctx, a.ID); got.WeekOverride != nil {
		t.Fatalf("still has %+v", got.WeekOverride)
	}
}

// The override is deleted with its lecture (ON DELETE CASCADE), also when
// the course goes.
func TestWeekOverrideCascade(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	a, b := mkLecture(t, st, c.ID), mkLecture(t, st, c.ID)
	for _, id := range []string{a.ID, b.ID} {
		if err := st.SetWeekOverride(ctx, id, model.WeekOverride{Week: 2, Kind: model.WeekOverrideMakeup}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		if err := st.(*sqlStore).q.QueryRowContext(ctx, `SELECT count(*) FROM lecture_week_override`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := st.DeleteLecture(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after deleting a lecture: %d rows, want 1", n)
	}
	if _, err := st.(*sqlStore).q.ExecContext(ctx, `DELETE FROM course WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after deleting the course: %d rows, want 0", n)
	}
}
