// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"testing"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

func (e *qenv) mkLecture(courseID, title, filename string) string {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: courseID, Title: title, OriginalFilename: filename,
		MediaPath: "media.mp4", TitleSource: model.TitleSourceFilename}
	if err := e.st.CreateLecture(context.Background(), l); err != nil {
		e.t.Fatal(err)
	}
	return l.ID
}

func checkByID(items []bridge.CheckItem) map[string]bridge.CheckItem {
	out := map[string]bridge.CheckItem{}
	for _, it := range items {
		out[it.SessionID] = it
	}
	return out
}

// "更新 / 添加这门课程" (the user, 2026-10-27): sessions without a download
// task are deduplicated by title against the lectures of the course.
func TestCheckByTitle(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	other := e.mkCourse("CSC2001")
	// a lecture uploaded from Panopto's own download: "/" and ":" became "-"
	byFile := e.mkLecture(e.course, "Lecture 1", "CSC1003-L01-01 Wed Oct 21 09-35-00 BST 2026.mp4")
	byTitle := e.mkLecture(e.course, "Graph  Theory: Intro_Part 2", "rec.m4a")
	elsewhere := e.mkLecture(other, "Week 9 Revision", "")
	e.mkLecture(e.course, "---", "") // a title that normalises to nothing
	e.mkLecture(e.course, "Lecture 3.5 notes", "Lecture 3.5 notes")

	req := &bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "c"}, Platform: "panopto", CourseID: e.course,
		SessionIDs: []string{"s-file", "s-title", "s-other-course", "s-empty", "s-underscores", "s-new", "s-dots"},
		Titles: []string{"CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026", "  graph theory - intro part\t2 ", "Week 9 Revision",
			"", "___", "Lecture 4", "lecture 3.5 notes"}}
	rep, err := e.q.Check(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got := checkByID(rep.Items)
	want := map[string]string{"s-file": byFile, "s-title": byTitle, "s-dots": ""}
	if len(got) != len(want) {
		t.Fatalf("items %+v", rep.Items)
	}
	for sid, lec := range want {
		it, ok := got[sid]
		if !ok || it.State != bridge.StateImported || it.MatchedBy != "title" || it.CourseID != e.course || it.TaskID != 0 ||
			(lec != "" && it.LectureID != lec) {
			t.Errorf("%s: %+v", sid, it)
		}
	}
	// every course: the other course's lecture matches too
	req.CourseID = ""
	rep, _ = e.q.Check(ctx, req)
	if it := checkByID(rep.Items)["s-other-course"]; it.LectureID != elsewhere || it.CourseID != other || it.MatchedBy != "title" {
		t.Fatalf("every course %+v", rep.Items)
	}
	// without titles (older extensions): no title match
	rep, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s-file"}})
	if len(rep.Items) != 0 {
		t.Fatalf("no titles %+v", rep.Items)
	}
	// fewer titles than sessions: the rest have none; more: refused
	rep, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s-title", "s-file"},
		Titles: []string{"Graph Theory Intro Part 2"}})
	if len(rep.Items) != 1 || rep.Items[0].SessionID != "s-title" {
		t.Fatalf("short titles %+v", rep.Items)
	}
	if _, err := e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"a"}, Titles: []string{"x", "y"}}); bridgeCode(err) != bridge.CodeBadRequest {
		t.Fatalf("too many titles: %v", err)
	}
}

// A session's own download task wins over a title match.
func TestCheckSessionBeforeTitle(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.mkLecture(e.course, "Lecture 3: Graphs / Trees", "")
	e.media.put("/files/a.m4a", pattern(1, 1000))
	done := e.wait(e.enqueue(e.request("s-done", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
	e.media.setStatus("/files/b.m4a", 500)
	failed := e.enqueue(e.request("s-failed", e.fileTrack(bridge.KindAudio, "/files/b.m4a")))
	e.wait(failed, bridge.StateFailed)
	title := "Lecture 3: Graphs / Trees" // also the title of the lecture made beforehand
	rep, err := e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", CourseID: e.course,
		SessionIDs: []string{"s-done", "s-failed"}, Titles: []string{title, title}})
	if err != nil {
		t.Fatal(err)
	}
	got := checkByID(rep.Items)
	if it := got["s-done"]; it.MatchedBy != "session" || it.State != bridge.StateImported || it.LectureID != done.LectureID || it.TaskID != done.ID {
		t.Fatalf("done %+v", it)
	}
	if it := got["s-failed"]; it.MatchedBy != "session" || it.State != bridge.StateFailed || it.TaskID != failed || it.LectureID != "" {
		t.Fatalf("failed %+v", it)
	}
	// the downloaded lecture deleted: the session is no longer imported by
	// its task, but the other lecture of that title still counts
	if err := e.st.DeleteLecture(ctx, done.LectureID); err != nil {
		t.Fatal(err)
	}
	rep, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", CourseID: e.course, SessionIDs: []string{"s-done"}, Titles: []string{title}})
	if len(rep.Items) != 1 || rep.Items[0].MatchedBy != "title" || rep.Items[0].LectureID == done.LectureID {
		t.Fatalf("after the delete %+v", rep.Items)
	}
}

func TestTitleKey(t *testing.T) {
	for in, want := range map[string]string{
		"CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026": "csc1003 l01 01 wed oct 21 09 35 00 bst 2026",
		"CSC1003-L01-01 Wed Oct 21 09-35-00 BST 2026": "csc1003 l01 01 wed oct 21 09 35 00 bst 2026",
		` A\B :: c__d `: "a b c d",
		" \t-_/ ":       "",
		"Ünïcode  Тема": "ünïcode тема",
	} {
		if got := titleKey(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"a.mp4": "a", "a.b.MKV": "a.b", "Lecture 3.5 notes": "Lecture 3.5 notes", "noext": "noext", "x.verylong": "x.verylong"} {
		if got := stripExt(in); got != want {
			t.Errorf("stripExt(%q) = %q", in, got)
		}
	}
}
