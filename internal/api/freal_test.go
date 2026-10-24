// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Regression tests for issues found by running the front end against a
// real back end (F-real, v1.1).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// A deleted lecture is announced as `changed {kind: lecture, id}` (single
// and batch delete), so a page showing that lecture (another tab) reloads
// it, gets 404 and shows "找不到这节讲座" instead of stale data.
func TestDeleteLectureAnnouncesLectureChange(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l1 := e.draft(c.ID, "one")
	l2 := e.draft(c.ID, "two")
	r := openSSE(t, e.ts.URL+"/api/events")
	waitClients(t, e.hub, 1)

	isLectureChange := func(id string) func(sseFrame) bool {
		return func(f sseFrame) bool {
			if f.event != "changed" {
				return false
			}
			var ev ChangedEvent
			return json.Unmarshal([]byte(f.data), &ev) == nil && ev.Kind == "lecture" && ev.ID == id
		}
	}

	e.call("DELETE", "/api/lectures/"+l1.ID, nil, http.StatusNoContent, nil)
	r.next(3*time.Second, isLectureChange(l1.ID))

	var br BatchResponse
	e.call("POST", "/api/lectures/delete", BatchRequest{IDs: []string{l2.ID}}, http.StatusOK, &br)
	if len(br.Results) != 1 || !br.Results[0].OK {
		t.Fatalf("batch delete: %+v", br.Results)
	}
	r.next(3*time.Second, isLectureChange(l2.ID))
	e.call("GET", "/api/lectures/"+l2.ID, nil, http.StatusNotFound, nil)
}

// can_delete must agree with Scheduler.DeleteLecture, which also counts the
// jobs of materials bound to the lecture (they are deleted with it): a draft
// whose bound material is being extracted needs force, so the UI must see
// can_delete=false and send it (A7; spec review after F-real).
func TestCanDeleteCountsBoundMaterials(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.draft(c.ID, "one")
	res, b := e.upload("/api/courses/"+c.ID+"/materials", [][2]string{{"lecture_id", l.ID}}, "w1.pdf", []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("material: %d %s", res.StatusCode, b)
	}
	var cv Course
	json.Unmarshal(b, &cv)
	m := cv.Materials[0]

	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if !lv.CanDelete {
		t.Fatal("nothing runs yet: can_delete must be true")
	}
	e.moveJob(model.ScopeMaterial, m.ID, model.StageMaterialExtract, model.StatusQueued, model.StatusRunning)
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if lv.CanDelete {
		t.Error("a bound material is running: can_delete must be false")
	}
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].CanDelete {
		t.Errorf("list view must agree: %+v", list)
	}
	// without force the back end refuses, as can_delete said (the force path,
	// which cancels the bound material's job first, is Scheduler.DeleteLecture's)
	e.callErr("DELETE", "/api/lectures/"+l.ID, nil, http.StatusConflict)
}
