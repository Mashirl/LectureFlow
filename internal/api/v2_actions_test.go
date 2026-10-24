// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"testing"

	"lectureflow/internal/model"
)

// SPEC-v2 P13.5 "regenerate title and blurb" (fe-fix, Gate round 1): the back
// end offers it in lecture_actions exactly when POST /api/lectures/{id}/title
// would accept it — the summary is done or stale and the organizer role is
// configured — so the front end renders the menu item without guessing.
func TestLectureActionRegenerateTitle(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	offered := func(l Lecture) bool {
		for _, a := range l.LectureActions {
			if a == model.LectureActionRegenerateTitle {
				return true
			}
		}
		return false
	}
	inList := func(id string) bool {
		var list []Lecture
		e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
		for _, l := range list {
			if l.ID == id {
				return offered(l)
			}
		}
		t.Fatalf("lecture %s not in the course list", id)
		return false
	}

	l := e.lecture(c.ID, "Week 3")
	e.allDone(l.ID)
	if offered(e.lectureView(l.ID)) || inList(l.ID) {
		t.Fatalf("offered without the organizer role: %v", e.lectureView(l.ID).LectureActions)
	}
	if info := e.callErrInfo("POST", "/api/lectures/"+l.ID+"/title", nil, http.StatusConflict); info.Code != "title.not_configured" {
		t.Fatalf("POST title without the role: %+v", info)
	}

	e.configureRole(model.RoleOrganizer)
	if v := e.lectureView(l.ID); !offered(v) || !inList(l.ID) {
		t.Fatalf("summary done, organizer configured: %v", v.LectureActions)
	}
	// stale summary: still offered (the title task's dependency, lead ruling 1)
	e.moveJob(model.ScopeLecture, l.ID, model.StageSummary, model.StatusStale)
	if v := e.lectureView(l.ID); !offered(v) {
		t.Fatalf("summary stale: %v", v.LectureActions)
	}
	// and the endpoint takes it (the title is the user's: force)
	e.call("POST", "/api/lectures/"+l.ID+"/title", TitleRegenerateInput{Force: true}, http.StatusAccepted, nil)

	// summary not finished yet: not offered, and the endpoint refuses
	l2 := e.lecture(c.ID, "Week 4")
	check := func(when string, want bool) {
		t.Helper()
		if v := e.lectureView(l2.ID); offered(v) != want {
			t.Fatalf("%s: lecture_actions %v, want regenerate_title %v", when, v.LectureActions, want)
		}
	}
	check("summary pending", false)
	if info := e.callErrInfo("POST", "/api/lectures/"+l2.ID+"/title", nil, http.StatusConflict); info.Code != "title.summary_not_ready" {
		t.Fatalf("POST title before the summary: %+v", info)
	}
	for _, st := range []string{model.StagePrep, model.StageASR} {
		e.moveJob(model.ScopeLecture, l2.ID, st, model.StatusQueued, model.StatusRunning, model.StatusDone)
	}
	e.moveJob(model.ScopeLecture, l2.ID, model.StageSummary, model.StatusQueued, model.StatusRunning)
	check("summary running", false)
	e.moveJob(model.ScopeLecture, l2.ID, model.StageSummary, model.StatusDone)
	check("summary done", true)

	// a draft has no summary
	d := e.draft(c.ID, "Week 5")
	if v := e.lectureView(d.ID); offered(v) {
		t.Fatalf("draft: %v", v.LectureActions)
	}
}
