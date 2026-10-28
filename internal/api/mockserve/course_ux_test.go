// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// course_ux.go: week overrides and the batch rerun in the mock, with the
// real numbering (calendar.LabelKinds) and the real codes.

func mockLectures(t *testing.T, url string) map[string]api.Lecture {
	t.Helper()
	var ls []api.Lecture
	doJSON(t, "GET", url, nil, 200, &ls)
	out := map[string]api.Lecture{}
	for _, l := range ls {
		out[l.ID] = l
	}
	return out
}

func TestCourseUXScenario(t *testing.T) {
	srv := testServer(t)
	q := "?mock=courseux"
	ls := mockLectures(t, srv.URL+"/api/courses/"+courseID(c3631)+"/lectures"+q)
	m1, des := ls[lecID3631(10)], ls[lecID3631(4)]
	if o := m1.WeekOverride; o == nil || o.Kind != api.WeekOverrideSupplement || o.Week != 3 || !o.Applied || m1.Week != 3 || m1.Label != "M1" {
		t.Fatalf("supplement %s week %d %+v", m1.Label, m1.Week, m1.WeekOverride)
	}
	if o := des.WeekOverride; o == nil || o.Kind != api.WeekOverrideMakeup || des.Label != "W01L3" || des.Week != 1 {
		t.Fatalf("make-up %s %+v", des.Label, des.WeekOverride)
	}
	if l := ls[lecID3631(9)]; l.Label != "W02T1" {
		t.Fatalf("week 2 tutorial %s", l.Label)
	}
	os := mockLectures(t, srv.URL+"/api/courses/"+courseID(c2120)+"/lectures"+q)
	if mm := os[lecID2120(6)]; mm.Label != "W03L3" || mm.WeekOverride == nil || mm.CalendarHint != "" {
		t.Fatalf("COMP2120 make-up %s %+v %q", mm.Label, mm.WeekOverride, mm.CalendarHint)
	}
	// the default data has no overrides
	for _, l := range mockLectures(t, srv.URL+"/api/courses/"+courseID(c3631)+"/lectures") {
		if l.WeekOverride != nil {
			t.Fatalf("default data: %s has %+v", l.ID, l.WeekOverride)
		}
	}
}

func TestCourseUXWeekOverride(t *testing.T) {
	srv := testServer(t)
	q := "?mock=courseux"
	week := func(id string) string { return srv.URL + "/api/lectures/" + id + "/week" + q }
	var l api.Lecture
	var e api.ErrorBody

	// the past lecture hangs under week 2: numbers unchanged, no question
	doJSON(t, "PUT", week(lecID3631(11)), api.WeekOverrideSet{Week: 2, Kind: api.WeekOverrideSupplement}, 200, &l)
	if l.Label != "P1" || l.Week != 2 || l.WeekOverride == nil || !l.WeekOverride.Applied {
		t.Fatalf("past lecture under week 2 %s %d %+v", l.Label, l.Week, l.WeekOverride)
	}
	// only teaching materials
	doJSON(t, "PUT", week(lecID3631(5)), api.WeekOverrideSet{Week: 2, Kind: api.WeekOverrideSupplement}, 409, &e)
	if e.Error.Code != "week_override.not_material" {
		t.Fatalf("lecture as supplement %+v", e)
	}
	doJSON(t, "PUT", week(lecID3631(5)), api.WeekOverrideSet{Week: 0, Kind: api.WeekOverrideMakeup}, 400, &e)
	if e.Error.Code != "week_override.week_invalid" {
		t.Fatalf("week 0 %+v", e)
	}
	doJSON(t, "PUT", week(lecID3631(5)), api.WeekOverrideSet{Week: 2, Kind: "x"}, 400, &e)
	if e.Error.Code != "week_override.kind_invalid" {
		t.Fatalf("kind %+v", e)
	}
	// a make-up renumbers; CSC1003 exported citations: P13.6 asks first
	var rc api.RenumberConflict
	doJSON(t, "PUT", week(lecID3631(5)), api.WeekOverrideSet{Week: 4, Kind: api.WeekOverrideMakeup}, 409, &rc)
	if rc.Error.Code != "calendar.renumber" || len(rc.Changes) == 0 {
		t.Fatalf("conflict %+v", rc)
	}
	doJSON(t, "PUT", week(lecID3631(5)), api.WeekOverrideSet{Week: 4, Kind: api.WeekOverrideMakeup, Confirm: true}, 200, &l)
	if l.Label != "W04L3" || l.WeekOverride.Kind != api.WeekOverrideMakeup {
		t.Fatalf("make-up %s %+v", l.Label, l.WeekOverride)
	}
	// undo (asks again), then a lecture without one: nothing to do
	doJSON(t, "DELETE", week(lecID3631(5)), nil, 409, &rc)
	var back api.Lecture
	doJSON(t, "DELETE", week(lecID3631(5))+"&confirm=1", nil, 200, &back)
	if back.Label != "W03L1" || back.WeekOverride != nil {
		t.Fatalf("undone %s %+v", back.Label, back.WeekOverride)
	}
	doJSON(t, "DELETE", week(lecID3631(5)), nil, 200, &back)
	doJSON(t, "PUT", srv.URL+"/api/lectures/nope/week"+q, api.WeekOverrideSet{Week: 2, Kind: api.WeekOverrideMakeup}, 404, &e)
	if e.Error.Code != "lecture.not_found" {
		t.Fatalf("unknown lecture %+v", e)
	}
	// a kind that contradicts the override ends it (as the real server)
	var asLecture api.Lecture
	doJSON(t, "PUT", srv.URL+"/api/lectures/"+lecID3631(11)+"/kind"+q, api.LectureKindSet{Kind: model.KindLecture, Confirm: true}, 200, &asLecture)
	if asLecture.WeekOverride != nil || asLecture.Kind != model.KindLecture {
		t.Fatalf("supplement made a lecture: %s %+v", asLecture.Kind, asLecture.WeekOverride)
	}
}

func TestCourseUXRerun(t *testing.T) {
	srv := testServer(t)
	q := "?mock=courseux"
	url := srv.URL + "/api/courses/" + courseID(c3631) + "/rerun" + q
	ids := []string{lecID3631(1), lecID3631(3), lecID3631(6), lecID3631(8), lecID2120(1), "missing"}
	var dry api.CourseRerunResult
	doJSON(t, "POST", url, api.CourseRerunRequest{Stage: model.StageTranslate, LectureIDs: ids, DryRun: true}, 200, &dry)
	if dry.Queued != 2 || dry.Skipped != 3 || dry.Failed != 1 || len(dry.Results) != 6 {
		t.Fatalf("dry run %+v", dry)
	}
	codes := map[string]string{}
	for _, it := range dry.Results {
		if it.Reason != nil {
			codes[it.LectureID] = it.Reason.Code
		}
		if it.Lecture != nil {
			t.Fatalf("a dry run returns no lectures: %+v", it)
		}
	}
	if codes[lecID3631(6)] != "rerun.skipped_status" || codes[lecID3631(8)] != "rerun.skipped_draft" || codes[lecID2120(1)] != "rerun.other_course" || codes["missing"] != "lecture.not_found" {
		t.Fatalf("reasons %v", codes)
	}
	choice := ""
	if len(dry.Diff) > 0 {
		var nm api.NeedsModelChoice
		doJSON(t, "POST", url, api.CourseRerunRequest{Stage: model.StageTranslate, LectureIDs: ids}, http.StatusConflict, &nm)
		if nm.Error.Code != model.ErrNeedsModelChoice || len(nm.Diff) != len(dry.Diff) {
			t.Fatalf("409 %+v", nm)
		}
		choice = model.ModelChoiceCurrent
	}
	var res api.CourseRerunResult
	doJSON(t, "POST", url, api.CourseRerunRequest{Stage: model.StageTranslate, LectureIDs: ids, ModelChoice: choice}, 200, &res)
	if res.Queued != 2 {
		t.Fatalf("rerun %+v", res)
	}
	for _, it := range res.Results[:2] {
		if it.Lecture == nil || uxStageStatus(it.Lecture.Stages, model.StageTranslate) == model.StatusDone {
			t.Fatalf("rerun lecture %+v", it)
		}
	}
	var e api.ErrorBody
	doJSON(t, "POST", url, api.CourseRerunRequest{Stage: "title", LectureIDs: ids}, 400, &e)
	if e.Error.Code != "stage.invalid" {
		t.Fatalf("aux stage %+v", e)
	}
}

func uxStageStatus(ss []api.StageView, stage string) string {
	for _, s := range ss {
		if s.Stage == stage {
			return s.Status
		}
	}
	return ""
}
