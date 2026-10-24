// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/cite"
	"lectureflow/internal/model"
)

// Fixes after the lecture-kinds review (NOTES.md "决定（类型审查修复 web）").

// Review item 2: an uploaded slide file is bound to the week its name gives
// only when the course has a term start; without one it applies to the
// whole course and week_suggested still names the week (the course page
// offers it as a one-click binding). A term start set later does not move
// existing slides.
func TestUploadWeekNeedsTermStart(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.draft(c.ID, "L")
	w2 := e.uploadMaterial(c.ID, "Week2-Block-Ciphers.pdf", nil)
	if w2.Bind == nil || w2.Bind.Scope != BindCourse || w2.Bind.Week != nil || w2.LectureID != nil ||
		w2.WeekSuggested == nil || *w2.WeekSuggested != 2 {
		t.Fatalf("without a term start %+v (bind %+v)", w2, w2.Bind)
	}
	if st, _ := e.st.GetMaterial(context.Background(), w2.ID); st.BindWeek != nil || st.LectureID != nil {
		t.Fatalf("stored %+v", st)
	}
	// lecture_id still binds to the lecture
	one := e.uploadMaterial(c.ID, "W03-notes.pdf", [][2]string{{"lecture_id", l.ID}})
	if one.Bind.Scope != BindLecture || one.LectureID == nil || *one.LectureID != l.ID || *one.WeekSuggested != 3 {
		t.Fatalf("lecture material %+v", one)
	}
	// the one-click binding of the course page
	var m Material
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: w2.WeekSuggested}}, http.StatusOK, &m)
	if m.Bind.Scope != BindWeek || *m.Bind.Week != 2 {
		t.Fatalf("bound to the suggested week %+v", m.Bind)
	}
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindCourse}}, http.StatusOK, &m)

	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, nil)
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	for _, x := range cv.Materials {
		if x.ID == w2.ID && x.Bind.Scope != BindCourse {
			t.Fatalf("a term start moved existing slides: %+v", x.Bind)
		}
	}
	w4 := e.uploadMaterial(c.ID, "wk 4 hashing.pdf", nil)
	if w4.Bind.Scope != BindWeek || w4.Bind.Week == nil || *w4.Bind.Week != 4 || *w4.WeekSuggested != 4 {
		t.Fatalf("with a term start %+v", w4.Bind)
	}
	plain := e.uploadMaterial(c.ID, "Hashing.pdf", nil)
	if plain.Bind.Scope != BindCourse || plain.WeekSuggested != nil {
		t.Fatalf("no week in the name %+v", plain)
	}
	// clearing the term start again: course-wide
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("")}, http.StatusOK, nil)
	if w5 := e.uploadMaterial(c.ID, "Week5.pdf", nil); w5.Bind.Scope != BindCourse || *w5.WeekSuggested != 5 {
		t.Fatalf("after clearing the term start %+v", w5.Bind)
	}
}

// Review item 11: a PATCH body with neither bind nor lecture_id.
func TestPatchMaterialBindMissing(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	m := e.uploadMaterial(c.ID, "a.pdf", nil)
	for _, body := range []any{map[string]any{}, map[string]any{"scope": "week", "week": 2}} {
		if info := e.callErrInfo("PATCH", "/api/materials/"+m.ID, body, http.StatusBadRequest); info.Code != "material.bind_missing" || len(info.Params) != 0 {
			t.Errorf("%v: %+v", body, info)
		}
	}
	if _, ok := errorCatalog["material.bind_missing"]; !ok {
		t.Error("material.bind_missing is not in the catalog")
	}
}

// Review item 5: the renumbering check compares citation labels, like the
// automatic renumbering notice. When one of two unnumbered lectures whose
// ids share their first 6 characters gets a number, the other one's
// "W??-" prefix shrinks back to 6 characters: its Label stays "W??" but
// its exported references change, so it is listed too.
func TestRenumberChangesCiteLabels(t *testing.T) {
	c := model.Course{ID: "C", Name: "C", TermStart: ptr("2026-09-28")}
	lecs := []model.Lecture{
		{ID: "01JMAAXQ01", CourseID: "C", Title: "A", CreatedAt: "2026-10-01T00:00:00Z"},
		{ID: "01JMAAYQ02", CourseID: "C", Title: "B", CreatedAt: "2026-10-01T00:00:01Z"},
		{ID: "01JMBBZQ03", CourseID: "C", Title: "N", CreatedAt: "2026-10-01T00:00:02Z", RecordedAt: ptr("2026-09-29T09:00")},
	}
	before := numberLectures(c, lecs, nil)
	if a, b := before["01JMAAXQ01"], before["01JMAAYQ02"]; a.Label != "W??" || a.CiteLabel != "W??-01JMAAX" || b.CiteLabel != "W??-01JMAAY" {
		t.Fatalf("before: %+v / %+v", a.Numbered, b.Numbered)
	}
	after := slices.Clone(lecs)
	after[0].RecordedAt = ptr("2026-10-06T09:00")
	afterNums := numberLectures(c, after, nil)
	got := renumberChanges(after, before, afterNums)
	want := []RenumberChange{
		{LectureID: "01JMAAXQ01", Title: "A", From: "W??-01JMAAX", To: "W02L1"},
		{LectureID: "01JMAAYQ02", Title: "B", From: "W??-01JMAAY", To: "W??-01JMAA"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes %+v, want %+v", got, want)
	}
	if b := afterNums["01JMAAYQ02"]; b.Label != "W??" {
		t.Fatalf("B's label %q (unchanged by the comparison)", b.Label)
	}
	// and the other way round: the prefix grows when a second unnumbered
	// lecture with the same 6 characters appears
	if got := renumberChanges(lecs, afterNums, before); len(got) != 2 || got[1].From != "W??-01JMAA" || got[1].To != "W??-01JMAAY" {
		t.Fatalf("back: %+v", got)
	}
	// nothing changes: no entries
	if got := renumberChanges(lecs, before, numberLectures(c, lecs, nil)); len(got) != 0 {
		t.Fatalf("unchanged: %+v", got)
	}
	// a lecture missing from a numbering counts as "W??"
	if got := renumberChanges(lecs[:1], numbering{}, numbering{}); len(got) != 0 {
		t.Fatalf("missing from both: %+v", got)
	}
}

// Review item 9: Lecture.materials comes from the bindings and SQL page
// counts (no page texts): pages as extracted, 0 before.
func TestLectureMaterialPages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.draft(c.ID, "L")
	a := e.uploadMaterial(c.ID, "a.pdf", [][2]string{{"lecture_id", l.ID}})
	b := e.uploadMaterial(c.ID, "b.pdf", nil)
	pages := func() []int {
		t.Helper()
		var out []int
		for _, m := range e.lectureView(l.ID).Materials {
			out = append(out, m.Pages)
		}
		var list []Lecture
		e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
		if len(list) != 1 || len(list[0].Materials) != len(out) {
			t.Fatalf("list materials %+v", list)
		}
		for i, m := range list[0].Materials {
			if m.Pages != out[i] {
				t.Fatalf("list pages %+v, view %v", list[0].Materials, out)
			}
		}
		return out
	}
	if got := pages(); !slices.Equal(got, []int{0, 0}) {
		t.Fatalf("not extracted: %v", got)
	}
	if err := e.st.SetMaterialPages(ctx, a.ID, []string{"# 1", "# 2", "# 3"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetMaterialPages(ctx, b.ID, []string{"only"}); err != nil {
		t.Fatal(err)
	}
	if got := pages(); !slices.Equal(got, []int{3, 1}) {
		t.Fatalf("extracted: %v", got)
	}
}

// Review item 7: the course citation JSON carries the kinds table with the
// user's name of a custom kind, and kind_code on the lecture.
func TestCourseCiteKinds(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.insertTranscript(l.ID, true)
	e.moveJob(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning, model.StatusDone)
	var k LectureKind
	e.call("POST", "/api/lecture-kinds", LectureKindInput{Name: ptr("Lab session"), Code: ptr("LB")}, http.StatusCreated, &k)
	e.call("PUT", "/api/lectures/"+l.ID+"/kind", LectureKindSet{Kind: k.ID}, http.StatusOK, nil)
	res, raw := e.do("GET", "/api/courses/"+c.ID+"/export/cite.json", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cite.json: %d %s", res.StatusCode, raw)
	}
	var doc cite.Doc
	decodeStrict(t, raw, &doc)
	if !reflect.DeepEqual(doc.Kinds, []cite.Kind{{ID: k.ID, Code: "LB", Name: "Lab session"}}) {
		t.Fatalf("kinds %+v", doc.Kinds)
	}
	if len(doc.Lectures) != 1 || doc.Lectures[0].Kind != k.ID || doc.Lectures[0].KindCode != "LB" || !strings.HasPrefix(doc.Lectures[0].Label, "W??-") {
		t.Fatalf("lectures %+v", doc.Lectures)
	}
}
