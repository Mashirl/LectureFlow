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

func downloadStore(t *testing.T) (*sqlStore, string) {
	t.Helper()
	st := openTest(t)
	c := mkCourse(t, st)
	return st.(*sqlStore), c.ID
}

func sampleTask(courseID, session string) DownloadTask {
	return DownloadTask{
		Platform: "panopto", SessionID: session, ProfileID: "panopto", ProfileVersion: "1.0.0",
		CourseID: courseID, Title: "Lecture 3", RecordedAt: sp("2026-10-20T10:00"), DurationSec: 3000,
		PageHost:   "x.example.edu",
		TracksJSON: `[{"kind":"audio","format":"file","role":"primary-audio","label":"Lecture audio","host":"cdn.example.edu","path":"/a.mp4"}]`,
		State:      "queued", BytesTotal: -1,
	}
}

func TestMigration010Upgrade(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO course (id, name, created_at) VALUES ('c1','CSC1003','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v < 10 {
		t.Fatalf("user_version = %d, want >= 10", v)
	}
	st := New(d).(*sqlStore)
	id, err := st.InsertDownloadTask(ctx, sampleTask("c1", "s1"))
	if err != nil || id <= 0 {
		t.Fatalf("insert after upgrade: %d %v", id, err)
	}
	var name string
	if err := d.QueryRow(`SELECT name FROM course WHERE id = 'c1'`).Scan(&name); err != nil || name != "CSC1003" {
		t.Fatalf("course row changed: %q %v", name, err)
	}
}

func TestDownloadTaskCRUD(t *testing.T) {
	ctx := context.Background()
	st, course := downloadStore(t)
	in := sampleTask(course, "s1")
	id, err := st.InsertDownloadTask(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDownloadTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	in.ID, in.CreatedAt, in.UpdatedAt = id, got.CreatedAt, got.UpdatedAt
	if got.CreatedAt == "" || got.UpdatedAt != got.CreatedAt {
		t.Fatalf("times %q %q", got.CreatedAt, got.UpdatedAt)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, in)
	}
	if f, err := st.FindDownloadTask(ctx, "panopto", "s1"); err != nil || f.ID != id {
		t.Fatalf("find: %+v %v", f, err)
	}
	if _, err := st.FindDownloadTask(ctx, "generic", "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find other platform: %v", err)
	}
	if _, err := st.InsertDownloadTask(ctx, sampleTask(course, "s1")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same session again: %v", err)
	}
	// the same session id of another platform is another task
	if _, err := st.InsertDownloadTask(ctx, DownloadTask{Platform: "generic", SessionID: "s1", ProfileID: "generic",
		CourseID: course, Title: "x", TracksJSON: "[]", State: "queued", BytesTotal: -1}); err != nil {
		t.Fatal(err)
	}

	got.State, got.ErrorCode, got.ErrorParams = "failed", "download.http_status", `{"status":500}`
	got.BytesDone, got.BytesTotal, got.UpdatedAt = 10, 100, "2026-10-26T12:00:00Z"
	got.OverrideDisabled, got.RecordedAt = true, nil
	if err := st.UpdateDownloadTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetDownloadTask(ctx, id)
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("update\n got %+v\nwant %+v", again, got)
	}
	if err := st.SetDownloadProgress(ctx, id, 50, 200, ""); err != nil {
		t.Fatal(err)
	}
	again, _ = st.GetDownloadTask(ctx, id)
	if again.BytesDone != 50 || again.BytesTotal != 200 || again.UpdatedAt == got.UpdatedAt {
		t.Fatalf("progress %+v", again)
	}
	list, err := st.ListDownloadTasks(ctx)
	if err != nil || len(list) != 2 || list[0].ID != id {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := st.DeleteDownloadTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDownloadTask(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := st.DeleteDownloadTask(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := st.UpdateDownloadTask(ctx, got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update deleted: %v", err)
	}
	// ids are not reused (AUTOINCREMENT): the extension may still hold one
	id2, err := st.InsertDownloadTask(ctx, sampleTask(course, "s9"))
	if err != nil || id2 <= id+1 {
		t.Fatalf("new id %d after deleting %d: %v", id2, id, err)
	}
}

func TestDownloadTaskStateCheck(t *testing.T) {
	st, course := downloadStore(t)
	bad := sampleTask(course, "s1")
	bad.State = "imported"
	if _, err := st.InsertDownloadTask(context.Background(), bad); err == nil {
		t.Fatal("state outside the CHECK accepted")
	}
	for _, s := range DownloadStates {
		ok := sampleTask(course, "s-"+s)
		ok.State = s
		if _, err := st.InsertDownloadTask(context.Background(), ok); err != nil {
			t.Fatalf("state %s: %v", s, err)
		}
	}
}

// A deleted lecture leaves the task (lecture_id NULL: it may be sent
// again); a deleted course takes its tasks along.
func TestDownloadTaskForeignKeys(t *testing.T) {
	ctx := context.Background()
	st, course := downloadStore(t)
	l := mkLecture(t, st, course)
	tk := sampleTask(course, "s1")
	tk.State, tk.LectureID = "done", &l.ID
	id, err := st.InsertDownloadTask(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLecture(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDownloadTask(ctx, id)
	if err != nil || got.LectureID != nil || got.State != "done" {
		t.Fatalf("after lecture delete: %+v %v", got, err)
	}
	bad := sampleTask("no-such-course", "s2")
	if _, err := st.InsertDownloadTask(ctx, bad); err == nil {
		t.Fatal("task of a missing course accepted")
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM course WHERE id = ?`, course); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDownloadTask(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after course delete: %v", err)
	}
}

func TestResetUnfinishedDownloads(t *testing.T) {
	ctx := context.Background()
	st, course := downloadStore(t)
	ids := map[string]int64{}
	for _, s := range DownloadStates {
		tk := sampleTask(course, "s-"+s)
		tk.State = s
		if s == "failed" {
			tk.ErrorCode = "download.network"
		}
		id, err := st.InsertDownloadTask(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		ids[s] = id
	}
	n, err := st.ResetUnfinishedDownloads(ctx, "download.restarted", "")
	if err != nil || n != 4 {
		t.Fatalf("reset %d %v", n, err)
	}
	for s, id := range ids {
		got, _ := st.GetDownloadTask(ctx, id)
		switch s {
		case "queued", "downloading", "merging", "importing":
			if got.State != "needs_refresh" || got.ErrorCode != "download.restarted" {
				t.Errorf("%s → %+v", s, got)
			}
		default:
			if got.State != s {
				t.Errorf("%s changed to %s", s, got.State)
			}
		}
	}
	if got, _ := st.GetDownloadTask(ctx, ids["failed"]); got.ErrorCode != "download.network" {
		t.Errorf("failed task's code changed: %q", got.ErrorCode)
	}
}

func TestListLectureNames(t *testing.T) {
	ctx := context.Background()
	st, course := downloadStore(t)
	other := mkCourse(t, st).ID
	a := model.Lecture{ID: NewID(), CourseID: course, Title: "Week 1", OriginalFilename: "CSC1003-L01-01.mp4", MediaPath: "m.mp4"}
	b := model.Lecture{ID: NewID(), CourseID: other, Title: "Week 2", MediaPath: "m.mp4"}
	for _, l := range []model.Lecture{a, b} {
		if err := st.CreateLecture(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListLectureNames(ctx, course)
	if err != nil || len(got) != 1 || got[0] != (LectureName{ID: a.ID, CourseID: course, Title: "Week 1", OriginalFilename: "CSC1003-L01-01.mp4"}) {
		t.Fatalf("one course: %+v %v", got, err)
	}
	all, err := st.ListLectureNames(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("every course: %+v %v", all, err)
	}
	none, err := st.ListLectureNames(ctx, "01NOSUCHCOURSE")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("unknown course: %+v %v", none, err)
	}
}
