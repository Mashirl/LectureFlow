// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// Lecture kinds, per-kind numbering and slides bound to a teaching week
// (.claude/kinds-brief.md section 3; K3).

func TestLectureKindCatalog(t *testing.T) {
	e := newEnv(t)
	base := "/api/lecture-kinds"
	var ks []LectureKind
	e.call("GET", base, nil, http.StatusOK, &ks)
	var ids []string
	for _, k := range ks {
		ids = append(ids, k.ID)
		if !k.Builtin || k.Name != nil {
			t.Errorf("built-in kind %+v", k)
		}
	}
	if !slices.Equal(ids, model.BuiltinKindIDs) || ks[1].Code != "T" || ks[3].Group != KindGroupMaterial {
		t.Fatalf("catalog %+v", ks)
	}

	bad := []struct {
		in   LectureKindInput
		code string
	}{
		{LectureKindInput{Code: ptr("W")}, "kind.name_empty"},
		{LectureKindInput{Name: ptr(strings.Repeat("长", model.KindNameMaxLen+1)), Code: ptr("W")}, "kind.name_too_long"},
		{LectureKindInput{Name: ptr("Workshop"), Code: ptr("W1")}, "kind.code_invalid"},
		{LectureKindInput{Name: ptr("Workshop"), Code: ptr("ABCD")}, "kind.code_invalid"},
		{LectureKindInput{Name: ptr("Workshop"), Code: ptr("W"), Group: ptr("lecture")}, "kind.group_invalid"},
	}
	for _, b := range bad {
		if info := e.callErrInfo("POST", base, b.in, http.StatusBadRequest); info.Code != b.code {
			t.Errorf("%+v: %+v, want %s", b.in, info, b.code)
		}
	}
	if info := e.callErrInfo("POST", base, LectureKindInput{Name: ptr("Workshop"), Code: ptr("t")}, http.StatusConflict); info.Code != "kind.code_taken" || info.Params["code"] != "T" {
		t.Errorf("code taken %+v", info)
	}
	var k LectureKind
	e.call("POST", base, LectureKindInput{Name: ptr(" Workshop "), Code: ptr(" wk ")}, http.StatusCreated, &k)
	if k.Builtin || k.Code != "WK" || k.Name == nil || *k.Name != "Workshop" || k.Group != KindGroupSession || k.Order != 60 || k.ID == "" {
		t.Fatalf("created %+v", k)
	}
	if info := e.callErrInfo("POST", base, LectureKindInput{Name: ptr("workshop"), Code: ptr("WS")}, http.StatusConflict); info.Code != "kind.name_taken" {
		t.Errorf("name taken %+v", info)
	}
	// built-in kinds are fixed
	if info := e.callErrInfo("PATCH", base+"/tutorial", LectureKindInput{Name: ptr("x")}, http.StatusBadRequest); info.Code != "kind.builtin" {
		t.Errorf("patch built-in %+v", info)
	}
	if info := e.callErrInfo("DELETE", base+"/lecture", nil, http.StatusBadRequest); info.Code != "kind.builtin" {
		t.Errorf("delete built-in %+v", info)
	}
	if info := e.callErrInfo("PATCH", base+"/"+k.ID, LectureKindInput{}, http.StatusBadRequest); info.Code != "kind.nothing_to_change" {
		t.Errorf("nothing to change %+v", info)
	}
	if info := e.callErrInfo("PATCH", base+"/nope", LectureKindInput{Name: ptr("x")}, http.StatusNotFound); info.Code != "kind.not_found" {
		t.Errorf("unknown kind %+v", info)
	}
	// unused: the code and the group may change
	e.call("PATCH", base+"/"+k.ID, LectureKindInput{Code: ptr("WS"), Group: ptr(KindGroupMaterial)}, http.StatusOK, &k)
	if k.Code != "WS" || k.Group != KindGroupMaterial {
		t.Fatalf("patched %+v", k)
	}
	e.call("PATCH", base+"/"+k.ID, LectureKindInput{Group: ptr(KindGroupSession)}, http.StatusOK, &k)

	// in use: renamed, but neither its code nor its group changes, nor is it deleted
	c := e.course("C")
	l := e.draft(c.ID, "L")
	var lv Lecture
	e.call("PUT", "/api/lectures/"+l.ID+"/kind", LectureKindSet{Kind: k.ID}, http.StatusOK, &lv)
	if lv.Kind != k.ID || lv.KindSource != KindSourceUser {
		t.Fatalf("custom kind %+v", lv)
	}
	for _, in := range []LectureKindInput{{Code: ptr("WX")}, {Group: ptr(KindGroupMaterial)}} {
		if info := e.callErrInfo("PATCH", base+"/"+k.ID, in, http.StatusConflict); info.Code != "kind.in_use" || info.Params["count"] != float64(1) {
			t.Errorf("in use %+v: %+v", in, info)
		}
	}
	e.call("PATCH", base+"/"+k.ID, LectureKindInput{Name: ptr("Workshop session"), Code: ptr("ws")}, http.StatusOK, &k)
	if *k.Name != "Workshop session" || k.Code != "WS" {
		t.Errorf("renamed %+v", k)
	}
	if info := e.callErrInfo("DELETE", base+"/"+k.ID, nil, http.StatusConflict); info.Code != "kind.in_use" {
		t.Errorf("delete in use %+v", info)
	}

	// deleting drops the kind_map entries pointing at it (rule and proposal)
	e.call("PUT", "/api/lectures/"+l.ID+"/kind", LectureKindSet{Kind: ""}, http.StatusOK, &lv)
	if lv.Kind != model.KindLecture || lv.KindSource != KindSourceNone {
		t.Fatalf("automatic without a rule %+v", lv)
	}
	ctx := context.Background()
	rule := `^(?P<kind>[A-Z]+)_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	e.call("PUT", "/api/courses/"+c.ID+"/filename-rule", FilenamePreviewInput{Regex: rule,
		KindMap: map[string]string{"WS": k.ID, "T": model.KindTutorial}}, http.StatusOK, nil)
	prop, _ := json.Marshal(calendar.FilenameRule{Regex: rule, Source: calendar.RuleSourceLLM, KindMap: map[string]string{"X": k.ID, "L": "lecture"}})
	raw, _ := json.Marshal(model.FilenameProposal{Rule: prop, Explanation: "x", CreatedAt: "2026-10-24T00:00:00Z"})
	if err := e.st.SetKV(ctx, model.KVFilenameProposal+c.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	e.call("DELETE", base+"/"+k.ID, nil, http.StatusNoContent, nil)
	var v FilenameRuleView
	e.call("GET", "/api/courses/"+c.ID+"/filename-rule", nil, http.StatusOK, &v)
	if v.Rule == nil || len(v.Rule.KindMap) != 1 || v.Rule.KindMap["T"] != model.KindTutorial || len(v.KindMap) != 1 ||
		v.Proposal == nil || len(v.Proposal.KindMap) != 1 || v.Proposal.KindMap["L"] != "lecture" {
		t.Fatalf("kind maps after the delete: rule %+v view %v proposal %+v", v.Rule, v.KindMap, v.Proposal)
	}
	if info := e.callErrInfo("DELETE", base+"/"+k.ID, nil, http.StatusNotFound); info.Code != "kind.not_found" {
		t.Errorf("deleted twice %+v", info)
	}
	e.call("GET", base, nil, http.StatusOK, &ks)
	if len(ks) != 5 {
		t.Fatalf("catalog after the delete %+v", ks)
	}
}

// kindCourse is a course with term start 28 Sep 2026 and lectures uploaded
// with Panopto-like names: two lectures and two tutorials in week 2, a
// lecture in week 3 and a recording from 2024.
func kindCourse(e *env) (Course, map[string]Lecture) {
	e.t.Helper()
	c := e.course("C")
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, &c)
	ls := map[string]Lecture{}
	for _, x := range []struct{ name, file, at string }{
		{"L1", "C-L01-01 2026-10-06.mp4", "2026-10-06T09:00"},
		{"L2", "C-L01-02 2026-10-08.mp4", "2026-10-08T09:00"},
		{"T1", "C-T01-01 2026-10-07.mp4", "2026-10-07T13:00"},
		{"T2", "C-T01-02 2026-10-07.mp4", "2026-10-07T15:00"},
		{"L3", "C-L01-03 2026-10-13.mp4", "2026-10-13T09:00"},
		{"OLD", "C-L01-01 2024-10-07.mp4", "2024-10-07T09:00"},
	} {
		l := e.uploadNamed(c.ID, x.file)
		e.call("PATCH", "/api/lectures/"+l.ID, LecturePatch{RecordedAt: ptr(x.at)}, http.StatusOK, &l)
		ls[x.name] = l
	}
	return c, ls
}

func (e *env) courseLecturesByID(courseID string) map[string]Lecture {
	e.t.Helper()
	var list []Lecture
	e.call("GET", "/api/courses/"+courseID+"/lectures", nil, http.StatusOK, &list)
	out := map[string]Lecture{}
	for _, l := range list {
		out[l.ID] = l
	}
	return out
}

func TestLectureKindsNumbering(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, ls := kindCourse(e)
	for _, n := range []string{"T1", "T2"} {
		e.call("PUT", "/api/lectures/"+ls[n].ID+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, nil)
	}
	type want struct {
		label, kind, source string
		week, n, kn, kc     int
		showN, numbered     bool
	}
	check := func(numbering string, wants map[string]want) {
		t.Helper()
		got := e.courseLecturesByID(c.ID)
		for name, w := range wants {
			l := got[ls[name].ID]
			if l.Label != w.label || l.CiteLabel != w.label || l.Kind != w.kind || l.KindSource != w.source || l.Week != w.week || l.N != w.n ||
				l.KindN != w.kn || l.KindCount != w.kc || l.ShowN != w.showN || l.Numbered != w.numbered || l.Materials == nil {
				t.Errorf("%s %s: %s/%s kind %s/%s week %d n %d %d/%d show_n %v numbered %v, want %+v", numbering, name, l.Label, l.CiteLabel,
					l.Kind, l.KindSource, l.Week, l.N, l.KindN, l.KindCount, l.ShowN, l.Numbered, w)
			}
		}
	}
	auto := map[string]want{
		"L1":  {"W02L1", "lecture", "none", 2, 1, 1, 2, true, true},
		"L2":  {"W02L2", "lecture", "none", 2, 2, 2, 2, true, true},
		"T1":  {"W02T1", "tutorial", "user", 2, 1, 1, 2, true, true},
		"T2":  {"W02T2", "tutorial", "user", 2, 2, 2, 2, true, true},
		"L3":  {"W03L1", "lecture", "none", 3, 1, 1, 1, false, true},
		"OLD": {"P1", "past_lecture", KindSourceDate, 0, 1, 1, 1, false, true},
	}
	check("auto", auto)
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.Numbering != NumberingAuto {
		t.Fatalf("numbering %q", cv.Numbering)
	}
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Numbering: ptr(NumberingAlways)}, http.StatusOK, &cv)
	always := map[string]want{}
	for k, w := range auto {
		w.showN = true
		always[k] = w
	}
	check("always", always)
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Numbering: ptr(NumberingNever)}, http.StatusOK, &cv)
	never := map[string]want{}
	for k, w := range auto {
		w.showN = false
		never[k] = w
	}
	check("never", never)
	if info := e.callErrInfo("PATCH", "/api/courses/"+c.ID, CoursePatch{Numbering: ptr("sometimes")}, http.StatusBadRequest); info.Code != "course.numbering_invalid" || info.Params["value"] != "sometimes" {
		t.Errorf("bad numbering %+v", info)
	}
	if cv.Numbering != NumberingNever {
		t.Fatalf("course numbering %q", cv.Numbering)
	}

	// The kind set by hand renumbers; the numbering mode never asks.
	if err := stages.MarkCiteExported(ctx, e.st, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Numbering: ptr(NumberingAuto)}, http.StatusOK, nil)
	if info := e.callErrInfo("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: "nope"}, http.StatusBadRequest); info.Code != "kind.unknown" || info.Params["kind"] != "nope" {
		t.Errorf("unknown kind %+v", info)
	}
	if info := e.callErrInfo("PUT", "/api/lectures/nope/kind", LectureKindSet{Kind: "lecture"}, http.StatusNotFound); info.Code != "lecture.not_found" {
		t.Errorf("unknown lecture %+v", info)
	}
	res, raw := e.do("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: model.KindSeminar})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("seminar: %d %s", res.StatusCode, raw)
	}
	var rc RenumberConflict
	decodeStrict(t, raw, &rc)
	if rc.Error.Code != "calendar.renumber" || len(rc.Changes) != 1 || rc.Changes[0].From != "W02L2" || rc.Changes[0].To != "W02S1" {
		t.Fatalf("conflict %+v", rc)
	}
	var lv Lecture
	e.call("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: model.KindSeminar, Confirm: true}, http.StatusOK, &lv)
	if lv.Label != "W02S1" || lv.Kind != model.KindSeminar || lv.KindSource != KindSourceUser || lv.ShowN {
		t.Fatalf("seminar %+v", lv)
	}
	if l1 := e.lectureView(ls["L1"].ID); l1.Label != "W02L1" || l1.ShowN || l1.KindCount != 1 {
		t.Fatalf("the other lecture of the week %+v", l1)
	}
	// a past recording set by hand stays what the user says
	e.call("PUT", "/api/lectures/"+ls["OLD"].ID+"/kind", LectureKindSet{Kind: model.KindLecture, Confirm: true}, http.StatusOK, &lv)
	if lv.Label != "W??" || lv.Kind != model.KindLecture || lv.KindSource != KindSourceUser || lv.CalendarHint != hintBeforeTerm ||
		!strings.HasPrefix(lv.CiteLabel, "W??-") {
		t.Fatalf("past recording as a lecture %+v", lv)
	}

	// "" = automatic: the file-name rule's kind through its kind_map
	rule := `^C-(?P<kind>[A-Z])\d{2}-\d{2} (?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	e.call("PUT", "/api/courses/"+c.ID+"/filename-rule", FilenamePreviewInput{Regex: rule, Confirm: true,
		KindMap: map[string]string{"L": model.KindLecture, "T": model.KindTutorial}}, http.StatusOK, nil)
	e.call("PUT", "/api/lectures/"+ls["L2"].ID+"/kind", LectureKindSet{Kind: "", Confirm: true}, http.StatusOK, &lv)
	if lv.Kind != model.KindLecture || lv.KindSource != KindSourceFilename || lv.Label != "W02L2" {
		t.Fatalf("automatic again %+v", lv)
	}
	e.call("PUT", "/api/lectures/"+ls["T2"].ID+"/kind", LectureKindSet{Kind: "", Confirm: true}, http.StatusOK, &lv)
	if lv.Kind != model.KindTutorial || lv.KindSource != KindSourceFilename || lv.Label != "W02T2" {
		t.Fatalf("automatic tutorial %+v", lv)
	}
	if st := e.storedLecture(ls["T2"].ID); st.Kind != model.KindTutorial || st.KindSource != model.KindSourceFilename {
		t.Fatalf("stored %s/%s", st.Kind, st.KindSource)
	}
}

func (e *env) storedLecture(id string) model.Lecture {
	e.t.Helper()
	l, err := e.st.GetLecture(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// The renumbering check of PUT filename-rule sees the kinds the rule
// gives (predictKind): a kind_map that turns a lecture into a tutorial
// changes its number.
func TestFilenameRuleKindMap(t *testing.T) {
	e := newEnv(t)
	c, ls := kindCourse(e)
	base := "/api/courses/" + c.ID + "/filename-rule"
	rule := `^C-(?P<kind>[A-Z])\d{2}-\d{2} (?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	var pr FilenamePreviewResult
	e.call("POST", base+"/preview", FilenamePreviewInput{Regex: rule, KindMap: map[string]string{" T ": model.KindTutorial, "": "lecture", "X": "nope"}}, http.StatusOK, &pr)
	rows := map[string]FilenamePreviewRow{}
	for _, r := range pr.Preview {
		rows[r.Filename] = r
	}
	if r := rows["C-T01-01 2026-10-07.mp4"]; r.KindCode != "T" || r.Kind != model.KindTutorial || r.Past {
		t.Errorf("tutorial row %+v", r)
	}
	if r := rows["C-L01-01 2026-10-06.mp4"]; r.KindCode != "L" || r.Kind != "" {
		t.Errorf("unmapped row %+v", r)
	}
	if r := rows["C-L01-01 2024-10-07.mp4"]; !r.Past || r.Status != calendar.StatusOK || r.ErrorInfo != nil {
		t.Errorf("past row %+v", r)
	}
	if !pr.OK {
		t.Errorf("a past recording does not fail the rule: %+v", pr.ProblemsInfo)
	}
	if info := e.callErrInfo("PUT", base, FilenamePreviewInput{Regex: rule, KindMap: map[string]string{"T": "nope"}}, http.StatusBadRequest); info.Code != "kind.unknown" || info.Params["kind"] != "nope" {
		t.Errorf("unknown kind %+v", info)
	}
	if err := stages.MarkCiteExported(context.Background(), e.st, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, raw := e.do("PUT", base, FilenamePreviewInput{Regex: rule, KindMap: map[string]string{"T": model.KindTutorial}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("apply: %d %s", res.StatusCode, raw)
	}
	var rc RenumberConflict
	decodeStrict(t, raw, &rc)
	changed := map[string]string{}
	for _, ch := range rc.Changes {
		changed[ch.LectureID] = ch.From + ">" + ch.To
	}
	// T1, T2 become W02T1, W02T2; L2 moves up to W02L2
	if len(changed) != 3 || changed[ls["T1"].ID] != "W02L2>W02T1" || changed[ls["T2"].ID] != "W02L3>W02T2" || changed[ls["L2"].ID] != "W02L4>W02L2" {
		t.Fatalf("changes %v", changed)
	}
	var cv Course
	e.call("PUT", base, FilenamePreviewInput{Regex: rule, KindMap: map[string]string{"T": model.KindTutorial, " ": "lecture"}, Confirm: true}, http.StatusOK, &cv)
	if cv.FilenameRule == nil || len(cv.FilenameRule.KindMap) != 1 || cv.FilenameRule.KindMap["T"] != model.KindTutorial || cv.FilenameRule.Source != calendar.RuleSourceUser {
		t.Fatalf("saved rule %+v", cv.FilenameRule)
	}
	var v FilenameRuleView
	e.call("GET", base, nil, http.StatusOK, &v)
	if v.KindMap["T"] != model.KindTutorial {
		t.Fatalf("view kind_map %v", v.KindMap)
	}
	for _, r := range v.Preview {
		if r.Filename == "C-T01-02 2026-10-07.mp4" && (r.Kind != model.KindTutorial || r.KindCode != "T") {
			t.Errorf("view row %+v", r)
		}
		if r.Filename == "C-L01-01 2024-10-07.mp4" && !r.Past {
			t.Errorf("view past row %+v", r)
		}
	}
}

// setHasher is a fake summary stage whose input hash covers the ids of
// the lecture's material set (as the real one covers the slides' text).
type setHasher struct{ st db.Store }

func (h setHasher) Name() string                 { return model.StageSummary }
func (h setHasher) Resource() sched.ResourceKind { return sched.ResourceAPI }
func (h setHasher) Run(context.Context, string, func(float64)) error {
	return nil
}
func (h setHasher) InputHash(ctx context.Context, lectureID string) (string, error) {
	uses, err := state.LectureMaterialSet(ctx, h.st, lectureID)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	for _, u := range uses {
		sum.Write([]byte(u.Material.ID + "\n"))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (e *env) uploadMaterial(courseID, filename string, fields [][2]string) Material {
	e.t.Helper()
	res, b := e.upload("/api/courses/"+courseID+"/materials", fields, filename, []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("upload %s: %d %s", filename, res.StatusCode, b)
	}
	var c Course
	decodeStrict(e.t, b, &c)
	for _, m := range c.Materials {
		if m.Filename == filename {
			return m
		}
	}
	e.t.Fatalf("material %s not in %+v", filename, c.Materials)
	return Material{}
}

func TestMaterialsByWeek(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, ls := kindCourse(e)
	h := setHasher{e.st}
	e.sc.Register(h)
	add := func(name, at string) {
		l := e.uploadNamed(c.ID, "C-L09-0"+name+".mp4")
		e.call("PATCH", "/api/lectures/"+l.ID, LecturePatch{RecordedAt: ptr(at)}, http.StatusOK, &l)
		ls[name] = l
	}
	// done finishes lectures with the summary hash of their current sets;
	// then a run that changes nothing (the kind of L1 set again) records
	// every lecture's set as the baseline.
	done := func(names ...string) {
		for _, name := range names {
			l := ls[name]
			e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, nil)
			e.allDone(l.ID)
			j := e.moveJob(model.ScopeLecture, l.ID, model.StageSummary)
			hash, _ := h.InputHash(ctx, l.ID)
			if err := e.st.SetJobInputHash(ctx, j.ID, hash); err != nil {
				t.Fatal(err)
			}
		}
		e.call("PUT", "/api/lectures/"+ls["L1"].ID+"/kind", LectureKindSet{Kind: model.KindLecture}, http.StatusOK, nil)
		e.srv.WaitInvalidations()
	}
	summary := func(name string) StageView {
		t.Helper()
		e.srv.WaitInvalidations()
		return stageOf(e.lectureView(ls[name].ID).Stages, model.StageSummary)
	}
	staleWith := func(name, param string, want ...string) {
		t.Helper()
		s := summary(name)
		if s.Status != model.StatusStale || s.StaleInfo == nil || s.StaleInfo.Code != model.StaleCodeMaterials ||
			!slices.Equal(anyStrings(s.StaleInfo.Params[param]), want) {
			t.Fatalf("%s summary %s %+v, want %s %v", name, s.Status, s.StaleInfo, param, want)
		}
		other := map[string]string{"added": "removed", "removed": "added"}[param]
		if s.StaleInfo.Params[other] != nil {
			t.Fatalf("%s summary %+v: unexpected %s", name, s.StaleInfo, other)
		}
	}
	stillDone := func(name string) {
		t.Helper()
		if s := summary(name); s.Status != model.StatusDone {
			t.Fatalf("%s summary %s %+v", name, s.Status, s.StaleInfo)
		}
	}
	done("L1", "T1", "L3")
	stillDone("L1")

	// upload: a week in the name binds to that week; lecture_id to the
	// lecture; anything else to the whole course
	w2 := e.uploadMaterial(c.ID, "Week2-Block-Ciphers.pdf", nil)
	if w2.Bind == nil || w2.Bind.Scope != BindWeek || w2.Bind.Week == nil || *w2.Bind.Week != 2 || w2.LectureID != nil ||
		w2.WeekSuggested == nil || *w2.WeekSuggested != 2 {
		t.Fatalf("week material %+v", w2)
	}
	staleWith("L1", "added", "Week2-Block-Ciphers.pdf")
	staleWith("T1", "added", "Week2-Block-Ciphers.pdf") // every kind of the week
	stillDone("L3")
	one := e.uploadMaterial(c.ID, "W03-notes.pdf", [][2]string{{"lecture_id", ls["L2"].ID}})
	if one.Bind.Scope != BindLecture || one.Bind.LectureID == nil || *one.Bind.LectureID != ls["L2"].ID || one.LectureID == nil ||
		one.WeekSuggested == nil || *one.WeekSuggested != 3 {
		t.Fatalf("lecture material %+v", one)
	}
	stillDone("L3")
	all := e.uploadMaterial(c.ID, "StreamCiphers-2.pdf", nil)
	if all.Bind.Scope != BindCourse || all.Bind.Week != nil || all.Bind.LectureID != nil || all.WeekSuggested != nil {
		t.Fatalf("course material %+v", all)
	}
	staleWith("L3", "added", "StreamCiphers-2.pdf")
	mats := func(name string) []string {
		var out []string
		for _, m := range e.lectureView(ls[name].ID).Materials {
			w := ""
			if m.Week > 0 {
				w = ":" + strconv.Itoa(m.Week)
			}
			out = append(out, m.Filename+"/"+m.Via+w)
		}
		return out
	}
	if got := mats("L2"); !slices.Equal(got, []string{"W03-notes.pdf/lecture", "Week2-Block-Ciphers.pdf/week:2", "StreamCiphers-2.pdf/course"}) {
		t.Fatalf("L2 materials %v", got)
	}
	if got := mats("OLD"); !slices.Equal(got, []string{"StreamCiphers-2.pdf/course"}) {
		t.Fatalf("a lecture without a week uses no week's slides: %v", got)
	}

	// PATCH bind: week 3 moves the slides from week 2 to week 3
	add("A", "2026-10-14T09:00") // week 3
	add("B", "2026-10-07T10:00") // week 2
	add("C", "2026-10-15T09:00") // week 3
	done("A", "B")
	var m Material
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(3)}}, http.StatusOK, &m)
	if m.Bind.Scope != BindWeek || *m.Bind.Week != 3 || m.LectureID != nil {
		t.Fatalf("bound to week 3 %+v", m)
	}
	staleWith("A", "added", "Week2-Block-Ciphers.pdf")
	staleWith("B", "removed", "Week2-Block-Ciphers.pdf")
	// the same binding again changes nothing
	done("C")
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(3)}}, http.StatusOK, &m)
	stillDone("C")

	// scope lecture / course and the v1 body
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindLecture, LectureID: ptr(ls["L1"].ID)}}, http.StatusOK, &m)
	if m.Bind.Scope != BindLecture || m.LectureID == nil || *m.LectureID != ls["L1"].ID || m.Bind.Week != nil {
		t.Fatalf("bound to a lecture %+v", m)
	}
	staleWith("C", "removed", "Week2-Block-Ciphers.pdf")
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindCourse}}, http.StatusOK, &m)
	if m.Bind.Scope != BindCourse || m.LectureID != nil {
		t.Fatalf("course-wide %+v", m)
	}
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(2)}}, http.StatusOK, &m)
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"lecture_id": ls["L3"].ID}, http.StatusOK, &m)
	if m.Bind.Scope != BindLecture || *m.Bind.LectureID != ls["L3"].ID {
		t.Fatalf("v1 body %+v", m)
	}
	if st, _ := e.st.GetMaterial(ctx, w2.ID); st.BindWeek != nil {
		t.Fatalf("v1 binding kept the week %v", *st.BindWeek)
	}
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"lecture_id": nil}, http.StatusOK, &m)
	if m.Bind.Scope != BindCourse {
		t.Fatalf("v1 null %+v", m)
	}
	other := e.course("Other")
	ol := e.draft(other.ID, "X")
	for _, b := range []struct {
		body any
		code string
	}{
		{map[string]any{"bind": MaterialBind{Scope: "term"}}, "material.bind_invalid"},
		{map[string]any{"bind": "week"}, "material.bind_invalid"},
		{map[string]any{"bind": MaterialBind{Scope: BindLecture}}, "material.bind_invalid"},
		{map[string]any{"bind": MaterialBind{Scope: BindWeek}}, "material.bind_week_invalid"},
		{map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(0)}}, "material.bind_week_invalid"},
		{map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(61)}}, "material.bind_week_invalid"},
		{map[string]any{"bind": MaterialBind{Scope: BindLecture, LectureID: ptr(ol.ID)}}, "material.bind_lecture_other_course"},
		{map[string]any{"bind": MaterialBind{Scope: BindLecture, LectureID: ptr("nope")}}, "material.bind_lecture_other_course"},
		{map[string]any{"lecture_id": ol.ID}, "material.lecture_not_in_course"},
		{map[string]any{}, "material.bind_missing"},
	} {
		if info := e.callErrInfo("PATCH", "/api/materials/"+w2.ID, b.body, http.StatusBadRequest); info.Code != b.code {
			t.Errorf("%v: %+v, want %s", b.body, info, b.code)
		}
	}
	if info := e.callErrInfo("PATCH", "/api/materials/nope", map[string]any{"bind": MaterialBind{Scope: BindCourse}}, http.StatusNotFound); info.Code != "material.not_found" {
		t.Errorf("unknown material %+v", info)
	}

	// the term start moves the teaching weeks: the week's slides follow
	e.call("PATCH", "/api/materials/"+w2.ID, map[string]any{"bind": MaterialBind{Scope: BindWeek, Week: ptr(2)}}, http.StatusOK, &m)
	add("D", "2026-10-09T09:00") // week 2, week 1 after the move
	add("E", "2026-10-16T09:00") // week 3, week 2 after the move
	done("D", "E")
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-10-05")}, http.StatusOK, nil)
	staleWith("D", "removed", "Week2-Block-Ciphers.pdf")
	staleWith("E", "added", "Week2-Block-Ciphers.pdf")
	// a recording time moves one lecture
	add("F", "2026-10-21T09:00") // week 3
	add("G", "2026-10-22T09:00") // week 3
	done("F", "G")
	e.call("PATCH", "/api/lectures/"+ls["F"].ID, LecturePatch{RecordedAt: ptr("2026-10-14T09:00")}, http.StatusOK, nil)
	staleWith("F", "added", "Week2-Block-Ciphers.pdf")
	stillDone("G")
	// deleting slides
	e.call("DELETE", "/api/materials/"+all.ID, nil, http.StatusNoContent, nil)
	staleWith("G", "removed", "StreamCiphers-2.pdf")
}

// The materials cause is in causeOrder: a request merged into a running
// invalidation is run afterwards, not dropped.
func TestInvalidatorKeepsMaterialsCause(t *testing.T) {
	if !slices.Contains(causeOrder, state.StaleMaterials) {
		t.Fatal("causeOrder lacks state.StaleMaterials")
	}
	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	iv := newInvalidator(func(_ string, c state.StaleCause) {
		if c.Kind == state.StaleGlossary {
			<-release
		}
		mu.Lock()
		ran = append(ran, c.Kind)
		mu.Unlock()
	})
	iv.request("c", state.StaleCause{Kind: state.StaleGlossary})
	iv.request("c", state.StaleCause{Kind: state.StaleMaterials})
	iv.request("c", state.StaleCause{Kind: state.StaleMaterial, Material: "a.pdf"})
	close(release)
	iv.wait()
	if !slices.Equal(ran, []string{state.StaleGlossary, state.StaleMaterial, state.StaleMaterials}) {
		t.Fatalf("ran %v", ran)
	}
}

// The dashboard's reviews and failures carry the kind and number; failures
// the course code.
func TestDashboardKindTags(t *testing.T) {
	e := newEnv(t)
	c, ls := kindCourse(e)
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Code: ptr("CSC1002")}, http.StatusOK, nil)
	t2 := ls["T2"]
	e.call("PUT", "/api/lectures/"+ls["T1"].ID+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, nil)
	e.call("PUT", "/api/lectures/"+t2.ID+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, nil)
	e.call("POST", "/api/lectures/"+t2.ID+"/launch", nil, http.StatusOK, nil)
	e.moveJob(model.ScopeLecture, t2.ID, model.StagePrep, "queued", "running", "failed")
	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	var f *FailureItem
	for i := range d.Failures {
		if d.Failures[i].LectureID == t2.ID {
			f = &d.Failures[i]
		}
	}
	if f == nil || f.CourseCode == nil || *f.CourseCode != "CSC1002" || f.Kind != model.KindTutorial || f.Week != 2 || f.KindN != 2 || !f.ShowN || f.Label != "W02T2" {
		t.Fatalf("failure %+v", f)
	}
}

// Course.renumber_notices: the automatic renumbering notices stages records
// (kv renumbered_auto:{course}); DELETE dismisses them.
func TestRenumberNotices(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.RenumberNotices != nil {
		t.Fatalf("no notices: %+v", cv.RenumberNotices)
	}
	raw := `[{"code":"calendar.renumbered_auto","params":{"lecture":"L1","n":1,"changes":[{"lecture_id":"L2","from":"W02L2","to":"W02L1"}]},"detail":"x","at":"2026-10-24T01:00:00Z"}]`
	if err := e.st.SetKV(context.Background(), stages.KVRenumberedAutoPrefix+c.ID, raw); err != nil {
		t.Fatal(err)
	}
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if len(cv.RenumberNotices) != 1 || cv.RenumberNotices[0].Code != stages.CodeRenumberedAuto || cv.RenumberNotices[0].At != "2026-10-24T01:00:00Z" ||
		cv.RenumberNotices[0].Params["n"] != float64(1) {
		t.Fatalf("notices %+v", cv.RenumberNotices)
	}
	cv = Course{}
	e.call("DELETE", "/api/courses/"+c.ID+"/renumber-notices", nil, http.StatusOK, &cv)
	if cv.ID != c.ID || cv.RenumberNotices != nil {
		t.Fatalf("dismissed %+v", cv.RenumberNotices)
	}
	if info := e.callErrInfo("DELETE", "/api/courses/nope/renumber-notices", nil, http.StatusNotFound); info.Code != "course.not_found" {
		t.Fatalf("unknown course %+v", info)
	}
}
