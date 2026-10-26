// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// "只转写" (SPEC-v3 V4.2 step 4, 3.1.0; NOTES.md): launching without any
// provider, the waiting_for of the API stages, glossary_pending, and the
// on-demand English files.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
	"lectureflow/internal/subs"
)

// clearRoles removes the provider of every role (the "只转写" preset never
// assigns one).
func (e *env) clearRoles() {
	e.t.Helper()
	ctx := context.Background()
	for _, r := range model.Roles {
		if err := e.st.UpsertRoleSetting(ctx, model.RoleSetting{Role: r}); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.set.Load(ctx); err != nil {
		e.t.Fatal(err)
	}
	if !e.sc.TranscribeOnly() {
		e.t.Fatal("roles still configured")
	}
}

func TestTranscribeOnlyLaunchAPI(t *testing.T) {
	e := newEnv(t)
	e.clearRoles()
	c := e.course("C")
	l := e.draft(c.ID, "L1")
	var got Lecture
	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, &got)
	if got.Draft || got.LaunchedAt == nil {
		t.Fatalf("not launched: %+v", got)
	}
	// Batch launch too (it checks the settings once first).
	l2 := e.draft(c.ID, "L2")
	var br BatchResponse
	e.call("POST", "/api/lectures/launch", BatchRequest{IDs: []string{l2.ID}}, http.StatusOK, &br)
	if len(br.Results) != 1 || !br.Results[0].OK {
		t.Fatalf("batch launch %+v", br)
	}
	// "启动全部草稿"
	l3 := e.draft(c.ID, "L3")
	e.call("POST", "/api/courses/"+c.ID+"/launch-drafts", nil, http.StatusOK, nil)
	if v := e.lectureView(l3.ID); v.Draft {
		t.Fatal("launch-drafts left a draft")
	}

	// asr done (the scheduler is not started: the jobs are moved by hand).
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.insertTranscript(l.ID, true)
	v := e.lectureView(l.ID)
	if !v.Playable {
		t.Fatal("asr done: playable")
	}
	sum := stageOf(v.Stages, model.StageSummary)
	if sum.Status != model.StatusPending || sum.WaitingFor == nil || *sum.WaitingFor != sched.WaitNoProvider {
		t.Fatalf("summary %+v", sum)
	}
	if w := stageOf(v.Stages, model.StageRepair).WaitingFor; w == nil || *w != model.WaitUpstream(model.StageSummary) {
		t.Fatalf("repair waiting_for %v", w)
	}
	if len(v.Exports) != 0 {
		t.Fatalf("exports %+v", v.Exports)
	}

	// Some roles but not all: refused as before.
	e.configureRole(model.RoleSummary)
	l4 := e.draft(c.ID, "L4")
	if info := e.callErrInfo("POST", "/api/lectures/"+l4.ID+"/launch", nil, http.StatusConflict); info.Code != settings.CodeRoleNotConfigured {
		t.Fatalf("partial roles: %+v", info)
	}
	if info := e.callErrInfo("POST", "/api/lectures/launch", BatchRequest{IDs: []string{l4.ID}}, http.StatusConflict); info.Code != settings.CodeRoleNotConfigured {
		t.Fatalf("partial roles, batch: %+v", info)
	}
	// The launched lecture still waits: a deferred lecture needs every role.
	if w := stageOf(e.lectureView(l.ID).Stages, model.StageSummary).WaitingFor; w == nil || *w != sched.WaitNoProvider {
		t.Fatalf("partial roles: summary waiting_for %v", w)
	}
	// Every role: nothing waits for a provider any more.
	e.configureRole(model.RoleRepair)
	e.configureRole(model.RoleTranslate)
	if w := stageOf(e.lectureView(l.ID).Stages, model.StageSummary).WaitingFor; w != nil {
		t.Fatalf("all roles: summary waiting_for %q", *w)
	}
}

// Without a term extraction role the term jobs do not count as pending
// glossary (asr does not wait for them).
func TestTranscribeOnlyGlossaryPending(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	if err := e.st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: "s.md", Path: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := state.ResetGlossarySeed(ctx, e.st, c.ID); err != nil {
		t.Fatal(err)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if !cv.GlossaryPending {
		t.Fatal("glossary role configured: glossary_pending")
	}
	e.clearRoles()
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.GlossaryPending {
		t.Fatal("no glossary role: glossary_pending must be false")
	}
	if g := stageOf(cv.Stages, model.StageGlossarySeed); g.WaitingFor == nil || *g.WaitingFor != sched.WaitNoProvider {
		t.Fatalf("glossary_seed %+v", g)
	}
}

func TestEnglishFilesAPI(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "Block Ciphers")
	base := "/api/lectures/" + l.ID + "/english/"
	if info := e.callErrInfo("GET", base+"vtt", nil, http.StatusNotFound); info.Code != "export.not_found" {
		t.Fatalf("no sentences: %+v", info)
	}
	e.insertTranscript(l.ID, true)
	ctx := context.Background()
	ss, err := e.st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	res, b := e.do("GET", base+"vtt", nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Fatalf("vtt: %d %s %s", res.StatusCode, res.Header.Get("Content-Type"), b)
	}
	var want strings.Builder
	if err := stages.WriteEnglishVTT(&want, ss); err != nil {
		t.Fatal(err)
	}
	if string(b) != want.String() || !strings.HasPrefix(string(b), "WEBVTT\n\n1\n") {
		t.Fatalf("vtt body:\n%s", b)
	}
	if n, cues := strings.Count(string(b), " --> "), subs.SegmentLecture(stages.CueSentences(ss)); n != len(cues) {
		t.Fatalf("%d cues, the player has %d", n, len(cues))
	}
	v := e.lectureView(l.ID)
	label := v.CiteLabel
	if label == "" {
		label = v.Label
	}
	wantName := stages.ExportBase(label, "Block Ciphers") + ".en.vtt"
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="`+asciiFileName(wantName)+`"`) {
		t.Fatalf("Content-Disposition %q, want the name %q", cd, wantName)
	}

	res, b = e.do("GET", base+"txt", nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		string(b) != "A block cipher.\nIt works on blocks.\n" {
		t.Fatalf("txt: %d %q", res.StatusCode, b)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, ".en.txt") {
		t.Fatalf("txt Content-Disposition %q", cd)
	}
	if info := e.callErrInfo("GET", base+"srt", nil, http.StatusBadRequest); info.Code != "export.bad_name" {
		t.Fatalf("srt: %+v", info)
	}
	if info := e.callErrInfo("GET", "/api/lectures/nope/english/vtt", nil, http.StatusNotFound); info.Code != "lecture.not_found" {
		t.Fatalf("unknown lecture: %+v", info)
	}
	// Without a title: the uploaded file's name.
	if err := e.st.SetLectureTitle(ctx, l.ID, ""); err != nil {
		t.Fatal(err)
	}
	res, _ = e.do("GET", base+"txt", nil)
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, " rec.en.txt") {
		t.Fatalf("untitled: Content-Disposition %q", cd)
	}
}
