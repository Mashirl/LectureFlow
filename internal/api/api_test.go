// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/stages"
)

func TestCoursesAndErrorMapping(t *testing.T) {
	e := newEnv(t)
	e.callErr("POST", "/api/courses", map[string]string{"name": "  "}, http.StatusBadRequest)
	e.callErr("POST", "/api/courses", "not an object", http.StatusBadRequest)
	c := e.course("CSC1002")
	if c.Name != "CSC1002" || c.Syllabus != nil || len(c.Stages) != 0 || c.Materials == nil {
		t.Fatalf("unexpected course %+v", c)
	}
	var list []Course
	e.call("GET", "/api/courses", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list %+v", list)
	}
	e.callErr("GET", "/api/courses/nope", nil, http.StatusNotFound)
	e.callErr("GET", "/api/lectures/nope", nil, http.StatusNotFound)
	e.callErr("GET", "/api/courses/nope/lectures", nil, http.StatusNotFound)
	e.callErr("PUT", "/api/courses", nil, http.StatusNotFound) // unknown route → JSON 404
	e.callErr("GET", "/api/whatever", nil, http.StatusNotFound)
}

func TestUploadLectureStreamsToDisk(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	content := []byte(strings.Repeat("media-bytes-", 10000))
	res, b := e.upload("/api/courses/"+c.ID+"/lectures", [][2]string{{"title", " Week 3 - Caching "}}, "Week3.MP4", content)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d %s", res.StatusCode, b)
	}
	var l Lecture
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	if l.Title != "Week 3 - Caching" || l.CourseID != c.ID || len(l.Stages) != 6 || !l.CanDelete || !l.Reviewable {
		t.Fatalf("unexpected lecture %+v", l)
	}
	// v1.1 A3: an upload is a draft. v2 P13.4: its number is "W??" until the
	// course has a term start (and the lecture a recording time); v2 P13.3.1:
	// the uploaded file name is kept; a typed title is the user's (P13.5).
	if !l.Draft || l.LaunchedAt != nil || l.Seq != 1 || l.Label != "W??" || l.Numbered || l.CalendarHint != "term_start" ||
		l.OriginalFilename != "Week3.MP4" || l.TitleSource != model.TitleSourceUser || l.RecordedAtSource != model.RecordedAtNone ||
		!eqStrings(l.LectureActions, []string{model.LectureActionLaunch, model.LectureActionDelete}) {
		t.Fatalf("draft fields %+v", l)
	}
	for i, st := range l.Stages {
		if st.Stage != model.LectureStages[i] || st.Status != model.StatusPending || st.AllowedActions == nil ||
			st.DisplayStatus != model.StatusPending || st.Model != nil {
			t.Errorf("stage %d: %+v", i, st)
		}
	}
	// waiting_for is filled in for pending stages (SPEC §9.5): a draft's
	// stages are "not_started" (A3.1).
	if w := stageOf(l.Stages, "asr").WaitingFor; w == nil || *w != model.WaitNotStarted {
		t.Errorf("asr waiting_for = %v", w)
	}
	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, &l)
	if w := stageOf(l.Stages, "asr").WaitingFor; w == nil || *w != "upstream:prep" {
		t.Errorf("asr waiting_for after launch = %v", w)
	}
	row, err := e.st.GetLecture(context.Background(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(row.MediaPath) || filepath.Base(row.MediaPath) != "media.mp4" {
		t.Errorf("media path %q", row.MediaPath)
	}
	got, err := os.ReadFile(row.MediaPath)
	if err != nil || string(got) != string(content) {
		t.Fatalf("stored media differs (err %v, %d bytes)", err, len(got))
	}
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(row.MediaPath), "*.tmp")); len(tmp) > 0 {
		t.Errorf("leftover tmp files %v", tmp)
	}

	// No title: the file name without extension (SPEC §9.4).
	res, b = e.upload("/api/courses/"+c.ID+"/lectures", nil, "Lecture 4.m4a", []byte("x"))
	if res.StatusCode != http.StatusCreated || !strings.Contains(string(b), `"title":"Lecture 4"`) {
		t.Fatalf("default title: %d %s", res.StatusCode, b)
	}
	// No file part.
	res, b = e.upload("/api/courses/"+c.ID+"/lectures", [][2]string{{"title", "x"}}, "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing file: %d %s", res.StatusCode, b)
	}
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if len(list) != 2 {
		t.Fatalf("want 2 lectures, got %d", len(list))
	}
	// v2 P13.4: without recording times the list keeps the upload order.
	if list[0].Seq != 1 || list[1].Seq != 2 || list[1].Label != "W??" || list[1].Title != "Lecture 4" {
		t.Fatalf("seq order %+v / %+v", list[0], list[1])
	}
}

func TestUploadLimits(t *testing.T) {
	free := int64(100 << 30)
	e := newEnv(t, func(s *Server) {
		s.maxUpload = 1000
		s.freeSpace = func(string) (int64, error) { return free, nil }
	})
	c := e.course("C")
	res, b := e.upload("/api/courses/"+c.ID+"/lectures", [][2]string{{"title", "big"}}, "big.mp4", make([]byte, 5000))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d %s", res.StatusCode, b)
	}
	free = 1 << 30 // below the 2 GB floor (SPEC §17.5)
	res, b = e.upload("/api/courses/"+c.ID+"/lectures", [][2]string{{"title", "small"}}, "small.mp4", []byte("x"))
	if res.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("disk full: %d %s", res.StatusCode, b)
	}
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("rejected uploads created lectures: %+v", list)
	}
	entries, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, "courses", c.ID, "lectures"))
	if len(entries) != 0 {
		t.Fatalf("rejected uploads left directories: %v", entries)
	}
}

func TestSyllabusAndMaterials(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "Week 1")
	base := "/api/courses/" + c.ID

	res, b := e.upload(base+"/syllabus", nil, "syllabus.exe", []byte("x"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad syllabus type: %d %s", res.StatusCode, b)
	}
	res, b = e.upload(base+"/syllabus", nil, "outline.md", []byte("\xef\xbb\xbf# 课程大纲\nCaches"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("syllabus: %d %s", res.StatusCode, b)
	}
	var cv Course
	json.Unmarshal(b, &cv)
	if cv.Syllabus == nil || cv.Syllabus.Filename != "outline.md" || cv.Syllabus.Chars != len([]rune("# 课程大纲\nCaches")) {
		t.Fatalf("syllabus view %+v", cv.Syllabus)
	}
	if len(cv.Stages) != 1 || cv.Stages[0].Stage != "glossary_seed" || cv.Stages[0].Status != "pending" {
		t.Fatalf("glossary_seed %+v", cv.Stages)
	}
	// Re-upload replaces (another extension: the old file is removed).
	res, _ = e.upload(base+"/syllabus", nil, "outline.txt", []byte("v2"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replace syllabus: %d", res.StatusCode)
	}
	files, _ := filepath.Glob(filepath.Join(model.SyllabusDir(e.cfg.DataDir, c.ID), "*"))
	if len(files) != 1 || filepath.Base(files[0]) != "syllabus.txt" {
		t.Fatalf("syllabus dir %v", files)
	}

	res, b = e.upload(base+"/materials", nil, "notes.docx", []byte("x"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad material type: %d %s", res.StatusCode, b)
	}
	res, b = e.upload(base+"/materials", [][2]string{{"lecture_id", "nope"}}, "w1.pdf", []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign lecture: %d %s", res.StatusCode, b)
	}
	res, b = e.upload(base+"/materials", [][2]string{{"lecture_id", l.ID}}, "w1.pdf", []byte("%PDF-1.4"))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("material: %d %s", res.StatusCode, b)
	}
	cv = Course{}
	json.Unmarshal(b, &cv)
	if len(cv.Materials) != 1 {
		t.Fatalf("materials %+v", cv.Materials)
	}
	m := cv.Materials[0]
	if m.Filename != "w1.pdf" || m.LectureID == nil || *m.LectureID != l.ID || len(m.Stages) != 2 || !m.CanDelete {
		t.Fatalf("material %+v", m)
	}
	// A bound material blocks the lecture's asr (SPEC §4.3).
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if w := stageOf(lv.Stages, "asr").WaitingFor; w == nil || *w != "glossary_seed" {
		t.Errorf("asr waiting_for = %v, want glossary_seed first", w)
	}

	var mv Material
	e.call("PATCH", "/api/materials/"+m.ID, map[string]any{"lecture_id": nil}, http.StatusOK, &mv)
	if mv.LectureID != nil {
		t.Fatalf("rebinding to course-wide: %+v", mv)
	}
	e.callErr("PATCH", "/api/materials/"+m.ID, map[string]any{}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/materials/"+m.ID, map[string]any{"lecture_id": "nope"}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/materials/nope", map[string]any{"lecture_id": nil}, http.StatusNotFound)

	// Stage actions on material / course scopes go through DoAction.
	e.callErr("POST", "/api/materials/"+m.ID+"/stages/material_extract/retry", nil, http.StatusConflict) // pending
	e.callErr("POST", "/api/materials/"+m.ID+"/stages/prep/retry", nil, http.StatusBadRequest)
	e.moveJob(model.ScopeMaterial, m.ID, "material_extract", "queued")
	e.call("POST", "/api/materials/"+m.ID+"/stages/material_extract/cancel", nil, http.StatusOK, &mv)
	if st := stageOf(mv.Stages, "material_extract"); st.Status != "cancelled" || !eqStrings(st.AllowedActions, []string{"retry"}) {
		t.Fatalf("after cancel %+v", st)
	}
	e.callErr("POST", "/api/courses/"+c.ID+"/stages/glossary_seed/rerun", nil, http.StatusConflict)
	e.moveJob(model.ScopeCourse, c.ID, "glossary_seed", "queued")
	e.call("POST", "/api/courses/"+c.ID+"/stages/glossary_seed/cancel", nil, http.StatusOK, &cv)
	if cv.Stages[0].Status != "cancelled" {
		t.Fatalf("glossary_seed after cancel %+v", cv.Stages[0])
	}

	// Delete: 409 while running, then 204 and the directory is gone.
	e.call("POST", "/api/materials/"+m.ID+"/stages/material_extract/retry", nil, http.StatusOK, &mv)
	e.moveJob(model.ScopeMaterial, m.ID, "material_extract", "running")
	e.callErr("DELETE", "/api/materials/"+m.ID, nil, http.StatusConflict)
	e.moveJob(model.ScopeMaterial, m.ID, "material_extract", "done")
	e.call("DELETE", "/api/materials/"+m.ID, nil, http.StatusNoContent, nil)
	if _, err := os.Stat(model.MaterialDir(e.cfg.DataDir, c.ID, m.ID)); !os.IsNotExist(err) {
		t.Fatalf("material dir not removed: %v", err)
	}
}

func TestLectureStageActionsRoundTrip(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	p := "/api/lectures/" + l.ID + "/stages/"

	e.callErr("POST", p+"bogus/retry", nil, http.StatusBadRequest)
	e.callErr("POST", p+"prep/explode", nil, http.StatusBadRequest)
	e.callErr("POST", p+"prep/retry", nil, http.StatusConflict) // pending: SPEC §4.5 allows nothing
	e.callErr("POST", "/api/lectures/nope/stages/prep/retry", nil, http.StatusNotFound)

	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "failed")
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	prep := stageOf(lv.Stages, "prep")
	if prep.Status != "failed" || !eqStrings(prep.AllowedActions, []string{"retry"}) || prep.Error == nil || prep.DurationMS == nil {
		t.Fatalf("failed prep %+v", prep)
	}
	e.call("POST", p+"prep/retry", nil, http.StatusOK, &lv)
	prep = stageOf(lv.Stages, "prep")
	if prep.Status != "queued" || prep.Attempt != 1 || !eqStrings(prep.AllowedActions, []string{"cancel"}) {
		t.Fatalf("after retry %+v", prep)
	}
	e.call("POST", p+"prep/cancel", nil, http.StatusOK, &lv)
	if prep = stageOf(lv.Stages, "prep"); prep.Status != "cancelled" {
		t.Fatalf("after cancel %+v", prep)
	}
	e.call("POST", p+"prep/retry", nil, http.StatusOK, &lv)
	e.moveJob(model.ScopeLecture, l.ID, "prep", "running")
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if lv.CanDelete {
		t.Fatal("can_delete while prep runs")
	}
	e.callErr("DELETE", "/api/lectures/"+l.ID, nil, http.StatusConflict)
	e.moveJob(model.ScopeLecture, l.ID, "prep", "done")
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if prep = stageOf(lv.Stages, "prep"); prep.Status != "done" || prep.Progress != 1 || !eqStrings(prep.AllowedActions, []string{"rerun"}) {
		t.Fatalf("done prep %+v", prep)
	}
	old := e.moveJob(model.ScopeLecture, l.ID, "prep")
	e.call("POST", p+"prep/rerun", nil, http.StatusOK, &lv)
	cur := e.moveJob(model.ScopeLecture, l.ID, "prep")
	if cur.ID == old.ID || stageOf(lv.Stages, "prep").Status != "pending" {
		t.Fatalf("rerun did not create a new pending job: %+v", stageOf(lv.Stages, "prep"))
	}

	// Rename (NOTES.md A) and delete.
	e.callErr("PATCH", "/api/lectures/"+l.ID, map[string]string{"title": " "}, http.StatusBadRequest)
	e.call("PATCH", "/api/lectures/"+l.ID, map[string]string{"title": "Week 9"}, http.StatusOK, &lv)
	if lv.Title != "Week 9" {
		t.Fatalf("title %q", lv.Title)
	}
	dir := model.LectureDir(e.cfg.DataDir, c.ID, l.ID)
	e.call("DELETE", "/api/lectures/"+l.ID, nil, http.StatusNoContent, nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lecture dir not removed: %v", err)
	}
	e.callErr("GET", "/api/lectures/"+l.ID, nil, http.StatusNotFound)
}

// A done stage keeps its warning text in `error` (NOTES.md Gate B 4: repair
// chunks that failed); it is still done, offers only rerun, and the lecture
// is complete.
func TestDoneStageWithWarningKeepsError(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	for _, st := range model.LectureStages {
		e.moveJob(model.ScopeLecture, l.ID, st, "queued", "running", "done")
	}
	warning := "共 17 块，其中第 9 块（句 320–359）的回答连续 3 次无法解析，这些句子未做修复"
	j := e.moveJob(model.ScopeLecture, l.ID, model.StageRepair)
	if err := e.st.SetJobError(context.Background(), j.ID, &warning); err != nil {
		t.Fatal(err)
	}
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	rep := stageOf(lv.Stages, model.StageRepair)
	if rep.Status != model.StatusDone || rep.Error == nil || *rep.Error != warning {
		t.Fatalf("repair stage %+v (error %v)", rep, rep.Error)
	}
	if !eqStrings(rep.AllowedActions, []string{model.ActionRerun}) {
		t.Errorf("allowed_actions %v, want [rerun]", rep.AllowedActions)
	}
	for _, st := range lv.Stages {
		if st.Stage != model.StageRepair && st.Error != nil {
			t.Errorf("%s has error %q", st.Stage, *st.Error)
		}
	}
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if got := stageOf(list[0].Stages, model.StageRepair).Error; got == nil || *got != warning {
		t.Errorf("list view lost the warning: %v", got)
	}
}

// termHasher is a fake repair stage whose input hash covers the enabled
// glossary, like the real one (SPEC §4.4).
type termHasher struct{ st db.Store }

func (h termHasher) Name() string                 { return model.StageRepair }
func (h termHasher) Resource() sched.ResourceKind { return sched.ResourceAPI }
func (h termHasher) Run(context.Context, string, func(float64)) error {
	return nil
}
func (h termHasher) InputHash(ctx context.Context, lectureID string) (string, error) {
	l, err := h.st.GetLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	ts, err := h.st.ListTerms(ctx, l.CourseID)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	for _, t := range ts {
		if t.Enabled {
			sum.Write([]byte(t.En + "=>" + t.Zh + "\n"))
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func TestTermsChangesInvalidate(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	h := termHasher{e.st}
	e.sc.Register(h)
	ctx := context.Background()
	markDone := func() {
		for _, st := range []string{"prep", "asr", "summary", "repair"} {
			j := e.moveJob(model.ScopeLecture, l.ID, st)
			if j.Status == model.StatusPending {
				e.moveJob(model.ScopeLecture, l.ID, st, "queued", "running", "done")
			}
		}
		j := e.moveJob(model.ScopeLecture, l.ID, "repair")
		hash, _ := h.InputHash(ctx, l.ID)
		if err := e.st.SetJobInputHash(ctx, j.ID, hash); err != nil {
			t.Fatal(err)
		}
	}
	markDone()
	repairStatus := func() string {
		e.srv.WaitInvalidations()
		var lv Lecture
		e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		return stageOf(lv.Stages, "repair").Status
	}

	e.callErr("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "  "}, http.StatusBadRequest)
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": " write  back ", "zh": "写回"}, http.StatusCreated, &term)
	if term.En != "write back" || term.Source != "manual" || !term.Enabled {
		t.Fatalf("term %+v", term)
	}
	if got := repairStatus(); got != model.StatusStale {
		t.Fatalf("after adding a term repair is %s, want stale", got)
	}
	e.callErr("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "Write Back"}, http.StatusConflict)

	// Each change bumps glossary_version.
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.GlossaryVersion != 1 || cv.TermCount != 1 {
		t.Fatalf("course after add %+v", cv)
	}

	// Rerun repair so it is done again, then patch the term.
	var lv Lecture
	e.call("POST", "/api/lectures/"+l.ID+"/stages/repair/rerun", nil, http.StatusOK, &lv)
	markDone()
	e.callErr("PATCH", "/api/terms/"+term.ID, map[string]any{}, http.StatusBadRequest)
	e.call("PATCH", "/api/terms/"+term.ID, map[string]any{"enabled": false}, http.StatusOK, &term)
	if term.Enabled {
		t.Fatal("term still enabled")
	}
	if got := repairStatus(); got != model.StatusStale {
		t.Fatalf("after disabling a term repair is %s, want stale", got)
	}
	e.call("POST", "/api/lectures/"+l.ID+"/stages/repair/rerun", nil, http.StatusOK, &lv)
	markDone()
	e.call("DELETE", "/api/terms/"+term.ID, nil, http.StatusNoContent, nil)
	// The deleted term was disabled: the enabled glossary did not change.
	if got := repairStatus(); got != model.StatusDone {
		t.Fatalf("after deleting a disabled term repair is %s, want done", got)
	}
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.GlossaryVersion != 3 || cv.TermCount != 0 {
		t.Fatalf("course after delete %+v", cv)
	}
	e.callErr("DELETE", "/api/terms/"+term.ID, nil, http.StatusNotFound)
	var terms []Term
	e.call("GET", "/api/courses/"+c.ID+"/terms", nil, http.StatusOK, &terms)
	if len(terms) != 0 {
		t.Fatalf("terms %+v", terms)
	}
}

func TestSentencesPatchesAndReviewValidation(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	ctx := context.Background()
	human := "It has 64-byte lines."
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 4000, ASRText: "Okay.", RepairedText: "Okay.", ZhStatus: "none"},
		{Idx: 1, StartMS: 4000, EndMS: 9000, ASRText: "A right back cache.", RepairedText: "A write-back cache.",
			LowConfWords: []model.LowConfWord{{I: 1, W: "right"}, {I: 2, W: "back"}}, ZhStatus: "none"},
		{Idx: 2, StartMS: 9000, EndMS: 12000, ASRText: "It has 64 byte lines.", RepairedText: "It has 64 byte lines.", HumanText: &human, ZhStatus: "none"},
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, l.ID, ss); err != nil {
			return err
		}
		if err := tx.RecomputeText(ctx, l.ID, nil); err != nil {
			return err
		}
		return tx.InsertPatches(ctx, []model.Patch{{ID: db.NewID(), LectureID: l.ID, SentenceIdx: 1, Before: "A right back cache.",
			After: "A write-back cache.", Class: "decide", Reason: "may change meaning", Review: "pending", CreatedAt: db.Now()}})
	}); err != nil {
		t.Fatal(err)
	}
	var page SentencePage
	e.call("GET", "/api/lectures/"+l.ID+"/sentences?offset=1&limit=5", nil, http.StatusOK, &page)
	if page.Total != 3 || page.Offset != 1 || len(page.Sentences) != 2 {
		t.Fatalf("page %+v", page)
	}
	s1 := page.Sentences[0]
	if s1.Idx != 1 || s1.Text != "A write-back cache." || s1.Patch == nil || s1.Patch.Review != "pending" || len(s1.LowConfWords) != 2 {
		t.Fatalf("sentence 1 %+v", s1)
	}
	if s2 := page.Sentences[1]; s2.Text != human || s2.Patch != nil || s2.ZhStatus != "none" {
		t.Fatalf("sentence 2 %+v", s2)
	}
	e.callErr("GET", "/api/lectures/"+l.ID+"/sentences?limit=0", nil, http.StatusBadRequest)
	e.callErr("GET", "/api/lectures/"+l.ID+"/sentences?offset=-1", nil, http.StatusBadRequest)

	var ps []Patch
	e.call("GET", "/api/lectures/"+l.ID+"/patches?review=pending", nil, http.StatusOK, &ps)
	if len(ps) != 1 {
		t.Fatalf("patches %+v", ps)
	}
	p := ps[0]
	if p.LowConfCount != 2 || p.StartMS != 4000 || p.EndMS != 9000 || p.PrevText == nil || *p.PrevText != "Okay." || p.NextText == nil || *p.NextText != human {
		t.Fatalf("patch view %+v", p)
	}
	e.call("GET", "/api/lectures/"+l.ID+"/patches?review=accepted", nil, http.StatusOK, &ps)
	if len(ps) != 0 {
		t.Fatalf("accepted patches %+v", ps)
	}
	e.callErr("GET", "/api/lectures/"+l.ID+"/patches?review=bogus", nil, http.StatusBadRequest)

	// Validation happens before state.ReviewPatch / EditSentence are called.
	e.callErr("POST", "/api/patches/"+p.ID+"/review", map[string]string{"action": "maybe"}, http.StatusBadRequest)
	e.callErr("POST", "/api/patches/"+p.ID+"/review", map[string]string{"action": "edit", "text": "  "}, http.StatusBadRequest)
	e.callErr("POST", "/api/patches/"+p.ID+"/review", "x", http.StatusBadRequest)
	e.callErr("POST", "/api/lectures/"+l.ID+"/sentences/x/edit", map[string]string{"text": "a"}, http.StatusBadRequest)
	e.callErr("POST", "/api/lectures/"+l.ID+"/sentences/1/edit", map[string]string{"text": ""}, http.StatusBadRequest)
	e.callErr("POST", "/api/lectures/"+l.ID+"/sentences/1/explode", nil, http.StatusBadRequest)
	e.callErr("POST", "/api/lectures/nope/sentences/1/reset", nil, http.StatusNotFound)
}

func TestExportDownloadAndTraversal(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	dir := model.LectureDir(e.cfg.DataDir, c.ID, l.ID)
	exp := filepath.Join(dir, model.ExportDirName)
	if err := os.MkdirAll(exp, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(exp, "Week 1.bilingual.md"), []byte("**[00:00]** Hi\n\n你好\n"), 0o644)
	os.WriteFile(filepath.Join(exp, "Week 1.en.txt.tmp"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644)

	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	// SPEC-v2 P13.6: listed under its download name, "{number} {title}" (here
	// the unnumbered lecture's citation label, "?" made "_"); the URL is the
	// file on disk.
	if len(lv.Exports) != 1 || lv.Exports[0].Name != stages.ExportDownloadName(lv.CiteLabel, "L", "Week 1.bilingual.md") ||
		!strings.HasPrefix(lv.Exports[0].Name, "W__-") || !strings.HasSuffix(lv.Exports[0].URL, "/export/Week%201.bilingual.md") {
		t.Fatalf("exports %+v", lv.Exports)
	}
	res, b := e.do("GET", lv.Exports[0].URL, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "你好") ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/markdown") || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %q %v", res.StatusCode, b, res.Header)
	}
	for _, bad := range []string{"..%2Fsecret.txt", "..%5Csecret.txt", "%2E%2E", "Week%201.en.txt.tmp", "C%3A%5CWindows%5Cwin.ini"} {
		res, b := e.do("GET", "/api/lectures/"+l.ID+"/export/"+bad, nil)
		if res.StatusCode == http.StatusOK || strings.Contains(string(b), "secret") {
			t.Errorf("%s: status %d body %q", bad, res.StatusCode, b)
		}
	}
	e.callErr("GET", "/api/lectures/"+l.ID+"/export/missing.md", nil, http.StatusNotFound)
}

func TestStaticFrontEnd(t *testing.T) {
	e := newEnv(t)
	for path, ct := range map[string]string{"/": "text/html", "/app.js": "text/javascript", "/style.css": "text/css"} {
		res, b := e.do("GET", path, nil)
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), ct) || len(b) == 0 {
			t.Errorf("%s: %d %q", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header set", path)
		}
	}
	res, _ := e.do("GET", "/nope.js", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset: %d", res.StatusCode)
	}
}
