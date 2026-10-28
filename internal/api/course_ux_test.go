// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// Course page requests of 2026-10-27 (course-ux).

func (e *env) jobStatuses(lectureID string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, st := range model.LectureStages {
		out[st] = e.moveJob(model.ScopeLecture, lectureID, st).Status
	}
	return out
}

func (e *env) jobIDs(lectureID string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, st := range model.LectureStages {
		out[st] = e.moveJob(model.ScopeLecture, lectureID, st).ID
	}
	return out
}

func rerunItems(r CourseRerunResult) map[string]CourseRerunItem {
	out := map[string]CourseRerunItem{}
	for _, it := range r.Results {
		out[it.LectureID] = it
	}
	return out
}

// POST /api/courses/{id}/rerun: a dry run changes nothing; the rerun goes
// through the single-lecture path (same jobs afterwards as POST
// /stages/{stage}/rerun); lectures that cannot rerun are skipped with the
// reason; one model question for all.
func TestCourseRerun(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	other := e.course("Other")
	l1, l2, busy, single := e.lecture(c.ID, "L1"), e.lecture(c.ID, "L2"), e.lecture(c.ID, "Busy"), e.lecture(c.ID, "Single")
	d := e.draft(c.ID, "Draft")
	o := e.lecture(other.ID, "Elsewhere")
	for _, l := range []Lecture{l1, l2, single, o} {
		e.allDone(l.ID)
	}
	for _, st := range []string{model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate} {
		e.moveJob(model.ScopeLecture, busy.ID, st, "queued", "running", "done")
	}
	e.moveJob(model.ScopeLecture, busy.ID, model.StageExport, "queued", "running")
	path := "/api/courses/" + c.ID + "/rerun"
	ids := []string{l1.ID, busy.ID, l2.ID, d.ID, o.ID, "missing", l1.ID}
	before1 := e.jobIDs(l1.ID)

	// dry run
	var dry CourseRerunResult
	e.call("POST", path, CourseRerunRequest{Stage: model.StageTranslate, LectureIDs: ids, DryRun: true}, http.StatusOK, &dry)
	check := func(r CourseRerunResult, dryRun bool) {
		t.Helper()
		if r.Stage != model.StageTranslate || r.DryRun != dryRun || len(r.Results) != 6 || r.Queued != 2 || r.Skipped != 3 || r.Failed != 1 || r.Diff == nil {
			t.Fatalf("result %+v", r)
		}
		if r.Results[0].LectureID != l1.ID || r.Results[5].LectureID != "missing" {
			t.Fatalf("order %+v", r.Results)
		}
		it := rerunItems(r)
		for _, l := range []Lecture{l1, l2} {
			if x := it[l.ID]; x.Result != RerunQueued || x.Reason != nil || (x.Lecture != nil) == dryRun {
				t.Fatalf("%s: %+v", l.Title, x)
			}
		}
		if x := it[busy.ID]; x.Result != RerunSkipped || x.Reason.Code != "rerun.skipped_busy" || x.Reason.Params["stage"] != model.StageExport {
			t.Fatalf("busy: %+v", x)
		}
		if x := it[d.ID]; x.Result != RerunSkipped || x.Reason.Code != "rerun.skipped_draft" {
			t.Fatalf("draft: %+v", x)
		}
		if x := it[o.ID]; x.Result != RerunSkipped || x.Reason.Code != "rerun.other_course" {
			t.Fatalf("other course: %+v", x)
		}
		if x := it["missing"]; x.Result != RerunFailed || x.Reason.Code != "lecture.not_found" {
			t.Fatalf("missing: %+v", x)
		}
	}
	check(dry, true)
	if len(dry.Diff) != 0 {
		t.Fatalf("no model question expected: %+v", dry.Diff)
	}
	if got := e.jobIDs(l1.ID); got[model.StageTranslate] != before1[model.StageTranslate] {
		t.Fatal("a dry run changed jobs")
	}

	// the rerun, and the single-lecture rerun for comparison
	var res CourseRerunResult
	e.call("POST", path, CourseRerunRequest{Stage: model.StageTranslate, LectureIDs: ids}, http.StatusOK, &res)
	check(res, false)
	var lv Lecture
	e.call("POST", "/api/lectures/"+single.ID+"/stages/translate/rerun", nil, http.StatusOK, &lv)
	want := e.jobStatuses(single.ID)
	for _, l := range []Lecture{l1, l2} {
		if got := e.jobStatuses(l.ID); !mapsEqual(got, want) {
			t.Fatalf("%s after the batch %v, after the single rerun %v", l.Title, got, want)
		}
		v := rerunItems(res)[l.ID].Lecture
		if st := stageOf(v.Stages, model.StageTranslate); st.Status != model.StatusPending {
			t.Fatalf("%s view translate %s", l.Title, st.Status)
		}
	}
	if got := e.jobIDs(l1.ID); got[model.StageTranslate] == before1[model.StageTranslate] || got[model.StageSummary] != before1[model.StageSummary] {
		t.Fatalf("translate must be a new job, summary the old one: %v %v", got, before1)
	}
	if got := e.jobStatuses(busy.ID); got[model.StageTranslate] != model.StatusDone || got[model.StageExport] != model.StatusRunning {
		t.Fatalf("skipped lecture changed: %v", got)
	}
	if got := e.jobStatuses(o.ID); got[model.StageTranslate] != model.StatusDone {
		t.Fatalf("other course's lecture changed: %v", got)
	}

	// one model question for all lectures
	e.allDone(l1.ID)
	e.allDone(l2.ID)
	var sv Settings
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, &sv)
	e.call("POST", path, CourseRerunRequest{Stage: model.StageSummary, LectureIDs: []string{l1.ID, l2.ID}, DryRun: true}, http.StatusOK, &dry)
	if dry.Queued != 2 || len(dry.Diff) != 2 || dry.Diff[0].LectureID == "" || dry.Diff[0].Stage != model.StageTranslate || dry.Diff[0].LectureTitle == "" {
		t.Fatalf("dry run with a question %+v", dry)
	}
	var nm NeedsModelChoice
	e.call("POST", path, CourseRerunRequest{Stage: model.StageSummary, LectureIDs: []string{l1.ID, l2.ID}}, http.StatusConflict, &nm)
	if nm.Error.Code != model.ErrNeedsModelChoice || len(nm.Diff) != 2 || nm.Diff[0].LectureID == nm.Diff[1].LectureID {
		t.Fatalf("409 %+v", nm)
	}
	if got := e.jobStatuses(l1.ID); got[model.StageSummary] != model.StatusDone {
		t.Fatal("the question changed something")
	}
	e.call("POST", path, CourseRerunRequest{Stage: model.StageSummary, LectureIDs: []string{l1.ID, l2.ID}, ModelChoice: model.ModelChoiceCurrent}, http.StatusOK, &res)
	if res.Queued != 2 {
		t.Fatalf("with a choice %+v", res)
	}
	for _, l := range []Lecture{l1, l2} {
		if m := snapshotModel(t, e.moveJob(model.ScopeLecture, l.ID, model.StageTranslate)); m != "deepseek-reasoner" {
			t.Fatalf("%s translate model %q", l.Title, m)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestCourseRerunInput(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	path := "/api/courses/" + c.ID + "/rerun"
	for _, st := range []string{"", model.StageTitle, "glossary_seed"} {
		if info := e.callErrInfo("POST", path, CourseRerunRequest{Stage: st, LectureIDs: []string{l.ID}}, http.StatusBadRequest); info.Code != "stage.invalid" {
			t.Errorf("stage %q: %+v", st, info)
		}
	}
	if info := e.callErrInfo("POST", path, CourseRerunRequest{Stage: model.StageExport}, http.StatusBadRequest); info.Code != "lecture.batch_empty" {
		t.Errorf("no lectures: %+v", info)
	}
	if info := e.callErrInfo("POST", path, CourseRerunRequest{Stage: model.StageExport, LectureIDs: []string{l.ID}, ModelChoice: "both"}, http.StatusBadRequest); info.Code != "request.bad_param" {
		t.Errorf("bad choice: %+v", info)
	}
	if info := e.callErrInfo("POST", "/api/courses/nope/rerun", CourseRerunRequest{Stage: model.StageExport, LectureIDs: []string{l.ID}}, http.StatusNotFound); info.Code != "course.not_found" {
		t.Errorf("unknown course: %+v", info)
	}
	// export pending: skipped with the status
	var res CourseRerunResult
	e.call("POST", path, CourseRerunRequest{Stage: model.StageExport, LectureIDs: []string{l.ID}}, http.StatusOK, &res)
	if x := res.Results[0]; x.Result != RerunSkipped || x.Reason.Code != "rerun.skipped_status" || x.Reason.Params["status"] != model.StatusPending || x.Reason.Params["stage"] != model.StageExport {
		t.Fatalf("pending export %+v", x)
	}
}

// PUT / DELETE /api/lectures/{id}/week: numbers, the view, the rules, the
// renumbering confirmation, and the kind change that ends an override.
func TestLectureWeekOverride(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, ls := kindCourse(e) // term 2026-09-28: L1 L2 T1 T2 in week 2, L3 in week 3, OLD from 2024
	for _, n := range []string{"T1", "T2"} {
		e.call("PUT", "/api/lectures/"+ls[n].ID+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, nil)
	}
	mat := e.uploadNamed(c.ID, "AES by hand.mp4")
	e.call("PATCH", "/api/lectures/"+mat.ID, LecturePatch{RecordedAt: ptr("2026-10-07T16:00")}, http.StatusOK, nil)
	e.call("PUT", "/api/lectures/"+mat.ID+"/kind", LectureKindSet{Kind: model.KindMaterial}, http.StatusOK, nil)
	path := func(id string) string { return "/api/lectures/" + id + "/week" }

	// a make-up: L1 (week 2) is week 3's content, after L3
	var lv Lecture
	e.call("PUT", path(ls["L1"].ID), WeekOverrideSet{Week: 3, Kind: WeekOverrideMakeup}, http.StatusOK, &lv)
	if lv.Label != "W03L2" || lv.Week != 3 || lv.WeekOverride == nil || lv.WeekOverride.Week != 3 || lv.WeekOverride.Kind != WeekOverrideMakeup ||
		!lv.WeekOverride.Applied || lv.WeekOverride.CreatedAt == "" {
		t.Fatalf("make-up %+v %+v", lv.Label, lv.WeekOverride)
	}
	got := e.courseLecturesByID(c.ID)
	if got[ls["L2"].ID].Label != "W02L1" || got[ls["L3"].ID].Label != "W03L1" || got[ls["L3"].ID].WeekOverride != nil {
		t.Fatalf("others %s %s", got[ls["L2"].ID].Label, got[ls["L3"].ID].Label)
	}

	// a supplement: only teaching materials
	if info := e.callErrInfo("PUT", path(ls["L2"].ID), WeekOverrideSet{Week: 3, Kind: WeekOverrideSupplement}, http.StatusConflict); info.Code != "week_override.not_material" || info.Params["kind"] != model.KindLecture {
		t.Fatalf("lecture as supplement %+v", info)
	}
	e.call("PUT", path(mat.ID), WeekOverrideSet{Week: 3, Kind: WeekOverrideSupplement}, http.StatusOK, &lv)
	if lv.Label != "M1" || lv.Week != 3 || lv.Kind != model.KindMaterial || lv.WeekOverride.Kind != WeekOverrideSupplement {
		t.Fatalf("supplement %+v", lv)
	}
	// the past recording (past_lecture by the date rule) is a teaching material too
	e.call("PUT", path(ls["OLD"].ID), WeekOverrideSet{Week: 2, Kind: WeekOverrideSupplement}, http.StatusOK, &lv)
	if lv.Label != "P1" || lv.Week != 2 {
		t.Fatalf("past recording as supplement %+v", lv)
	}

	// input
	if info := e.callErrInfo("PUT", path(mat.ID), WeekOverrideSet{Week: 0, Kind: WeekOverrideSupplement}, http.StatusBadRequest); info.Code != "week_override.week_invalid" {
		t.Errorf("week 0 %+v", info)
	}
	if info := e.callErrInfo("PUT", path(mat.ID), WeekOverrideSet{Week: 61, Kind: WeekOverrideSupplement}, http.StatusBadRequest); info.Code != "week_override.week_invalid" {
		t.Errorf("week 61 %+v", info)
	}
	if info := e.callErrInfo("PUT", path(mat.ID), WeekOverrideSet{Week: 2, Kind: "moved"}, http.StatusBadRequest); info.Code != "week_override.kind_invalid" {
		t.Errorf("kind %+v", info)
	}
	if info := e.callErrInfo("PUT", path("nope"), WeekOverrideSet{Week: 2, Kind: WeekOverrideMakeup}, http.StatusNotFound); info.Code != "lecture.not_found" {
		t.Errorf("unknown lecture %+v", info)
	}
	if info := e.callErrInfo("DELETE", path("nope"), nil, http.StatusNotFound); info.Code != "lecture.not_found" {
		t.Errorf("unknown lecture %+v", info)
	}
	noTerm := e.course("No term")
	x := e.draft(noTerm.ID, "X")
	if info := e.callErrInfo("PUT", path(x.ID), WeekOverrideSet{Week: 2, Kind: WeekOverrideMakeup}, http.StatusConflict); info.Code != "week_override.no_term_start" {
		t.Errorf("no term start %+v", info)
	}

	// P13.6: after a citation export a change of numbers asks first
	if err := stages.MarkCiteExported(ctx, e.st, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, raw := e.do("DELETE", path(ls["L1"].ID), nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("undo without confirm: %d %s", res.StatusCode, raw)
	}
	var rc RenumberConflict
	decodeStrict(t, raw, &rc)
	if rc.Error.Code != "calendar.renumber" || len(rc.Changes) != 2 {
		t.Fatalf("conflict %+v", rc)
	}
	if e.storedLecture(ls["L1"].ID).WeekOverride == nil {
		t.Fatal("the 409 removed the override")
	}
	var undone Lecture // (a fresh value: an absent week_override leaves a decoded pointer as it was)
	e.call("DELETE", path(ls["L1"].ID)+"?confirm=1", nil, http.StatusOK, &undone)
	if undone.WeekOverride != nil || undone.Label != "W02L1" {
		t.Fatalf("undone %+v %+v", undone.Label, undone.WeekOverride)
	}
	e.call("DELETE", path(ls["L1"].ID), nil, http.StatusOK, &lv) // nothing to undo: no question
	// the supplement does not change numbers: no question
	e.call("PUT", path(mat.ID), WeekOverrideSet{Week: 4, Kind: WeekOverrideSupplement}, http.StatusOK, &lv)
	if lv.Week != 4 || lv.Label != "M1" {
		t.Fatalf("moved supplement %+v", lv)
	}

	// a kind that contradicts the override ends it (the later instruction wins)
	var asLecture Lecture
	e.call("PUT", "/api/lectures/"+mat.ID+"/kind", LectureKindSet{Kind: model.KindLecture, Confirm: true}, http.StatusOK, &asLecture)
	if asLecture.WeekOverride != nil || e.storedLecture(mat.ID).WeekOverride != nil || asLecture.Kind != model.KindLecture {
		t.Fatalf("supplement made a lecture %+v %+v", asLecture.Kind, asLecture.WeekOverride)
	}
	e.call("PUT", path(ls["L2"].ID), WeekOverrideSet{Week: 4, Kind: WeekOverrideMakeup, Confirm: true}, http.StatusOK, &lv)
	e.call("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: model.KindSeminar, Confirm: true}, http.StatusOK, &lv)
	if lv.WeekOverride == nil || lv.Label != "W04S1" {
		t.Fatalf("a session kind keeps the make-up %+v %+v", lv.Label, lv.WeekOverride)
	}
	var asPast Lecture
	e.call("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: model.KindPastLecture, Confirm: true}, http.StatusOK, &asPast)
	if asPast.WeekOverride != nil || e.storedLecture(ls["L2"].ID).WeekOverride != nil || asPast.Kind != model.KindPastLecture {
		t.Fatalf("make-up made a material %+v %+v", asPast.Kind, asPast.WeekOverride)
	}

	// a lecture with an override is deleted as usual (the row cascades;
	// internal/db TestWeekOverrideCascade)
	e.call("DELETE", "/api/lectures/"+ls["OLD"].ID, nil, http.StatusNoContent, nil)
	if _, ok := e.courseLecturesByID(c.ID)[ls["OLD"].ID]; ok {
		t.Fatal("deleted lecture still listed")
	}
}
