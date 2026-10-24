// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/cite"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

func TestCourseCodeAndPatch(t *testing.T) {
	e := newEnv(t)
	var c Course
	e.callErr("POST", "/api/courses", CourseCreate{Name: "Crypto", Code: ptr("CSC 3631")}, http.StatusBadRequest)
	e.call("POST", "/api/courses", CourseCreate{Name: " Cryptography ", Code: ptr(" CSC1003 ")}, http.StatusCreated, &c)
	if c.Name != "Cryptography" || c.Code == nil || *c.Code != "CSC1003" || c.LastActivityAt == "" || c.CourseActions == nil {
		t.Fatalf("created %+v", c)
	}
	e.callErr("PATCH", "/api/courses/"+c.ID, CoursePatch{}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/courses/"+c.ID, CoursePatch{Name: ptr("  ")}, http.StatusBadRequest)
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Name: ptr("Applied Cryptography")}, http.StatusOK, &c)
	if c.Name != "Applied Cryptography" || *c.Code != "CSC1003" {
		t.Fatalf("renamed %+v", c)
	}
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Code: ptr("")}, http.StatusOK, &c)
	if c.Code != nil {
		t.Fatalf("code not cleared %+v", c)
	}
	e.callErr("PATCH", "/api/courses/nope", CoursePatch{Name: ptr("x")}, http.StatusNotFound)
}

// A8: raw text → syllabus_format → SYLLABUS.md → glossary_seed.
func TestSyllabusWizardFlow(t *testing.T) {
	pages := []string{"Week 1: caches", "", "Week 2: write-back"}
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		s.extractPDF = func(_ context.Context, _, pdf string) ([]string, error) {
			if strings.Contains(pdf, "upload") && strings.HasSuffix(pdf, ".pdf") {
				return pages, nil
			}
			return nil, errors.New("unexpected file " + pdf)
		}
	}})
	ctx := context.Background()
	var c Course
	e.call("POST", "/api/courses", CourseCreate{Name: "Cryptography", Code: ptr("CSC1003")}, http.StatusCreated, &c)
	base := "/api/courses/" + c.ID
	l := e.lecture(c.ID, "Week 1")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	asrWait := func() string {
		var lv Lecture
		e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		if w := stageOf(lv.Stages, "asr").WaitingFor; w != nil {
			return *w
		}
		return ""
	}

	var doc SyllabusDoc
	e.call("GET", base+"/syllabus", nil, http.StatusOK, &doc)
	if doc != (SyllabusDoc{}) {
		t.Fatalf("empty doc %+v", doc)
	}
	e.callErr("POST", base+"/syllabus/format", nil, http.StatusConflict)
	e.callErr("POST", base+"/syllabus/raw", SyllabusRawInput{Text: "  "}, http.StatusBadRequest)
	raw := "CSC1003 Cryptography\nWeek 1: block ciphers\n"
	e.call("POST", base+"/syllabus/raw", SyllabusRawInput{Text: raw}, http.StatusOK, &doc)
	if doc.Filename != "pasted.txt" || doc.Raw != raw || doc.Markdown != "" {
		t.Fatalf("raw doc %+v", doc)
	}
	if b, err := os.ReadFile(filepath.Join(model.CourseDir(e.cfg.DataDir, c.ID), "syllabus.raw.txt")); err != nil || string(b) != raw {
		t.Fatalf("syllabus.raw.txt: %v %q", err, b)
	}
	// No syllabus row yet: launched lectures do not wait for a glossary_seed that will not come.
	e.call("GET", base, nil, http.StatusOK, &c)
	if c.Syllabus != nil || asrWait() == model.WaitGlossarySeed {
		t.Fatalf("raw upload created a syllabus: %+v, asr waits for %q", c.Syllabus, asrWait())
	}

	md := "# CSC1003 Cryptography\n\n## Key Terms\n\nblock cipher"
	e.llm.SetResponse(llmtest.RoleSyllabusFormat, "```markdown\n"+md+"\n```")
	var fr SyllabusFormatResult
	e.call("POST", base+"/syllabus/format", nil, http.StatusOK, &fr)
	if fr.Markdown != md || fr.Usage.PromptTokens == 0 || fr.Usage.EstimatedCost != nil {
		t.Fatalf("format %+v", fr)
	}
	reqs := e.llm.Requests(llmtest.RoleSyllabusFormat)
	if len(reqs) != 1 || reqs[0].User != "RAW SYLLABUS:\n"+raw || reqs[0].JSONMode {
		t.Fatalf("syllabus_format request %+v", reqs)
	}
	if fileExists(e.srv.syllabusMDPath(c.ID)) {
		t.Fatal("format saved SYLLABUS.md")
	}

	e.callErr("PUT", base+"/syllabus", SyllabusSaveInput{Markdown: " \n"}, http.StatusBadRequest)
	e.call("PUT", base+"/syllabus", SyllabusSaveInput{Markdown: md}, http.StatusOK, &c)
	if c.Syllabus == nil || c.Syllabus.Filename != "pasted.txt" || !c.Syllabus.Formatted || c.Syllabus.Chars != len([]rune(md)) ||
		len(c.Stages) != 1 || c.Stages[0].Status != model.StatusPending || !c.GlossaryPending {
		t.Fatalf("course after save %+v", c)
	}
	if b, _ := os.ReadFile(e.srv.syllabusMDPath(c.ID)); string(b) != md {
		t.Fatalf("SYLLABUS.md %q", b)
	}
	if sy, _ := e.st.GetSyllabus(ctx, c.ID); sy.Text == nil || *sy.Text != md {
		t.Fatalf("syllabus.text %v", sy.Text)
	}
	if w := asrWait(); w != model.WaitGlossarySeed {
		t.Fatalf("asr waits for %q, want glossary_seed", w)
	}
	e.call("GET", base+"/syllabus", nil, http.StatusOK, &doc)
	if doc.Raw != raw || doc.Markdown != md {
		t.Fatalf("doc %+v", doc)
	}

	// Saving again while glossary_seed runs is refused (and changes nothing).
	e.moveJob(model.ScopeCourse, c.ID, model.StageGlossarySeed, "queued", "running")
	e.callErr("PUT", base+"/syllabus", SyllabusSaveInput{Markdown: "# other"}, http.StatusConflict)
	if b, _ := os.ReadFile(e.srv.syllabusMDPath(c.ID)); string(b) != md {
		t.Fatalf("SYLLABUS.md changed by a refused save: %q", b)
	}
	res, _ := e.upload(base+"/syllabus", nil, "x.txt", []byte("x"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("legacy upload while running: %d", res.StatusCode)
	}
	e.moveJob(model.ScopeCourse, c.ID, model.StageGlossarySeed, "done")

	// A file as raw text: the row exists now, so its name follows; text stays.
	res, b := e.upload(base+"/syllabus/raw", nil, "outline.pdf", []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pdf raw: %d %s", res.StatusCode, b)
	}
	decodeStrict(t, b, &doc)
	if doc.Filename != "outline.pdf" || doc.Raw != strings.Join(pages, "\n\n") || doc.Markdown != md {
		t.Fatalf("pdf raw doc %+v", doc)
	}
	if sy, _ := e.st.GetSyllabus(ctx, c.ID); sy.Text == nil || *sy.Text != md || sy.Filename != "outline.pdf" {
		t.Fatalf("syllabus row after raw upload %+v", sy)
	}
	if left, _ := filepath.Glob(filepath.Join(model.SyllabusDir(e.cfg.DataDir, c.ID), "upload*")); len(left) != 0 {
		t.Fatalf("uploaded file kept: %v", left)
	}
	pages = []string{"", " "}
	res, _ = e.upload(base+"/syllabus/raw", nil, "scan.pdf", []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("scanned pdf: %d", res.StatusCode)
	}
	res, _ = e.upload(base+"/syllabus/raw", nil, "notes.docx", []byte("x"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("docx: %d", res.StatusCode)
	}

	// The v1 upload keeps working: the text becomes SYLLABUS.md unformatted.
	res, b = e.upload(base+"/syllabus", nil, "v1.md", []byte("# v1 syllabus\nCaches"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("legacy upload %d %s", res.StatusCode, b)
	}
	decodeStrict(t, b, &c)
	if !c.Syllabus.Formatted || c.Syllabus.Filename != "v1.md" || c.Stages[0].Status != model.StatusPending {
		t.Fatalf("legacy course %+v", c)
	}
	if got, _ := os.ReadFile(e.srv.syllabusMDPath(c.ID)); string(got) != "# v1 syllabus\nCaches" {
		t.Fatalf("legacy SYLLABUS.md %q", got)
	}
}

// Without the syllabus_format role the error of the settings is shown.
func TestFormatSyllabusErrors(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	e.call("POST", "/api/courses/"+c.ID+"/syllabus/raw", SyllabusRawInput{Text: "raw"}, http.StatusOK, nil)
	e.llm.SetError(http.StatusUnauthorized, "bad key")
	msg := e.callErr("POST", "/api/courses/"+c.ID+"/syllabus/format", nil, http.StatusBadGateway)
	if !strings.Contains(msg, "大纲整理失败") {
		t.Fatalf("llm error: %q", msg)
	}
	e.llm.SetError(0, "")
	ctx := context.Background()
	if err := e.st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleSyllabusFormat, Params: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := e.set.Load(ctx); err != nil {
		t.Fatal(err)
	}
	msg = e.callErr("POST", "/api/courses/"+c.ID+"/syllabus/format", nil, http.StatusConflict)
	if !strings.Contains(msg, "大纲整理") {
		t.Fatalf("unconfigured role: %q", msg)
	}
}

// A11: the course citation JSON lists the finished lectures by seq.
func TestCourseCiteExport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var c Course
	e.call("POST", "/api/courses", CourseCreate{Name: "Cryptography", Code: ptr("CSC1003")}, http.StatusCreated, &c)
	done := e.lecture(c.ID, "Week 1 - Classical")
	e.draft(c.ID, "Week 2 - draft")
	// v2 P13.6: the label is the calendar number.
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+done.ID, LecturePatch{RecordedAt: ptr("2026-09-30T10:00")}, http.StatusOK, nil)
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 4000, ASRText: "Hello.", RepairedText: "Hello.", ZhStatus: model.ZhNone},
		{Idx: 1, StartMS: 754000, EndMS: 758600, ASRText: "A block cipher.", RepairedText: "A block cipher.", ZhStatus: model.ZhNone},
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, done.ID, ss); err != nil {
			return err
		}
		return tx.UpsertSummary(ctx, model.Summary{LectureID: done.ID, Text: "Ciphers.", InputHash: "h"})
	}); err != nil {
		t.Fatal(err)
	}
	zh := "分组密码。"
	if err := e.st.SetTranslation(ctx, done.ID, 1, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK}); err != nil {
		t.Fatal(err)
	}
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "block cipher", "zh": "分组密码"}, http.StatusCreated, &term)
	e.allDone(done.ID)

	res, b := e.do("GET", "/api/courses/"+c.ID+"/export/cite.json", nil)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(res.Header.Get("Content-Disposition"), `filename="CSC1003.cite.json"`) {
		t.Fatalf("cite.json: %d %v", res.StatusCode, res.Header)
	}
	var doc cite.Doc
	decodeStrict(t, b, &doc)
	if doc.Schema != cite.Schema || doc.Course.Code == nil || *doc.Course.Code != "CSC1003" || len(doc.Lectures) != 1 ||
		len(doc.Glossary) != 1 || doc.GeneratedAt == "" {
		t.Fatalf("doc %+v", doc)
	}
	lc := doc.Lectures[0]
	if lc.ID != done.ID || lc.Label != "W01L1" || lc.Summary != "Ciphers." || len(lc.Sentences) != 2 || lc.DurationMS != 758600 {
		t.Fatalf("lecture %+v", lc)
	}
	if s := lc.Sentences[1]; s.ID != "CSC1003.W01L1.s0002" || s.Ref != "CSC1003 W01L1@12:34" || s.Zh == nil || *s.Zh != zh ||
		lc.Sentences[0].Zh != nil {
		t.Fatalf("sentences %+v", lc.Sentences)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if !strings.Contains(strings.Join(cv.CourseActions, ","), CourseActionExportCite) {
		t.Fatalf("course actions %v", cv.CourseActions)
	}
	// v2 P13.6: the export is recorded; renumbering now asks first.
	if !cv.CiteExported {
		t.Fatalf("cite_exported not recorded: %+v", cv)
	}
	// Without a code: the abbreviation of the name.
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Code: ptr("")}, http.StatusOK, &cv)
	res, _ = e.do("GET", "/api/courses/"+c.ID+"/export/cite.json", nil)
	if !strings.Contains(res.Header.Get("Content-Disposition"), `filename="C.cite.json"`) {
		t.Fatalf("disposition %q", res.Header.Get("Content-Disposition"))
	}
	e.callErr("GET", "/api/courses/nope/export/cite.json", nil, http.StatusNotFound)
}

// A9: the dashboard in one request.
func TestDashboard(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	if len(d.Courses) != 0 || d.InProgress == nil || d.Failures == nil || len(d.UsageToday) != 1 || d.GeneratedAt == "" {
		t.Fatalf("empty dashboard %+v", d)
	}

	var c Course
	e.call("POST", "/api/courses", CourseCreate{Name: "Cryptography", Code: ptr("CSC1003")}, http.StatusCreated, &c)
	draft := e.draft(c.ID, "Draft")
	prog := e.lecture(c.ID, "In progress")
	fin := e.lecture(c.ID, "Done")
	// v2 P13.6: lists show the calendar number.
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+fin.ID, LecturePatch{RecordedAt: ptr("2026-10-06T10:00")}, http.StatusOK, nil)
	failed := e.lecture(c.ID, "Failed")
	e.moveJob(model.ScopeLecture, prog.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, prog.ID, "asr", "queued")
	e.allDone(fin.ID)
	e.moveJob(model.ScopeLecture, failed.ID, "prep", "queued", "running", "failed")
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, fin.ID, []model.Sentence{
			{Idx: 0, EndMS: 1000, ASRText: "a", RepairedText: "a", ZhStatus: model.ZhNone},
			{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "b", RepairedText: "b", ZhStatus: model.ZhNone}}); err != nil {
			return err
		}
		return tx.InsertPatches(ctx, []model.Patch{
			{ID: db.NewID(), LectureID: fin.ID, SentenceIdx: 0, Before: "a", After: "A", Class: "decide", Review: "pending", CreatedAt: db.Now()},
			{ID: db.NewID(), LectureID: fin.ID, SentenceIdx: 1, Before: "b", After: "B", Class: "decide", Review: "pending", CreatedAt: db.Now()},
		})
	}); err != nil {
		t.Fatal(err)
	}
	// Today's usage, priced; plus a row of a provider deleted since. v2
	// P12.5: the cost shown is the accumulated per-request estimate
	// (estimated_cost), not tokens × today's prices.
	in, hit, out, cur := 2.0, 0.5, 8.0, "CNY"
	wantCost := (400_000*in + 600_000*hit + 100_000*out) / 1e6
	if _, err := e.set.UpdateProvider(ctx, "deepseek", settingsPrices(in, hit, out, cur)); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	for _, u := range []model.UsageDaily{
		{ProviderID: "deepseek", Day: today, Requests: 3, PromptTokens: 1_000_000, CacheHitTokens: 600_000, CompletionTokens: 100_000,
			EstimatedCost: wantCost},
		{ProviderID: "gone", Day: today, Requests: 1, PromptTokens: 10, CompletionTokens: 5},
		{ProviderID: "deepseek", Day: "2000-01-01", Requests: 9, PromptTokens: 9},
	} {
		if err := e.st.AddUsageDaily(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	ids := func(refs []LectureRef) []string {
		var out []string
		for _, r := range refs {
			out = append(out, r.ID)
		}
		return out
	}
	inprog := strings.Join(ids(d.InProgress), ",")
	if len(d.InProgress) != 2 || !strings.Contains(inprog, prog.ID) || !strings.Contains(inprog, failed.ID) ||
		d.InProgress[0].CourseName != "Cryptography" || d.InProgress[0].CourseCode == nil {
		t.Fatalf("in progress %v", ids(d.InProgress))
	}
	if d.DraftTotal != 1 || len(d.Drafts) != 1 || d.Drafts[0].Count != 1 || *d.Drafts[0].CourseCode != "CSC1003" {
		t.Fatalf("drafts %+v", d.Drafts)
	}
	if d.ReviewTotal != 2 || len(d.Reviews) != 1 || d.Reviews[0].LectureID != fin.ID || d.Reviews[0].Label != "W02L1" {
		t.Fatalf("reviews %+v", d.Reviews)
	}
	if len(d.Failures) != 1 || d.Failures[0].LectureID != failed.ID || d.Failures[0].Stage != "prep" ||
		!eqStrings(d.Failures[0].AllowedActions, []string{"retry"}) || d.Failures[0].Error == nil {
		t.Fatalf("failures %+v", d.Failures)
	}
	if len(d.RecentDone) != 1 || d.RecentDone[0].ID != fin.ID || d.RecentDone[0].DurationMS == nil || *d.RecentDone[0].DurationMS != 2000 {
		t.Fatalf("recent done %+v", d.RecentDone)
	}
	if d.StaleTotal != 0 || len(d.Stale) != 0 {
		t.Fatalf("stale %+v", d.Stale)
	}
	if len(d.UsageToday) != 2 {
		t.Fatalf("usage %+v", d.UsageToday)
	}
	u := d.UsageToday[0]
	if u.ProviderID != "deepseek" || u.Requests != 3 || u.PromptTokens != 1_000_000 || u.CacheHitRate == nil || *u.CacheHitRate != 0.6 ||
		u.Cost == nil || *u.Cost != wantCost || u.Currency == nil || *u.Currency != cur {
		t.Fatalf("usage row %+v", u)
	}
	if g := d.UsageToday[1]; g.ProviderID != "gone" || !strings.Contains(g.Name, "已删除") || g.Cost != nil || g.Completion != 5 {
		t.Fatalf("deleted provider row %+v", g)
	}
	if len(d.Courses) != 1 {
		t.Fatalf("courses %+v", d.Courses)
	}
	cc := d.Courses[0]
	if cc.LectureCount != 4 || cc.DraftCount != 1 || cc.RunningCount != 1 || cc.PatchesPending != 2 || cc.StaleCount != 0 ||
		cc.Code == nil || cc.LastActivityAt == "" ||
		!eqStrings(cc.CourseActions, []string{CourseActionLaunchDrafts, CourseActionExportCite}) {
		t.Fatalf("course card %+v", cc)
	}
	_ = draft
}

// A lecture whose only unfinished stages are stale is not "进行中": nothing
// runs or waits there; it is counted under 需要更新 (lead ruling, NOTES.md).
func TestDashboardStaleOnlyIsNotInProgress(t *testing.T) {
	e := newEnv(t)
	var c Course
	e.call("POST", "/api/courses", CourseCreate{Name: "Cryptography"}, http.StatusCreated, &c)
	stale := e.lecture(c.ID, "Stale only")
	e.allDone(stale.ID)
	e.moveJob(model.ScopeLecture, stale.ID, "repair", "stale")
	running := e.lecture(c.ID, "Running")
	e.moveJob(model.ScopeLecture, running.ID, "prep", "queued", "running")

	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	if len(d.InProgress) != 1 || d.InProgress[0].ID != running.ID {
		t.Fatalf("in progress = %+v, want only the running lecture", d.InProgress)
	}
	if d.StaleTotal != 1 || len(d.Stale) != 1 || d.Stale[0].CourseID != c.ID {
		t.Fatalf("stale = %d %+v", d.StaleTotal, d.Stale)
	}
}

func settingsPrices(in, hit, out float64, cur string) (p settings.ProviderInput) {
	p.PriceInput, p.PriceCacheHit, p.PriceOutput, p.Currency = &in, &hit, &out, &cur
	return p
}
