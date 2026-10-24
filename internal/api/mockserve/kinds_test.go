// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/api"
)

// kinds.go: the kind catalog, kinds and per-kind numbers, numbering modes,
// slides bound to a teaching week and the renumbering notice.

func TestKindCatalog(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/lecture-kinds"
	var ks []api.LectureKind
	doJSON(t, "GET", base, nil, 200, &ks)
	var ids []string
	for _, k := range ks {
		ids = append(ids, k.ID+"/"+k.Code+"/"+k.Group)
		if k.Builtin != (k.Name == nil) {
			t.Errorf("%s: built-in kinds have no name, custom kinds have one", k.ID)
		}
	}
	if got := strings.Join(ids, " "); got != "lecture/L/session tutorial/T/session seminar/S/session past_lecture/P/material material/M/material custom-lab/LAB/session" {
		t.Fatalf("catalog %s", got)
	}
	var e api.ErrorBody
	name, bad, taken, group := "Workshop", "w1", "T", "lecture"
	doJSON(t, "POST", base, api.LectureKindInput{Name: &name, Code: &bad}, 400, &e)
	if e.Error.Code != "kind.code_invalid" {
		t.Errorf("bad code: %+v", e)
	}
	doJSON(t, "POST", base, api.LectureKindInput{Name: &name, Code: &taken}, 409, &e)
	if e.Error.Code != "kind.code_taken" || e.Error.Params["code"] != "T" {
		t.Errorf("taken code: %+v", e)
	}
	code := "wk"
	doJSON(t, "POST", base, api.LectureKindInput{Name: &name, Code: &code, Group: &group}, 400, &e)
	if e.Error.Code != "kind.group_invalid" {
		t.Errorf("bad group: %+v", e)
	}
	var k api.LectureKind
	doJSON(t, "POST", base, api.LectureKindInput{Name: &name, Code: &code}, 201, &k)
	if k.Code != "WK" || k.Group != api.KindGroupSession || k.Builtin || k.Name == nil || *k.Name != name || k.Order != 70 {
		t.Fatalf("created %+v", k)
	}
	doJSON(t, "POST", base, api.LectureKindInput{Name: &name, Code: sp("WS")}, 409, &e)
	if e.Error.Code != "kind.name_taken" {
		t.Errorf("same name: %+v", e)
	}
	// built-in kinds are fixed
	doJSON(t, "PATCH", base+"/tutorial", api.LectureKindInput{Name: &name}, 400, &e)
	doJSON(t, "DELETE", base+"/tutorial", nil, 400, &e)
	if e.Error.Code != "kind.builtin" {
		t.Errorf("built-in: %+v", e)
	}
	// a kind in use: renamed, but its code stays and it cannot be deleted
	lec := srv.URL + "/api/lectures/" + lecID(c3311, 1)
	var l api.Lecture
	doJSON(t, "PUT", lec+"/kind", api.LectureKindSet{Kind: k.ID}, 200, &l)
	if l.Kind != k.ID || l.KindSource != api.KindSourceUser || l.CiteLabel == "" {
		t.Fatalf("custom kind %+v", l)
	}
	doJSON(t, "DELETE", base+"/"+k.ID, nil, 409, &e)
	if e.Error.Code != "kind.in_use" || e.Error.Params["count"] != float64(1) {
		t.Errorf("in use: %+v", e)
	}
	doJSON(t, "PATCH", base+"/"+k.ID, api.LectureKindInput{Code: sp("WX")}, 409, &e)
	renamed := "Workshop session"
	doJSON(t, "PATCH", base+"/"+k.ID, api.LectureKindInput{Name: &renamed}, 200, &k)
	if *k.Name != renamed || k.Code != "WK" {
		t.Errorf("renamed %+v", k)
	}
	doJSON(t, "PUT", lec+"/kind", api.LectureKindSet{Kind: "nope"}, 400, &e)
	if e.Error.Code != "kind.unknown" {
		t.Errorf("unknown kind: %+v", e)
	}
	// automatic again: COMP3311 has no rule, so the kind is not decided
	doJSON(t, "PUT", lec+"/kind", api.LectureKindSet{Kind: ""}, 200, &l)
	if l.Kind != "lecture" || l.KindSource != api.KindSourceNone {
		t.Errorf("automatic %s/%s", l.Kind, l.KindSource)
	}
	doJSON(t, "DELETE", base+"/"+k.ID, nil, 204, nil)
	doJSON(t, "DELETE", base+"/"+k.ID, nil, 404, &e)
	if e.Error.Code != "kind.not_found" {
		t.Errorf("deleted: %+v", e)
	}
}

func TestKindsAndNumbers(t *testing.T) {
	srv := testServer(t)
	// three tutorial recordings on one day: numbered by start time
	ls := lecturesOf(t, srv.URL, c3121)
	for seq, want := range map[int]string{7: "W03T1 1/3", 5: "W03T2 2/3", 4: "W03T3 3/3"} {
		l := ls[lecID(c3121, seq)]
		if got := l.Label + " " + strconv.Itoa(l.KindN) + "/" + strconv.Itoa(l.KindCount); got != want || l.Kind != "tutorial" || l.KindSource != api.KindSourceFilename || !l.ShowN || l.CiteLabel != l.Label {
			t.Errorf("tutorial %d: %s %s/%s show_n %v", seq, got, l.Kind, l.KindSource, l.ShowN)
		}
	}
	if l := ls[lecID(c3121, 3)]; l.Label != "W03L1" || l.ShowN {
		t.Errorf("the only lecture of week 3: %s show_n %v", l.Label, l.ShowN)
	}
	if l := ls[lecID(c3121, 6)]; l.Kind != "seminar" || l.KindSource != api.KindSourceUser || l.Label != "W??" || !strings.HasPrefix(l.CiteLabel, "W??-") {
		t.Errorf("guest talk %s/%s %s %s", l.Kind, l.KindSource, l.Label, l.CiteLabel)
	}
	ls = lecturesOf(t, srv.URL, c3631)
	// numbered: it has a number (P1) although it has no teaching week (K1:
	// calendar.Numbered.OK of a material kind)
	if l := ls[lecID(c3631, 11)]; l.Kind != "past_lecture" || l.KindSource != api.KindSourceLLM || l.Label != "P1" || !l.Numbered || l.Week != 0 || l.KindN != 1 {
		t.Errorf("past lecture %s/%s %s numbered %v n %d", l.Kind, l.KindSource, l.Label, l.Numbered, l.KindN)
	}
	if l := ls[lecID(c3631, 10)]; l.Kind != "material" || l.Label != "M1" || !l.Numbered || l.Week != 3 {
		t.Errorf("additional video in week 3: %s week %d", l.Label, l.Week)
	}
	// the kind set by hand renumbers the week; the course exported citations
	cu := srv.URL + "/api/courses/" + courseID(c3631)
	tut := srv.URL + "/api/lectures/" + lecID(c3631, 9)
	var conflict api.RenumberConflict
	doJSON(t, "PUT", tut+"/kind", api.LectureKindSet{Kind: "lecture"}, http.StatusConflict, &conflict)
	if conflict.Error.Code != "calendar.renumber" || len(conflict.Changes) != 1 || conflict.Changes[0].From != "W02T1" || conflict.Changes[0].To != "W02L3" {
		t.Fatalf("conflict %+v", conflict)
	}
	var l api.Lecture
	doJSON(t, "PUT", tut+"/kind", api.LectureKindSet{Kind: "lecture", Confirm: true}, 200, &l)
	if l.Label != "W02L3" || l.KindN != 3 || l.KindCount != 3 || !l.ShowN || l.KindSource != api.KindSourceUser {
		t.Errorf("as a lecture: %s %d/%d", l.Label, l.KindN, l.KindCount)
	}
	// automatic again: the file name says T
	doJSON(t, "PUT", tut+"/kind", api.LectureKindSet{Kind: "", Confirm: true}, 200, &l)
	if l.Kind != "tutorial" || l.KindSource != api.KindSourceFilename || l.Label != "W02T1" {
		t.Errorf("automatic: %s/%s %s", l.Kind, l.KindSource, l.Label)
	}
	// numbering modes only change what is shown
	var c api.Course
	if doJSON(t, "GET", cu, nil, 200, &c); c.Numbering != api.NumberingAuto {
		t.Errorf("default numbering %q", c.Numbering)
	}
	for mode, want := range map[string]bool{api.NumberingAlways: true, api.NumberingNever: false} {
		m := mode
		doJSON(t, "PATCH", cu, api.CoursePatch{Numbering: &m}, 200, &c)
		if l := lectureOf(t, srv.URL, lecID(c3631, 9)); l.ShowN != want || c.Numbering != mode {
			t.Errorf("%s: the only tutorial of week 2 show_n %v", mode, l.ShowN)
		}
	}
	var e api.ErrorBody
	bad := "sometimes"
	doJSON(t, "PATCH", cu, api.CoursePatch{Numbering: &bad}, 400, &e)
	if e.Error.Code != "course.numbering_invalid" {
		t.Errorf("bad numbering %+v", e)
	}
	// the dashboard's lists carry the tag
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard", nil, 200, &d)
	for _, r := range d.Reviews {
		if r.Kind == "" {
			t.Errorf("review without a kind: %+v", r)
		}
	}
}

func TestMaterialsByWeek(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	aes := lectureOf(t, srv.URL, lecID(c3631, 5)) // week 3
	var uses []string
	for _, m := range aes.Materials {
		uses = append(uses, m.Filename+"/"+m.Via)
	}
	if got := strings.Join(uses, " "); got != "W03-AES.pdf/week W03-Modes-of-Operation.pdf/week CSC1003 Module Handbook 2026-27.pdf/course" {
		t.Errorf("week 3 slides: %s", got)
	}
	des := lectureOf(t, srv.URL, lecID(c3631, 4))
	if len(des.Materials) != 3 || des.Materials[0].Via != api.BindLecture || des.Materials[1].Week != 2 {
		t.Errorf("week 2 lecture with its own slides %+v", des.Materials)
	}
	// bound to another week by hand
	mid := "01JM000000000000000036310E"
	var m api.Material
	week := 5
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+mid, map[string]any{"bind": api.MaterialBind{Scope: api.BindWeek, Week: &week}}, 200, &m)
	if m.Bind == nil || m.Bind.Scope != api.BindWeek || *m.Bind.Week != 5 || m.WeekSuggested == nil || *m.WeekSuggested != 3 || m.LectureID != nil {
		t.Fatalf("bound %+v", m)
	}
	if aes = lectureOf(t, srv.URL, lecID(c3631, 5)); len(aes.Materials) != 2 {
		t.Errorf("week 3 after the move: %+v", aes.Materials)
	}
	var e api.ErrorBody
	zero := 0
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+mid, map[string]any{"bind": api.MaterialBind{Scope: api.BindWeek, Week: &zero}}, 400, &e)
	if e.Error.Code != "material.bind_week_invalid" {
		t.Errorf("week 0: %+v", e)
	}
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+mid, map[string]any{"bind": map[string]any{"scope": "term"}}, 400, &e)
	if e.Error.Code != "material.bind_invalid" {
		t.Errorf("bad scope: %+v", e)
	}
	other := lecID(c2120, 1)
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+mid, map[string]any{"bind": api.MaterialBind{Scope: api.BindLecture, LectureID: &other}}, 400, &e)
	// the v1 body still works and means scope lecture
	lid := lecID(c3631, 5)
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+mid, map[string]any{"lecture_id": lid}, 200, &m)
	if m.Bind == nil || m.Bind.Scope != api.BindLecture || *m.Bind.LectureID != lid || m.LectureID == nil {
		t.Errorf("v1 binding %+v", m.Bind)
	}
	// a new upload is bound to the week in its name
	var c api.Course
	postFile(t, srv.URL+"/api/courses/"+courseID(c3631)+"/materials", "Week05_RSA.pdf", "%PDF", 201, &c)
	nm := c.Materials[len(c.Materials)-1]
	if nm.Bind == nil || nm.Bind.Scope != api.BindWeek || *nm.Bind.Week != 5 || nm.WeekSuggested == nil || *nm.WeekSuggested != 5 {
		t.Errorf("new slides %+v %v", nm.Bind, nm.WeekSuggested)
	}
	// review item 2: without a term start (COMP3311) the week is only suggested
	postFile(t, srv.URL+"/api/courses/"+courseID(c3311)+"/materials", "Week05_RSA.pdf", "%PDF", 201, &c)
	nm = c.Materials[len(c.Materials)-1]
	if c.TermStart != nil || nm.Bind == nil || nm.Bind.Scope != api.BindCourse || nm.WeekSuggested == nil || *nm.WeekSuggested != 5 {
		t.Errorf("slides of a course without a term start %+v %v", nm.Bind, nm.WeekSuggested)
	}
	// review item 11: neither bind nor lecture_id
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+nm.ID, map[string]any{}, 400, &e)
	if e.Error.Code != "material.bind_missing" {
		t.Errorf("empty body: %+v", e)
	}
	for name, want := range map[string]int{"Week1-Introduction.pdf": 1, "wk 12 notes.pdf": 12, "W03-AES.pdf": 3, "StreamCiphers-2.pdf": 0, "H&P-ch2-slides.pdf": 0, "CSC1003 Module Handbook 2026-27.pdf": 0} {
		got := 0
		if w := weekFromFilename(name); w != nil {
			got = *w
		}
		if got != want {
			t.Errorf("week in %q: %d, want %d", name, got, want)
		}
	}
}

// The scenario "renumbered": the notice of an automatic renumbering on the
// course, dismissed by DELETE.
func TestRenumberedScenario(t *testing.T) {
	srv := testServer(t)
	cu := srv.URL + "/api/courses/" + courseID(c3311)
	var c api.Course
	doJSON(t, "GET", cu+"?mock=renumbered", nil, 200, &c)
	// oldest first: the one-time check of the past-lecture rule, then the title task
	if len(c.RenumberNotices) != 2 || c.RenumberNotices[0].Code != "calendar.renumbered_upgrade" || c.RenumberNotices[1].Code != "calendar.renumbered_auto" ||
		c.RenumberNotices[0].At == "" || c.RenumberNotices[0].At >= c.RenumberNotices[1].At || !c.CiteExported {
		t.Fatalf("notices %+v", c.RenumberNotices)
	}
	var list []api.Lecture
	doJSON(t, "GET", cu+"/lectures?mock=renumbered", nil, 200, &list)
	labels := map[string]string{}
	for _, l := range list {
		labels[l.ID] = l.Label + "/" + l.Kind + "/" + l.KindSource
	}
	if labels[lecID(c3311, 2)] != "W02T1/tutorial/llm" || labels[lecID(c3311, 5)] != "W02L1/lecture/llm" || labels[lecID(c3311, 6)] != "P1/past_lecture/date" {
		t.Fatalf("labels %v", labels)
	}
	c = api.Course{}
	doJSON(t, "GET", cu, nil, 200, &c) // the default data has none
	if len(c.RenumberNotices) != 0 {
		t.Fatalf("default data %+v", c.RenumberNotices)
	}
	c = api.Course{}
	doJSON(t, "DELETE", cu+"/renumber-notices?mock=renumbered", nil, 200, &c)
	if len(c.RenumberNotices) != 0 {
		t.Fatalf("dismissed %+v", c.RenumberNotices)
	}
	c = api.Course{}
	doJSON(t, "GET", cu+"?mock=renumbered", nil, 200, &c)
	if len(c.RenumberNotices) != 0 {
		t.Fatalf("after the dismissal %+v", c.RenumberNotices)
	}
}
