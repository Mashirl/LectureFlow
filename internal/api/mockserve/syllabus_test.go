// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// fastSyllabus shortens the simulated work of syllabus.go for one test.
func fastSyllabus(t *testing.T) {
	t.Helper()
	old := sylTiming
	sylTiming.format, sylTiming.queued, sylTiming.run, sylTiming.extract, sylTiming.term = 0, 20*time.Millisecond, 150*time.Millisecond, 20*time.Millisecond, 60*time.Millisecond
	sylTiming.matQueued, sylTiming.slowTerms = 20*time.Millisecond, 50*time.Millisecond
	sylTiming.reviewQueued, sylTiming.review = 20*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { sylTiming = old })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// fileBody is a multipart body with one "file" part holding content.
func fileBody(t *testing.T, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func postFile(t *testing.T, url, filename, content string, want int, out any) {
	t.Helper()
	body, ctype := fileBody(t, filename, content)
	res, err := http.Post(url, ctype, body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("POST %s (%s): status %d, want %d: %s", url, filename, res.StatusCode, want, b)
	}
	if out != nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			t.Fatalf("POST %s: %v\n%s", url, err, b)
		}
	}
}

func strp(s string) *string { return &s }

func TestCourseCreateAndPatchValidation(t *testing.T) {
	srv := testServer(t)
	var e api.ErrorBody
	doJSON(t, "POST", srv.URL+"/api/courses", api.CourseCreate{Name: "  "}, 400, &e)
	doJSON(t, "POST", srv.URL+"/api/courses", api.CourseCreate{Name: "Crypto", Code: strp("CSC 3631")}, 400, &e)
	if !strings.Contains(e.Error.Detail, "空格") {
		t.Errorf("code with a space: %q", e.Error.Detail)
	}
	doJSON(t, "POST", srv.URL+"/api/courses", api.CourseCreate{Name: strings.Repeat("长", 201)}, 400, &e)
	var c api.Course
	doJSON(t, "POST", srv.URL+"/api/courses", api.CourseCreate{Name: " Applied Cryptography ", Code: strp("CSC4000")}, 201, &c)
	if c.Name != "Applied Cryptography" || c.Code == nil || *c.Code != "CSC4000" || c.Syllabus != nil || len(c.Stages) != 0 || c.GlossaryPending || c.TermCount != 0 {
		t.Fatalf("created %+v", c)
	}
	var p api.Course
	doJSON(t, "PATCH", srv.URL+"/api/courses/"+c.ID, api.CoursePatch{Code: strp("")}, 200, &p)
	if p.Code != nil {
		t.Errorf("code not cleared: %v", *p.Code)
	}
	doJSON(t, "PATCH", srv.URL+"/api/courses/"+c.ID, map[string]any{}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/courses/"+c.ID, api.CoursePatch{Code: strp("A B")}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/courses/NOPE", api.CoursePatch{Name: strp("x")}, 404, &e)
	var terms []api.Term
	doJSON(t, "GET", srv.URL+"/api/courses/"+c.ID+"/terms", nil, 200, &terms)
	if terms == nil || len(terms) != 0 {
		t.Errorf("new course terms %v", terms)
	}
}

// The wizard's steps 2–3 (A8.1) and the save of A8.2: raw text, format
// (not saved), save → glossary_seed runs, terms appear, manual terms stay.
func TestSyllabusWizardFlow(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	var c api.Course
	doJSON(t, "POST", srv.URL+"/api/courses", api.CourseCreate{Name: "Cryptography", Code: strp("CSC9999")}, 201, &c)
	base := srv.URL + "/api/courses/" + c.ID
	var doc api.SyllabusDoc
	doJSON(t, "GET", base+"/syllabus", nil, 200, &doc)
	if doc != (api.SyllabusDoc{}) {
		t.Fatalf("empty course syllabus %+v", doc)
	}
	var e api.ErrorBody
	doJSON(t, "POST", base+"/syllabus/format", nil, 409, &e)
	doJSON(t, "POST", base+"/syllabus/raw", api.SyllabusRawInput{Text: "  \n"}, 400, &e)

	// a manual term must survive glossary_seed
	var manual api.Term
	doJSON(t, "POST", base+"/terms", map[string]string{"en": "Stream ciphers", "zh": "自定义译法"}, 201, &manual)

	raw := "CSC9999 Cryptography\nCredits: 10\nAssessment: Coursework 30%, Exam 70%\n\nLearning outcomes\n- explain perfect secrecy;\n\nWeekly schedule\nWeek 1: Stream ciphers & LFSRs; block ciphers\nWeek 2 - Hash functions\n"
	doJSON(t, "POST", base+"/syllabus/raw", api.SyllabusRawInput{Text: raw}, 200, &doc)
	if doc.Raw != raw || doc.Filename != "pasted.txt" || doc.Markdown != "" {
		t.Fatalf("raw saved %+v", doc)
	}
	var f api.SyllabusFormatResult
	doJSON(t, "POST", base+"/syllabus/format", nil, 200, &f)
	for _, want := range []string{"# CSC9999 Cryptography\n", "## Course Information", "**Credits:** 10", "## Learning Outcomes", "- explain perfect secrecy", "## Weekly Topics", "| 1 | Stream ciphers & LFSRs | block ciphers |", "| 2 | Hash functions |", "## Assessment", "- Exam 70%", "## Key Terms", "LFSRs"} {
		if !strings.Contains(f.Markdown, want) {
			t.Errorf("formatted markdown lacks %q:\n%s", want, f.Markdown)
		}
	}
	if f.Usage.PromptTokens <= 0 || f.Usage.CompletionTokens <= 0 {
		t.Errorf("usage %+v", f.Usage)
	}
	doJSON(t, "GET", base+"/syllabus", nil, 200, &doc)
	if doc.Markdown != "" {
		t.Error("format must not save SYLLABUS.md")
	}

	doJSON(t, "PUT", base+"/syllabus", api.SyllabusSaveInput{Markdown: " "}, 400, &e)
	var saved api.Course
	doJSON(t, "PUT", base+"/syllabus", api.SyllabusSaveInput{Markdown: f.Markdown}, 200, &saved)
	if saved.Syllabus == nil || !saved.Syllabus.Formatted || saved.Syllabus.Filename != "pasted.txt" || !saved.GlossaryPending || len(saved.Stages) != 1 || saved.Stages[0].Status != model.StatusQueued || saved.Stages[0].WaitingFor == nil {
		t.Fatalf("after save %+v", saved)
	}
	doJSON(t, "GET", base+"/syllabus", nil, 200, &doc)
	if doc.Markdown != f.Markdown {
		t.Error("SYLLABUS.md not saved")
	}

	get := func() api.Course {
		var x api.Course
		doJSON(t, "GET", base, nil, 200, &x)
		return x
	}
	eventually(t, "glossary_seed running", func() bool { return get().Stages[0].Status == model.StatusRunning })
	doJSON(t, "PUT", base+"/syllabus", api.SyllabusSaveInput{Markdown: f.Markdown}, 409, &e)
	if e.Error.Detail != syllabusBusy {
		t.Errorf("409 text %q", e.Error.Detail)
	}
	eventually(t, "glossary_seed done", func() bool { return get().Stages[0].Status == model.StatusDone })
	// v2 G3.1 / lead ruling 4: with a janitor configured the merge queues a
	// glossary_review; asr waits for it, so the glossary is pending until then.
	eventually(t, "glossary_review done", func() bool {
		c := get()
		return len(c.Stages) == 2 && c.Stages[1].Stage == model.StageGlossaryReview && c.Stages[1].Status == model.StatusDone
	})
	done := get()
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	if done.GlossaryPending || done.TermCount != len(terms) || len(terms) < 3 || done.GlossaryVersion < 2 {
		t.Fatalf("after glossary_seed: %+v, %d terms", done, len(terms))
	}
	var fromSyllabus int
	for _, tm := range terms {
		if strings.EqualFold(tm.En, "stream ciphers") && (tm.Source != model.SourceManual || tm.Zh != "自定义译法") {
			t.Errorf("manual term overwritten: %+v", tm)
		}
		if tm.Source == model.SourceSyllabus {
			fromSyllabus++
		}
	}
	if fromSyllabus == 0 {
		t.Errorf("no syllabus terms in %+v", terms)
	}
}

func TestSyllabusRawUpload(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var doc api.SyllabusDoc
	doJSON(t, "GET", base+"/syllabus", nil, 200, &doc)
	if doc.Filename == "" || !strings.Contains(doc.Raw, "Week 3") || !strings.HasPrefix(doc.Markdown, "# CSC1003 Cryptography") {
		t.Fatalf("fixture syllabus %+v", doc)
	}
	postFile(t, base+"/syllabus/raw", "outline.md", "# Outline\nWeek 1: Ciphers\n", 200, &doc)
	if doc.Filename != "outline.md" || doc.Raw != "# Outline\nWeek 1: Ciphers\n" {
		t.Errorf("txt upload %+v", doc)
	}
	var c api.Course
	doJSON(t, "GET", base, nil, 200, &c)
	if c.Syllabus.Filename != "outline.md" || !c.Syllabus.Formatted {
		t.Errorf("course syllabus after raw upload %+v", c.Syllabus)
	}
	postFile(t, base+"/syllabus/raw", "outline.pdf", "%PDF-1.7", 200, &doc)
	if !strings.Contains(doc.Raw, "Weekly schedule") {
		t.Errorf("pdf upload raw %q", doc.Raw)
	}
	var e api.ErrorBody
	postFile(t, base+"/syllabus/raw", "outline.docx", "x", 400, &e)
	postFile(t, base+"/syllabus/raw", "fail.txt", "x", 500, &e)
	postFile(t, base+"/syllabus/raw", "scan.pdf", "x", 422, &e)
	postFile(t, base+"/syllabus/raw", "empty.txt", " \n", 422, &e)
	postFile(t, srv.URL+"/api/courses/NOPE/syllabus/raw", "a.txt", "x", 404, &e)
	doJSON(t, "GET", srv.URL+"/api/courses/NOPE/syllabus", nil, 404, &e)

	// legacy upload of a known course: raw text used unformatted, glossary_seed again
	var legacy api.Course
	postFile(t, base+"/syllabus", "old.txt", "Week 1: Ciphers", 200, &legacy)
	if legacy.Syllabus == nil || legacy.Syllabus.Formatted || legacy.Stages[0].Status != model.StatusQueued {
		t.Errorf("legacy upload %+v", legacy)
	}
}

func TestFormatSyllabusErrors(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var e api.ErrorBody
	doJSON(t, "POST", base+"/syllabus/format?mock=unset", nil, 409, &e)
	if !strings.Contains(e.Error.Detail, "大纲整理") {
		t.Errorf("unconfigured role: %q", e.Error.Detail)
	}
	var doc api.SyllabusDoc
	doJSON(t, "POST", base+"/syllabus/raw", api.SyllabusRawInput{Text: "mock-format-fail"}, 200, &doc)
	doJSON(t, "POST", base+"/syllabus/format", nil, 502, &e)
	if !strings.HasPrefix(e.Error.Detail, "大纲整理失败") {
		t.Errorf("format failure: %q", e.Error.Detail)
	}
}

func TestTermsCRUD(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var c api.Course
	doJSON(t, "GET", base, nil, 200, &c)
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	disabled := 0
	for _, tm := range terms {
		if !tm.Enabled {
			disabled++
		}
	}
	if len(terms) < 20 || c.TermCount != len(terms) || disabled == 0 {
		t.Fatalf("fixture glossary: %d terms, term_count %d, %d disabled", len(terms), c.TermCount, disabled)
	}
	var other []api.Term
	doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(c2120)+"/terms", nil, 200, &other)
	if len(other) == 0 || other[0].ID == terms[0].ID {
		t.Error("glossaries must be per course")
	}
	var e api.ErrorBody
	doJSON(t, "POST", base+"/terms", map[string]string{"en": " aes "}, 409, &e)
	doJSON(t, "POST", base+"/terms", map[string]string{"en": "   "}, 400, &e)
	var added api.Term
	doJSON(t, "POST", base+"/terms", map[string]string{"en": "zero  knowledge proof", "zh": " 零知识证明 "}, 201, &added)
	if added.En != "zero knowledge proof" || added.Zh != "零知识证明" || added.Source != model.SourceManual || !added.Enabled {
		t.Errorf("added %+v", added)
	}
	var c2 api.Course
	doJSON(t, "GET", base, nil, 200, &c2)
	if c2.GlossaryVersion != c.GlossaryVersion+1 || c2.TermCount != c.TermCount+1 {
		t.Errorf("version %d→%d, count %d→%d", c.GlossaryVersion, c2.GlossaryVersion, c.TermCount, c2.TermCount)
	}
	var p api.Term
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+added.ID, map[string]any{"zh": "零知识证明（ZKP）"}, 200, &p)
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+added.ID, map[string]any{"enabled": false}, 200, &p)
	if p.Zh != "零知识证明（ZKP）" || p.Enabled {
		t.Errorf("patched %+v", p)
	}
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+added.ID, map[string]any{}, 400, &e)
	doJSON(t, "DELETE", srv.URL+"/api/terms/"+added.ID, nil, 204, nil)
	doJSON(t, "DELETE", srv.URL+"/api/terms/"+added.ID, nil, 404, &e)
	doJSON(t, "GET", srv.URL+"/api/courses/NOPE/terms", nil, 404, &e)
	var c3 api.Course
	doJSON(t, "GET", base, nil, 200, &c3)
	if c3.GlossaryVersion != c.GlossaryVersion+4 || c3.TermCount != c.TermCount {
		t.Errorf("after edits: version %d (from %d), count %d", c3.GlossaryVersion, c.GlossaryVersion, c3.TermCount)
	}
}

func TestMaterialsFlow(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	cid := courseID(c3631)
	base := srv.URL + "/api/courses/" + cid
	var before api.Course
	doJSON(t, "GET", base, nil, 200, &before)
	var c api.Course
	postFile(t, base+"/materials", "week5-rsa.pdf", "%PDF", 201, &c)
	if len(c.Materials) != len(before.Materials)+1 {
		t.Fatalf("materials %d → %d", len(before.Materials), len(c.Materials))
	}
	m := c.Materials[len(c.Materials)-1]
	if m.Filename != "week5-rsa.pdf" || m.LectureID != nil || m.Stages[0].Status != model.StatusQueued || m.Stages[1].Status != model.StatusPending || !c.GlossaryPending {
		t.Fatalf("new material %+v (glossary_pending %v)", m, c.GlossaryPending)
	}
	var e api.ErrorBody
	postFile(t, base+"/materials", "slides.pptx", "x", 400, &e)
	get := func() api.Material {
		var x api.Course
		doJSON(t, "GET", base, nil, 200, &x)
		for _, mm := range x.Materials {
			if mm.ID == m.ID {
				return mm
			}
		}
		t.Fatal("material vanished")
		return api.Material{}
	}
	eventually(t, "material_terms done", func() bool { return get().Stages[1].Status == model.StatusDone })
	if got := get(); got.Pages == 0 || !got.CanDelete {
		t.Errorf("finished material %+v", got)
	}
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	if len(terms) <= before.TermCount {
		t.Errorf("material terms not merged: %d terms (before %d)", len(terms), before.TermCount)
	}

	var bound api.Material
	lec := lecID(c3631, 5)
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+m.ID, map[string]any{"lecture_id": lec}, 200, &bound)
	if bound.LectureID == nil || *bound.LectureID != lec {
		t.Errorf("bound %+v", bound)
	}
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+m.ID, map[string]any{"lecture_id": lecID(c2120, 1)}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+m.ID, map[string]any{}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/materials/"+m.ID, map[string]any{"lecture_id": nil}, 200, &bound)
	if bound.LectureID != nil {
		t.Error("not course-wide again")
	}
	// a material whose stage runs cannot be deleted (fixture: COMP2120 week4-paging.pdf)
	doJSON(t, "DELETE", srv.URL+"/api/materials/01JM000000000000000021200B", nil, 409, &e)
	doJSON(t, "DELETE", srv.URL+"/api/materials/"+m.ID, nil, 204, nil)
	doJSON(t, "DELETE", srv.URL+"/api/materials/"+m.ID, nil, 404, &e)
}

func TestCourseAndMaterialStageActions(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	rm := srv.URL + "/api/courses/" + courseID(cRM)
	var c api.Course
	var e api.ErrorBody
	doJSON(t, "POST", rm+"/stages/glossary_seed/rerun", nil, 409, &e)
	doJSON(t, "POST", rm+"/stages/glossary_seed/cancel", nil, 200, &c)
	if c.Stages[0].Status != model.StatusCancelled || strings.Join(c.Stages[0].AllowedActions, ",") != model.ActionRetry {
		t.Fatalf("cancelled %+v", c.Stages[0])
	}
	doJSON(t, "POST", rm+"/stages/glossary_seed/retry", nil, 200, &c)
	if c.Stages[0].Status != model.StatusQueued {
		t.Errorf("retried %+v", c.Stages[0])
	}
	doJSON(t, "POST", rm+"/stages/material_terms/retry", nil, 400, &e)

	mat := srv.URL + "/api/materials/01JM000000000000000021200B"
	var m api.Material
	doJSON(t, "POST", mat+"/stages/material_terms/cancel", nil, 200, &m)
	if m.Stages[1].Status != model.StatusCancelled || !m.CanDelete {
		t.Fatalf("cancelled material %+v", m)
	}
	doJSON(t, "POST", mat+"/stages/material_terms/retry", nil, 200, &m)
	if m.Stages[0].Status != model.StatusDone || m.Stages[1].Status != model.StatusQueued || m.Stages[1].WaitingFor == nil {
		t.Errorf("retried material %+v", m.Stages)
	}
	doJSON(t, "POST", mat+"/stages/glossary_seed/retry", nil, 400, &e)
}

// The example data's term_count equals the mock glossary of each course, in
// the files (course list, dashboard cards) and in what the server answers,
// without any adjustment at start-up.
func TestFixtureTermCounts(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "api")
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var courses []api.Course
	read("v11_courses.json", &courses)
	var dash api.Dashboard
	read("v11_dashboard.json", &dash)
	want := map[string]int{}
	for i := range courses {
		c := &courses[i]
		want[c.ID] = len(fixtureTerms(c))
		if c.TermCount != want[c.ID] {
			t.Errorf("v11_courses.json %s: term_count %d, glossary has %d terms", deref(c.Code), c.TermCount, want[c.ID])
		}
	}
	if len(dash.Courses) != len(courses) {
		t.Fatalf("dashboard has %d course cards, want %d", len(dash.Courses), len(courses))
	}
	for _, cc := range dash.Courses {
		if cc.TermCount != want[cc.ID] {
			t.Errorf("v11_dashboard.json %s: term_count %d, glossary has %d terms", deref(cc.Code), cc.TermCount, want[cc.ID])
		}
	}

	srv := testServer(t)
	var live api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard", nil, 200, &live)
	for _, cc := range live.Courses {
		var terms []api.Term
		doJSON(t, "GET", srv.URL+"/api/courses/"+cc.ID+"/terms", nil, 200, &terms)
		var c api.Course
		doJSON(t, "GET", srv.URL+"/api/courses/"+cc.ID, nil, 200, &c)
		if cc.TermCount != len(terms) || c.TermCount != len(terms) || len(terms) != want[cc.ID] {
			t.Errorf("%s: dashboard %d, course %d, GET terms %d, fixture %d", cc.Name, cc.TermCount, c.TermCount, len(terms), want[cc.ID])
		}
	}
}

// ?mock=error: the glossary and SYLLABUS.md reads fail (the course page's
// in-card error states) while the course itself still loads; ?mock=slow:
// the glossary arrives after the other reads.
func TestScenariosGlossaryAndSyllabus(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var e api.ErrorBody
	doJSON(t, "GET", base+"/terms?mock=error", nil, 500, &e)
	if !strings.Contains(e.Error.Detail, "mock 模拟的失败") {
		t.Errorf("terms error %q", e.Error.Detail)
	}
	doJSON(t, "GET", base+"/syllabus?mock=error", nil, 500, &e)
	var c api.Course
	doJSON(t, "GET", base+"?mock=error", nil, 200, &c)
	var terms []api.Term
	doJSON(t, "GET", base+"/terms?mock=1", nil, 200, &terms)
	if len(terms) == 0 {
		t.Error("default scenario lost the glossary")
	}

	t0 := time.Now()
	doJSON(t, "GET", base+"?mock=slow", nil, 200, &c)
	course := time.Since(t0)
	t0 = time.Now()
	var slow []api.Term
	doJSON(t, "GET", base+"/terms?mock=slow", nil, 200, &slow)
	if d := time.Since(t0); d < course+sylTiming.slowTerms-10*time.Millisecond || len(slow) != len(terms) {
		t.Errorf("slow glossary: %v (course %v), %d terms", d, course, len(slow))
	}
}

// A slide named "*corrupt*" fails material_extract once with the worker's
// error; retry continues the same job (attempt 1) and then succeeds.
func TestMaterialExtractFailureAndRetry(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var c api.Course
	postFile(t, base+"/materials", "week6-corrupt-scan.pdf", "%PDF", 201, &c)
	mid := c.Materials[len(c.Materials)-1].ID
	get := func() api.Material {
		var x api.Course
		doJSON(t, "GET", base, nil, 200, &x)
		for _, m := range x.Materials {
			if m.ID == mid {
				return m
			}
		}
		t.Fatal("material vanished")
		return api.Material{}
	}
	eventually(t, "material_extract failed", func() bool { return get().Stages[0].Status == model.StatusFailed })
	m := get()
	ex, tm := m.Stages[0], m.Stages[1]
	if ex.Error == nil || !strings.Contains(*ex.Error, "corrupt") || strings.Join(ex.AllowedActions, ",") != model.ActionRetry || ex.DisplayStatus != model.StatusFailed {
		t.Fatalf("failed stage %+v", ex)
	}
	if tm.Status != model.StatusPending || !m.CanDelete || m.Pages != 0 {
		t.Errorf("after the failure: terms %s, can_delete %v, pages %d", tm.Status, m.CanDelete, m.Pages)
	}
	var e api.ErrorBody
	doJSON(t, "POST", srv.URL+"/api/materials/"+mid+"/stages/material_extract/rerun", nil, 409, &e)
	var r api.Material
	doJSON(t, "POST", srv.URL+"/api/materials/"+mid+"/stages/material_extract/retry", nil, 200, &r)
	if r.Stages[0].Status != model.StatusQueued || r.Stages[0].Attempt != 1 || r.Stages[0].Error != nil {
		t.Fatalf("retried %+v", r.Stages[0])
	}
	eventually(t, "material_terms done after the retry", func() bool { return get().Stages[1].Status == model.StatusDone })
	if got := get(); got.Stages[0].Status != model.StatusDone || got.Pages == 0 || !got.CanDelete {
		t.Errorf("finished %+v", got)
	}
}
