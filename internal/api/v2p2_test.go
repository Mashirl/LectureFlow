// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
	"lectureflow/internal/subs"
)

// v2 phase 2 (SPEC-v2 P6–P8, P12, P13, P14.3, P14.7) HTTP tests: media with
// Range, cues, chapters, resume, titles, auxiliary-task actions, calendar
// numbering and the renumbering warning, file-name rules, holidays, UI
// settings, provider tariffs, structured stale reasons and coded errors.

func (e *env) dirOf(l Lecture) string { return model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID) }

// configureRole points an LLM role at the fake provider (the auxiliary
// tasks use the role's current setting).
func (e *env) configureRole(role string) {
	e.t.Helper()
	e.call("PUT", "/api/settings/roles/"+role, RoleInput{ProviderID: "deepseek", Model: "deepseek-flash"}, http.StatusOK, nil)
}

// get sends a GET with extra headers and returns the response and body.
func (e *env) get(path string, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// failCoded fails the current run of an aux job with a coded error, the way
// the scheduler stores it (state.EncodeJobError).
func (e *env) failCoded(scope, id, stage, code, detail string) {
	e.t.Helper()
	ctx := context.Background()
	j := e.moveJob(scope, id, stage)
	if j.Status != model.StatusRunning {
		j = e.moveJob(scope, id, stage, "queued", "running")
	}
	text := state.EncodeJobError(model.Errorf(code, nil, errors.New(detail)))
	if _, err := state.FailJob(ctx, e.st, j.ID, text, false); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) lectureView(id string) Lecture {
	e.t.Helper()
	var l Lecture
	e.call("GET", "/api/lectures/"+id, nil, http.StatusOK, &l)
	return l
}

func auxOf(l Lecture, stage string) (StageView, bool) {
	for _, a := range l.Aux {
		if a.Stage == stage {
			return a, true
		}
	}
	return StageView{}, false
}

// ---- P8.2 media (ruling 7) ----

// Range requests answer 206 with the exact byte range (SPEC-v2 step 7);
// ?audio=1 serves audio.wav the same way.
func TestMediaRangeAndAudio(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	base := "/api/lectures/" + l.ID + "/media"
	if info := e.callErrInfo("GET", base, nil, http.StatusNotFound); info.Code != "media.not_ready" {
		t.Fatalf("before prep: %+v", info)
	}
	if info := e.callErrInfo("GET", base+"?audio=1", nil, http.StatusNotFound); info.Code != "audio.not_ready" {
		t.Fatalf("no audio: %+v", info)
	}
	if info := e.callErrInfo("GET", base+"?audio=maybe", nil, http.StatusBadRequest); info.Code != "request.bad_param" {
		t.Fatalf("bad audio param: %+v", info)
	}

	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(e.dirOf(l), model.StreamFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, b := e.get(base, map[string]string{"Range": "bytes=1000-1999"})
	if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Range") != "bytes 1000-1999/5000" ||
		res.Header.Get("Content-Type") != "video/mp4" || !bytes.Equal(b, data[1000:2000]) {
		t.Fatalf("range: %d %v (%d bytes)", res.StatusCode, res.Header, len(b))
	}
	res, b = e.get(base, map[string]string{"Range": "bytes=-100"})
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(b, data[4900:]) {
		t.Fatalf("suffix range: %d (%d bytes)", res.StatusCode, len(b))
	}
	res, b = e.get(base, nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(b, data) || res.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("whole file: %d %v", res.StatusCode, res.Header)
	}
	if res, _ := e.get(base, map[string]string{"Range": "bytes=9000-9100"}); res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range: %d", res.StatusCode)
	}
	if v := e.lectureView(l.ID); v.Stream == nil || v.Stream.Status != "ready" || v.Stream.SizeBytes != 5000 {
		t.Fatalf("stream info %+v", v.Stream)
	}

	wav := filepath.Join(e.dirOf(l), model.AudioFile)
	writeWAV(t, wav, 500)
	full, _ := os.ReadFile(wav)
	res, b = e.get(base+"?audio=1", map[string]string{"Range": "bytes=0-43"})
	if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Type") != "audio/wav" || !bytes.Equal(b, full[:44]) {
		t.Fatalf("audio range: %d %v", res.StatusCode, res.Header)
	}
}

// A lecture prepared before v2 has no stream.mp4: GET /media queues the
// stream task (404 media.generating); its failure is reported with its code
// and retried by the user; a final result (audio only) is not queued again.
func TestMediaGeneratesStreamAndAuxActions(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, "queued", "running", "done")
	base := "/api/lectures/" + l.ID + "/media"
	if v := e.lectureView(l.ID); v.Stream == nil || v.Stream.Status != "pending" || len(v.Aux) != 0 {
		t.Fatalf("before /media: %+v %+v", v.Stream, v.Aux)
	}
	if info := e.callErrInfo("GET", base, nil, http.StatusNotFound); info.Code != "media.generating" {
		t.Fatalf("first open: %+v", info)
	}
	v := e.lectureView(l.ID)
	a, ok := auxOf(v, model.StageStream)
	if !ok || a.Status != model.StatusPending || v.Stream == nil || v.Stream.Status != "generating" ||
		!eqStrings(stageOf(v.Stages, model.StagePrep).AllowedActions, []string{"rerun"}) || len(v.Stages) != 6 {
		t.Fatalf("after /media: stream %+v aux %+v", v.Stream, v.Aux)
	}
	// The aux task never shows as a lecture-level "cancel" (only the pipeline counts).
	for _, x := range v.LectureActions {
		if x == model.LectureActionCancel {
			t.Fatalf("lecture actions %v", v.LectureActions)
		}
	}
	// Stage actions on aux tasks: retry and cancel only.
	e.callErrInfo("POST", "/api/lectures/"+l.ID+"/stages/stream/rerun", nil, http.StatusBadRequest)
	e.callErrInfo("POST", "/api/lectures/"+l.ID+"/stages/title/rerun", nil, http.StatusBadRequest)

	e.failCoded(model.ScopeLecture, l.ID, model.StageStream, "media.corrupt", "moov atom not found")
	if info := e.callErrInfo("GET", base, nil, http.StatusNotFound); info.Code != "media.corrupt" || info.Detail != "moov atom not found" {
		t.Fatalf("after a failure: %+v", info)
	}
	v = e.lectureView(l.ID)
	a, _ = auxOf(v, model.StageStream)
	if a.Status != model.StatusFailed || a.ErrorInfo == nil || a.ErrorInfo.Code != "media.corrupt" || a.Error == nil ||
		*a.Error != "moov atom not found" || !eqStrings(a.AllowedActions, []string{"retry"}) {
		t.Fatalf("failed aux %+v", a)
	}
	var lv Lecture
	e.call("POST", "/api/lectures/"+l.ID+"/stages/stream/retry", nil, http.StatusOK, &lv)
	if a, _ := auxOf(lv, model.StageStream); a.Status != model.StatusQueued && a.Status != model.StatusPending {
		t.Fatalf("retried aux %+v", a)
	}
	// A run that ended with "audio only" is final: not queued again by /media,
	// but "retry" starts a new run.
	e.moveJob(model.ScopeLecture, l.ID, model.StageStream, "running", "done")
	if err := e.st.SetKV(context.Background(), model.KVStreamPrefix+l.ID, `{"status":"audio_only"}`); err != nil {
		t.Fatal(err)
	}
	if info := e.callErrInfo("GET", base, nil, http.StatusNotFound); info.Code != "media.audio_only" {
		t.Fatalf("audio only: %+v", info)
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, model.StageStream); j.Status != model.StatusDone {
		t.Fatalf("audio only was queued again: %s", j.Status)
	}
	if v := e.lectureView(l.ID); v.Stream == nil || v.Stream.Status != "audio_only" {
		t.Fatalf("stream %+v", v.Stream)
	}
	e.call("POST", "/api/lectures/"+l.ID+"/stages/stream/retry", nil, http.StatusOK, &lv)
	if j := e.moveJob(model.ScopeLecture, l.ID, model.StageStream); j.Status != model.StatusPending {
		t.Fatalf("retry after audio only: %s", j.Status)
	}
	if a, _ := auxOf(e.lectureView(l.ID), model.StageStream); a.Status != model.StatusPending {
		t.Fatalf("aux %+v", a)
	}
	// Cancel an aux task that waits.
	e.moveJob(model.ScopeLecture, l.ID, model.StageStream, "queued")
	e.call("POST", "/api/lectures/"+l.ID+"/stages/stream/cancel", nil, http.StatusOK, &lv)
	if a, _ := auxOf(lv, model.StageStream); a.Status != model.StatusCancelled || !eqStrings(a.AllowedActions, []string{"retry"}) {
		t.Fatalf("cancelled aux %+v", a)
	}
}

// ---- P4 / P8.2 cues (ruling 8) ----

// GET /cues is subs.SegmentLecture of the lecture's sentences, converted
// exactly like the VTT export: English = sentence.text, Chinese only when
// zh_status is not none.
func TestCuesMatchSegmentLecture(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	var empty []Cue
	e.call("GET", "/api/lectures/"+l.ID+"/cues", nil, http.StatusOK, &empty)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("no sentences: %v", empty)
	}
	zh0, zhStale := "分组密码把明文分成固定长度的块，再逐块加密。", "旧译文"
	human := "So a block cipher works on fixed-size blocks."
	ss := []model.Sentence{
		{Idx: 0, StartMS: 1000, EndMS: 9000, ASRText: "A block cipher splits the plaintext into fixed length blocks, and then encrypts each block.",
			RepairedText: "A block cipher splits the plaintext into fixed-length blocks, and then encrypts each block.",
			Words:        []model.Word{{W: "A", S: 1000, E: 1100}, {W: "block", S: 1100, E: 1500}, {W: "cipher", S: 1500, E: 2000}},
			Zh:           &zh0, ZhStatus: model.ZhOK},
		{Idx: 1, StartMS: 9500, EndMS: 12000, ASRText: "So a block cypher works on fixed size blocks.", RepairedText: "So a block cypher works on fixed size blocks.",
			HumanText: &human, Zh: &zhStale, ZhStatus: model.ZhNone},
		{Idx: 2, StartMS: 20000, EndMS: 21000, ASRText: "Okay.", RepairedText: "Okay.", ZhStatus: model.ZhNone},
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, l.ID, ss); err != nil {
			return err
		}
		return tx.RecomputeText(ctx, l.ID, nil)
	}); err != nil {
		t.Fatal(err)
	}
	var got []Cue
	e.call("GET", "/api/lectures/"+l.ID+"/cues", nil, http.StatusOK, &got)
	stored, err := e.st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := subs.SegmentLecture(stages.CueSentences(stored))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cues differ from SegmentLecture:\n got  %+v\n want %+v", got, want)
	}
	var en1 []string
	zh0Shown := false
	for _, cu := range got {
		switch cu.SentenceIdx {
		case 0:
			zh0Shown = zh0Shown || cu.Zh != ""
		case 1:
			en1 = append(en1, cu.En)
			if cu.Zh != "" {
				t.Errorf("zh_status none must not be shown: %+v", cu)
			}
		}
	}
	if !zh0Shown {
		t.Errorf("the translated sentence shows no Chinese: %+v", got)
	}
	if strings.Join(en1, " ") != human {
		t.Fatalf("English must be sentence.text (human edit): %q", strings.Join(en1, " "))
	}
	e.callErrInfo("GET", "/api/lectures/nope/cues", nil, http.StatusNotFound)
}

// ---- P6.4 resume ----

func TestResumeRoundTrip(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	path := "/api/lectures/" + l.ID + "/resume"
	var rv ResumeView
	e.call("GET", path, nil, http.StatusOK, &rv)
	if rv.PositionMS != 0 || rv.SavedAt != "" {
		t.Fatalf("nothing saved: %+v", rv)
	}
	e.call("PUT", path, map[string]any{"position_ms": 1613000}, http.StatusOK, &rv)
	if rv.PositionMS != 1613000 || rv.SavedAt == "" {
		t.Fatalf("saved %+v", rv)
	}
	if _, err := time.Parse(time.RFC3339, rv.SavedAt); err != nil {
		t.Fatalf("saved_at %q", rv.SavedAt)
	}
	var again ResumeView
	e.call("GET", path, nil, http.StatusOK, &again)
	if again != rv {
		t.Fatalf("round trip %+v != %+v", again, rv)
	}
	for _, body := range []any{map[string]any{"position_ms": -1}, map[string]any{}, "x"} {
		info := e.callErrInfo("PUT", path, body, http.StatusBadRequest)
		if info.Code != "resume.bad_position" && info.Code != "request.bad_json" {
			t.Fatalf("bad body %v: %+v", body, info)
		}
	}
	e.callErrInfo("GET", "/api/lectures/nope/resume", nil, http.StatusNotFound)
	e.callErrInfo("PUT", "/api/lectures/nope/resume", map[string]any{"position_ms": 1}, http.StatusNotFound)
}

// ---- P7 chapters (ruling 9) ----

func TestChaptersEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	path := "/api/lectures/" + l.ID + "/chapters"
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, l.ID, []model.Sentence{
			{Idx: 0, StartMS: 0, EndMS: 4000, ASRText: "Hello.", RepairedText: "Hello.", ZhStatus: model.ZhNone},
			{Idx: 1, StartMS: 4000, EndMS: 9000, ASRText: "Caesar.", RepairedText: "Caesar.", ZhStatus: model.ZhNone},
			{Idx: 2, StartMS: 61000, EndMS: 65000, ASRText: "Vigenere.", RepairedText: "Vigenere.", ZhStatus: model.ZhNone},
		}); err != nil {
			return err
		}
		return tx.RecomputeText(ctx, l.ID, nil)
	}); err != nil {
		t.Fatal(err)
	}
	// Never generated (power-brief ruling 21): 200 with the empty view, the
	// same shape as a result (no longer 404 chapters.not_found).
	var empty ChaptersView
	e.call("GET", path, nil, http.StatusOK, &empty)
	if empty.Chapters == nil || len(empty.Chapters) != 0 || empty.Stale || empty.Running || empty.Error != "" ||
		empty.GeneratedAt != "" || empty.Model != "" {
		t.Fatalf("none yet: %+v", empty)
	}
	if res, raw := e.do("GET", path, nil); res.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"chapters":[],"stale":false,"running":false}` {
		got := string(raw)
		t.Fatalf("none yet body %s", got)
	}
	if info := e.callErrInfo("GET", "/api/lectures/nope/chapters", nil, http.StatusNotFound); info.Code != "lecture.not_found" {
		t.Fatalf("unknown lecture: %+v", info)
	}
	if info := e.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != "chapters.not_configured" {
		t.Fatalf("role not configured: %+v", info)
	}
	e.configureRole(model.RoleChapters)
	if info := e.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != "chapters.summary_not_ready" {
		t.Fatalf("no summary: %+v", info)
	}
	e.allDone(l.ID)
	if err := e.st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "Classical ciphers.", InputHash: "h"}); err != nil {
		t.Fatal(err)
	}
	var cv ChaptersView
	e.call("POST", path, nil, http.StatusAccepted, &cv)
	if !cv.Running || cv.Chapters == nil || len(cv.Chapters) != 0 {
		t.Fatalf("queued %+v", cv)
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, model.StageChapters); j.Status != model.StatusPending {
		t.Fatalf("chapters job %s", j.Status)
	}
	e.call("GET", path, nil, http.StatusOK, &cv)
	if !cv.Running {
		t.Fatalf("running %+v", cv)
	}

	// The task's result (kv chapters:{id}, P7) with the input hash.
	hash, err := stages.ChaptersInputHash(ctx, e.srv.stageDeps(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored := model.Chapters{InputHash: hash, GeneratedAt: "2026-10-23T20:00:00Z", Provider: "deepseek", Model: "deepseek-flash",
		Chapters: []model.ChapterEntry{{StartIdx: 0, TitleEn: "Introduction", TitleZh: "引言"}, {StartIdx: 2, TitleEn: "Vigenere", TitleZh: "维吉尼亚密码"}}}
	b, _ := json.Marshal(stored)
	if err := e.st.SetKV(ctx, model.KVChaptersPrefix+l.ID, string(b)); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StageChapters, "queued", "running", "done")
	e.call("GET", path, nil, http.StatusOK, &cv)
	if cv.Running || cv.Stale || cv.Error != "" || len(cv.Chapters) != 2 || cv.Chapters[1].StartMS != 61000 ||
		cv.Chapters[1].TitleZh != "维吉尼亚密码" || cv.GeneratedAt != stored.GeneratedAt || cv.Model != "deepseek / deepseek-flash" {
		t.Fatalf("chapters %+v", cv)
	}
	// The summary changes: "章节可能已过时" (never regenerated by itself).
	if err := e.st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "Classical ciphers, revised.", InputHash: "h2"}); err != nil {
		t.Fatal(err)
	}
	e.call("GET", path, nil, http.StatusOK, &cv)
	if !cv.Stale || len(cv.Chapters) != 2 {
		t.Fatalf("stale %+v", cv)
	}
	// Generating again fails validation twice: the old chapters stay, with the error code.
	e.call("POST", path, nil, http.StatusAccepted, &cv)
	e.failCoded(model.ScopeLecture, l.ID, model.StageChapters, "chapters.invalid_output", "start_idx not increasing")
	e.call("GET", path, nil, http.StatusOK, &cv)
	if cv.Error != "chapters.invalid_output" || cv.Running || len(cv.Chapters) != 2 {
		t.Fatalf("after a failure %+v", cv)
	}
	if a, ok := auxOf(e.lectureView(l.ID), model.StageChapters); !ok || !eqStrings(a.AllowedActions, []string{"retry"}) {
		t.Fatalf("aux %+v", a)
	}
}

// ---- P13.5 title regeneration ----

func TestTitleRegenerate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "Week 3")
	path := "/api/lectures/" + l.ID + "/title"
	if info := e.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != "title.not_configured" {
		t.Fatalf("role not configured: %+v", info)
	}
	e.configureRole(model.RoleOrganizer)
	if info := e.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != "title.summary_not_ready" {
		t.Fatalf("no summary: %+v", info)
	}
	e.allDone(l.ID)
	// The upload form's title is the user's.
	if v := e.lectureView(l.ID); v.TitleSource != model.TitleSourceUser {
		t.Fatalf("title source %q", v.TitleSource)
	}
	if info := e.callErrInfo("POST", path, TitleRegenerateInput{}, http.StatusConflict); info.Code != "title.user_set" {
		t.Fatalf("user title: %+v", info)
	}
	var lv Lecture
	e.call("POST", path, TitleRegenerateInput{Force: true}, http.StatusAccepted, &lv)
	if a, ok := auxOf(lv, model.StageTitle); !ok || a.Status != model.StatusPending {
		t.Fatalf("title task %+v", lv.Aux)
	}
	if v, ok, _ := e.st.GetKV(ctx, stages.KVTitleForcePrefix+l.ID); !ok || v == "" {
		t.Fatalf("force not recorded")
	}
	// A title from the file name is replaced without asking.
	res, b := e.upload("/api/courses/"+c.ID+"/lectures", nil, "Week 4.mp4", []byte("m"))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload %d %s", res.StatusCode, b)
	}
	var w4 Lecture
	decodeStrict(t, b, &w4)
	if w4.TitleSource != model.TitleSourceFilename || w4.Title != "Week 4" || w4.OriginalFilename != "Week 4.mp4" {
		t.Fatalf("filename title %+v", w4)
	}
	e.call("POST", "/api/lectures/"+w4.ID+"/launch", nil, http.StatusOK, nil)
	e.allDone(w4.ID)
	e.call("POST", "/api/lectures/"+w4.ID+"/title", nil, http.StatusAccepted, &lv)
	if _, ok := auxOf(lv, model.StageTitle); !ok {
		t.Fatalf("no title task %+v", lv.Aux)
	}
}

// ---- P13.4 numbering, P13.6 renumbering ----

func TestCourseCalendarAndNumbering(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	a, b, d := e.draft(c.ID, "A"), e.draft(c.ID, "B"), e.draft(c.ID, "D")
	e.call("PATCH", "/api/lectures/"+b.ID, LecturePatch{RecordedAt: ptr("2026-10-13T10:00")}, http.StatusOK, nil)
	var lv Lecture
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("2026-10-06 10:00")}, http.StatusOK, &lv)
	if lv.RecordedAt == nil || *lv.RecordedAt != "2026-10-06T10:00" || !lv.RecordedHasTime || lv.RecordedAtSource != model.RecordedAtUser ||
		lv.Numbered || lv.CalendarHint != "term_start" {
		t.Fatalf("recorded_at %+v", lv)
	}
	var cv Course
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-10-01"), SkipWeeks: &[]string{"2026-10-15", "2026-10-13", "2026-10-14"}},
		http.StatusOK, &cv)
	if cv.TermStart == nil || *cv.TermStart != "2026-09-28" || !eqStrings(cv.SkipWeeks, []string{"2026-10-12"}) {
		t.Fatalf("calendar %v %v", cv.TermStart, cv.SkipWeeks)
	}
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if len(list) != 3 || list[0].ID != a.ID || list[1].ID != b.ID || list[2].ID != d.ID {
		t.Fatalf("order %s %s %s", list[0].Title, list[1].Title, list[2].Title)
	}
	if x := list[0]; !x.Numbered || x.Label != "W02L1" || x.Week != 2 || x.N != 1 || x.Seq != 1 || x.CalendarHint != "" {
		t.Fatalf("a %+v", x)
	}
	if x := list[1]; x.Numbered || x.Label != "W??" || x.CalendarHint != "skipped_week" || x.Seq != 2 {
		t.Fatalf("b (skipped week) %+v", x)
	}
	if x := list[2]; x.Label != "W??" || x.CalendarHint != "recorded_at" || x.Seq != 3 {
		t.Fatalf("d (no time) %+v", x)
	}
	// A date without a time; the same day as A → ordered by file name.
	e.call("PATCH", "/api/lectures/"+d.ID, LecturePatch{RecordedAt: ptr("2026-10-06")}, http.StatusOK, &lv)
	if lv.RecordedAt == nil || *lv.RecordedAt != "2026-10-06T00:00" || lv.RecordedHasTime || !lv.SameDayByName || lv.Week != 2 {
		t.Fatalf("date only %+v", lv)
	}
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("2026-09-01T10:00")}, http.StatusOK, &lv)
	if lv.CalendarHint != "before_term" {
		t.Fatalf("before term %+v", lv)
	}
	for _, bad := range []CoursePatch{{TermStart: ptr("10/01/2026")}, {SkipWeeks: &[]string{"soon"}}, {}} {
		e.callErrInfo("PATCH", "/api/courses/"+c.ID, bad, http.StatusBadRequest)
	}
	e.callErrInfo("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("yesterday")}, http.StatusBadRequest)
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("")}, http.StatusOK, &cv)
	if cv.TermStart != nil {
		t.Fatalf("term start not cleared: %v", *cv.TermStart)
	}
	if x := e.lectureView(d.ID); x.CalendarHint != "term_start" || x.Numbered {
		t.Fatalf("without term start %+v", x)
	}
}

// P13.6: after a citation export, a change that renumbers lectures answers
// 409 RenumberConflict listing the changes; confirm saves it.
func TestRenumberConflict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	a, b := e.draft(c.ID, "A"), e.draft(c.ID, "B")
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("2026-10-06T10:00")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+b.ID, LecturePatch{RecordedAt: ptr("2026-10-08T10:00")}, http.StatusOK, nil)
	if err := stages.MarkCiteExported(ctx, e.st, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if !cv.CiteExported {
		t.Fatalf("cite_exported %+v", cv)
	}
	conflict := func(method, path string, body any) RenumberConflict {
		t.Helper()
		res, raw := e.do(method, path, body)
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, raw)
		}
		var rc RenumberConflict
		decodeStrict(t, raw, &rc)
		return rc
	}
	// B moves before A: both numbers change.
	rc := conflict("PATCH", "/api/lectures/"+b.ID, LecturePatch{RecordedAt: ptr("2026-10-05T09:00")})
	if rc.Error.Code != "calendar.renumber" || len(rc.Changes) != 2 || rc.Error.Params["count"] != float64(2) {
		t.Fatalf("conflict %+v", rc)
	}
	byID := map[string]RenumberChange{}
	for _, ch := range rc.Changes {
		byID[ch.LectureID] = ch
	}
	if x := byID[b.ID]; x.From != "W02L2" || x.To != "W02L1" || x.Title != "B" {
		t.Fatalf("b change %+v", x)
	}
	if x := byID[a.ID]; x.From != "W02L1" || x.To != "W02L2" {
		t.Fatalf("a change %+v", x)
	}
	if x := e.lectureView(b.ID); x.RecordedAt == nil || *x.RecordedAt != "2026-10-08T10:00" {
		t.Fatalf("409 saved anyway: %+v", x.RecordedAt)
	}
	var lv Lecture
	e.call("PATCH", "/api/lectures/"+b.ID, LecturePatch{RecordedAt: ptr("2026-10-05T09:00"), Confirm: true}, http.StatusOK, &lv)
	if lv.Label != "W02L1" {
		t.Fatalf("confirmed %+v", lv)
	}
	// A change that keeps every number needs no confirmation.
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("2026-10-06T11:00")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{Title: ptr("A2")}, http.StatusOK, nil)
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{Name: ptr("Crypto")}, http.StatusOK, nil)
	// Back to automatic (no rule, no metadata): A loses its number.
	rc = conflict("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("")})
	if len(rc.Changes) != 1 || rc.Changes[0].LectureID != a.ID || rc.Changes[0].To != "W??-"+a.ID[:6] { // the citation label (review item 5)
		t.Fatalf("clear %+v", rc)
	}
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr(""), Confirm: true}, http.StatusOK, &lv)
	if lv.RecordedAt != nil || lv.RecordedAtSource != model.RecordedAtNone || lv.CalendarHint != "recorded_at" {
		t.Fatalf("cleared %+v", lv)
	}
	// The course calendar.
	rc = conflict("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-10-05")})
	if len(rc.Changes) != 1 || rc.Changes[0].LectureID != b.ID || rc.Changes[0].To != "W01L1" {
		t.Fatalf("term start %+v", rc)
	}
	e.call("PATCH", "/api/courses/"+c.ID, CoursePatch{TermStart: ptr("2026-10-05"), Confirm: true}, http.StatusOK, &cv)
	if cv.TermStart == nil || *cv.TermStart != "2026-10-05" {
		t.Fatalf("confirmed term start %+v", cv.TermStart)
	}
}

// ---- P13.3 file-name rules ----

const testRule = `^CSC1003_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})_(?P<hour>\d{2})(?P<minute>\d{2})`

func (e *env) uploadNamed(courseID, filename string) Lecture {
	e.t.Helper()
	res, b := e.upload("/api/courses/"+courseID+"/lectures", nil, filename, []byte("media"))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("upload %s: %d %s", filename, res.StatusCode, b)
	}
	var l Lecture
	decodeStrict(e.t, b, &l)
	return l
}

func TestFilenameRuleFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	base := "/api/courses/" + c.ID + "/filename-rule"
	a := e.uploadNamed(c.ID, "CSC1003_2025-10-06_1000.mp4")
	b := e.uploadNamed(c.ID, "CSC1003_2025-10-08_1400.mp4")
	x := e.uploadNamed(c.ID, "random.mp4")
	if a.OriginalFilename != "CSC1003_2025-10-06_1000.mp4" || a.RecordedAtSource != model.RecordedAtNone || a.TitleSource != model.TitleSourceFilename {
		t.Fatalf("upload without a rule %+v", a)
	}
	var v FilenameRuleView
	e.call("GET", base, nil, http.StatusOK, &v)
	if v.Rule != nil || v.Proposal != nil || v.Task != nil || v.Preview == nil || len(v.Preview) != 0 {
		t.Fatalf("empty view %+v", v)
	}
	var pr FilenamePreviewResult
	e.call("POST", base+"/preview", FilenamePreviewInput{Regex: testRule}, http.StatusOK, &pr)
	if !pr.OK || pr.Matched != 2 || pr.Total != 3 || len(pr.Preview) != 3 {
		t.Fatalf("preview %+v", pr)
	}
	statuses := map[string]FilenamePreviewRow{}
	for _, r := range pr.Preview {
		statuses[r.Filename] = r
	}
	if r := statuses[a.OriginalFilename]; r.Status != "ok" || r.RecordedAt == nil || *r.RecordedAt != "2025-10-06T10:00" || !r.HasTime {
		t.Fatalf("row a %+v", r)
	}
	if r := statuses["random.mp4"]; r.Status != "unmatched" || r.RecordedAt != nil {
		t.Fatalf("row random %+v", r)
	}
	if info := e.callErrInfo("POST", base+"/preview", FilenamePreviewInput{}, http.StatusBadRequest); info.Code != "filename_rule.regex_empty" {
		t.Fatalf("empty regex %+v", info)
	}
	for _, bad := range []string{"(", `^CSC(?P<year>\d{4})`} {
		if info := e.callErrInfo("PUT", base, FilenamePreviewInput{Regex: bad}, http.StatusBadRequest); info.Code != "filename_rule.invalid" || info.Detail == "" {
			t.Fatalf("invalid %q: %+v", bad, info)
		}
	}
	// Apply an edited rule: source user; the lectures are re-resolved.
	var cv Course
	e.call("PUT", base, FilenamePreviewInput{Regex: testRule}, http.StatusOK, &cv)
	if cv.FilenameRule == nil || cv.FilenameRule.Source != "user" || cv.FilenameRule.SamplesMatched != 2 || cv.FilenameRule.SamplesTotal != 3 {
		t.Fatalf("applied %+v", cv.FilenameRule)
	}
	if l := e.lectureView(a.ID); l.RecordedAt == nil || *l.RecordedAt != "2025-10-06T10:00" || l.RecordedAtSource != model.RecordedAtFilename {
		t.Fatalf("a after the rule %+v", l)
	}
	if l := e.lectureView(x.ID); l.RecordedAt != nil {
		t.Fatalf("random after the rule %+v", l)
	}
	e.call("GET", base, nil, http.StatusOK, &v)
	if v.Rule == nil || v.Rule.Regex != testRule || len(v.Preview) != 3 {
		t.Fatalf("view with a rule %+v", v)
	}
	// A later upload is parsed at once with the saved rule.
	n := e.uploadNamed(c.ID, "CSC1003_2025-10-13_0900.mp4")
	if n.RecordedAt == nil || *n.RecordedAt != "2025-10-13T09:00" || n.RecordedAtSource != model.RecordedAtFilename {
		t.Fatalf("upload with a rule %+v", n)
	}

	// A generated proposal: shown, ignored, then applied (source llm).
	proposal := func() {
		t.Helper()
		rule, _ := json.Marshal(FilenameRule{Regex: testRule + `.*`, Source: "llm", CreatedAt: "2026-10-23T20:00:00Z", SamplesMatched: 3, SamplesTotal: 4})
		p, _ := json.Marshal(model.FilenameProposal{Rule: rule, Explanation: "Course code, date and time.", CreatedAt: "2026-10-23T20:00:00Z"})
		if err := e.st.SetKV(ctx, model.KVFilenameProposal+c.ID, string(p)); err != nil {
			t.Fatal(err)
		}
	}
	proposal()
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if !cv.FilenameProposal {
		t.Fatalf("proposal flag %+v", cv)
	}
	e.call("GET", base, nil, http.StatusOK, &v)
	if v.Proposal == nil || v.Proposal.Explanation == "" || !v.Proposal.OK || v.Proposal.Total != 4 || len(v.Proposal.Preview) != 4 {
		t.Fatalf("proposal %+v", v.Proposal)
	}
	e.call("DELETE", base+"/proposal", nil, http.StatusOK, &cv)
	if cv.FilenameProposal {
		t.Fatalf("proposal not dismissed")
	}
	if info := e.callErrInfo("DELETE", base+"/proposal", nil, http.StatusNotFound); info.Code != "filename_rule.no_proposal" {
		t.Fatalf("dismiss twice %+v", info)
	}
	proposal()
	e.call("PUT", base, FilenamePreviewInput{Regex: testRule + `.*`}, http.StatusOK, &cv)
	if cv.FilenameRule == nil || cv.FilenameRule.Source != "llm" || cv.FilenameProposal {
		t.Fatalf("proposal applied %+v %v", cv.FilenameRule, cv.FilenameProposal)
	}

	// Undo of an automatic first apply.
	if info := e.callErrInfo("POST", base+"/undo", nil, http.StatusNotFound); info.Code != "filename_rule.no_undo" {
		t.Fatalf("nothing to undo %+v", info)
	}
	undo, _ := json.Marshal(stages.FilenameUndo{Rule: nil, CreatedAt: "2026-10-23T20:00:00Z", Lectures: []stages.FilenameUndoLecture{
		{ID: b.ID, RecordedAtSource: model.RecordedAtNone, Title: b.Title, TitleSource: model.TitleSourceFilename}}})
	if err := e.st.SetKV(ctx, model.KVFilenameUndo+c.ID, string(undo)); err != nil {
		t.Fatal(err)
	}
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if !cv.FilenameUndo {
		t.Fatalf("undo flag")
	}
	e.call("POST", base+"/undo", nil, http.StatusOK, &cv)
	if cv.FilenameRule != nil || cv.FilenameUndo {
		t.Fatalf("after undo %+v", cv)
	}
	if l := e.lectureView(b.ID); l.RecordedAt != nil || l.RecordedAtSource != model.RecordedAtNone {
		t.Fatalf("b after undo %+v", l)
	}

	// "重新识别文件名格式": needs the organizer role and file names.
	if info := e.callErrInfo("POST", base+"/generate", nil, http.StatusConflict); info.Code != "filename_rule.not_configured" {
		t.Fatalf("generate without a role %+v", info)
	}
	e.configureRole(model.RoleOrganizer)
	e.call("POST", base+"/generate", nil, http.StatusAccepted, &v)
	if v.Task == nil || v.Task.Stage != model.StageFilenameRule || v.Task.Status != model.StatusPending {
		t.Fatalf("generate %+v", v.Task)
	}
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.FilenameTask == nil || cv.FilenameTask.Status != model.StatusPending {
		t.Fatalf("course task %+v", cv.FilenameTask)
	}
	empty := e.course("Empty")
	if info := e.callErrInfo("POST", "/api/courses/"+empty.ID+"/filename-rule/generate", nil, http.StatusConflict); info.Code != "filename_rule.no_samples" {
		t.Fatalf("no samples %+v", info)
	}
	// The course task takes retry and cancel, not rerun.
	e.callErrInfo("POST", "/api/courses/"+c.ID+"/stages/filename_rule/rerun", nil, http.StatusBadRequest)
	e.failCoded(model.ScopeCourse, c.ID, model.StageFilenameRule, "filename_rule.rejected", "2/5 matched")
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.FilenameTask == nil || cv.FilenameTask.ErrorInfo == nil || cv.FilenameTask.ErrorInfo.Code != "filename_rule.rejected" ||
		!eqStrings(cv.FilenameTask.AllowedActions, []string{"retry"}) {
		t.Fatalf("failed task %+v", cv.FilenameTask)
	}
	e.call("POST", "/api/courses/"+c.ID+"/stages/filename_rule/retry", nil, http.StatusOK, &cv)
	if cv.FilenameTask == nil || (cv.FilenameTask.Status != model.StatusQueued && cv.FilenameTask.Status != model.StatusPending) {
		t.Fatalf("retried task %+v", cv.FilenameTask)
	}
	e.callErrInfo("GET", "/api/courses/nope/filename-rule", nil, http.StatusNotFound)
}

// ---- P12.2 holidays, P14.6/P14.7 UI settings, P12 provider tariff ----

func TestHolidaysAndUISettings(t *testing.T) {
	e := newEnv(t)
	var hv HolidaysView
	e.call("GET", "/api/settings/holidays", nil, http.StatusOK, &hv)
	if hv.Edited || len(hv.Years["2026"]) == 0 || hv.Source == "" {
		t.Fatalf("built-in holidays %+v", hv)
	}
	hv = HolidaysView{} // decode into a fresh value (a map is merged into)
	e.call("PUT", "/api/settings/holidays", map[string]any{"years": map[string][]string{"2027": {"2027-02-08", "2027-01-01"}}}, http.StatusOK, &hv)
	if !hv.Edited || !eqStrings(hv.Years["2027"], []string{"2027-01-01", "2027-02-08"}) || len(hv.Years) != 1 {
		t.Fatalf("user holidays %+v", hv)
	}
	var again HolidaysView
	e.call("GET", "/api/settings/holidays", nil, http.StatusOK, &again)
	if !reflect.DeepEqual(again, hv) {
		t.Fatalf("round trip %+v != %+v", again, hv)
	}
	if info := e.callErrInfo("PUT", "/api/settings/holidays", map[string]any{"years": map[string][]string{"2027": {"2027-13-01"}}}, http.StatusBadRequest); info.Code != "tariff.invalid_holiday_data" {
		t.Fatalf("invalid holidays %+v", info)
	}
	hv = HolidaysView{}
	e.call("PUT", "/api/settings/holidays", map[string]any{"years": nil}, http.StatusOK, &hv)
	if hv.Edited || len(hv.Years["2026"]) == 0 {
		t.Fatalf("restored %+v", hv)
	}

	var ui UISettings
	e.call("GET", "/api/settings/ui", nil, http.StatusOK, &ui)
	if ui != (UISettings{}) {
		t.Fatalf("unset ui %+v", ui)
	}
	e.call("PUT", "/api/settings/ui", UISettings{Locale: "yue-Hant-HK", ZhDisplay: "hk"}, http.StatusOK, &ui)
	var got UISettings
	e.call("GET", "/api/settings/ui", nil, http.StatusOK, &got)
	if got != (UISettings{Locale: "yue-Hant-HK", ZhDisplay: "hk"}) || got != ui {
		t.Fatalf("ui round trip %+v %+v", got, ui)
	}
	// A settings validation error: its own code (codes-p2) or the
	// transitional settings.message with the text.
	if info := e.callErrInfo("PUT", "/api/settings/ui", UISettings{Locale: "xx"}, http.StatusBadRequest); !strings.HasPrefix(info.Code, "settings.") ||
		(info.Code == "settings.message" && info.Params["text"] == "") {
		t.Fatalf("bad locale %+v", info)
	}
	e.call("PUT", "/api/settings/ui", UISettings{}, http.StatusOK, &ui)
	if ui != (UISettings{}) {
		t.Fatalf("cleared %+v", ui)
	}
}

func TestProviderTariff(t *testing.T) {
	e := newEnv(t)
	// patch decodes each answer into a fresh value (omitted fields would
	// otherwise keep the previous answer's values).
	var id string
	patch := func(in ProviderInput) ProviderView {
		t.Helper()
		var pv ProviderView
		e.call("PATCH", "/api/providers/"+id, in, http.StatusOK, &pv)
		return pv
	}
	var pv ProviderView
	e.call("POST", "/api/providers", ProviderInput{Name: ptr("DeepSeek 2"), Type: ptr(model.ProviderOpenAI), BaseURL: ptr("https://api.deepseek.com"),
		APIKey: ptr("sk-test-0000")}, http.StatusCreated, &pv)
	id = pv.ID
	if len(pv.Tariff) == 0 || string(pv.Tariff) == "null" || !strings.Contains(string(pv.Tariff), "Asia/Shanghai") ||
		pv.TariffState == nil || pv.TariffState.Timezone != "Asia/Shanghai" || pv.OffpeakOnly {
		t.Fatalf("DeepSeek preset %s %+v", pv.Tariff, pv.TariffState)
	}
	preset := append(json.RawMessage(nil), pv.Tariff...)
	if pv = patch(ProviderInput{OffpeakOnly: ptr(true)}); !pv.OffpeakOnly || pv.TariffState == nil || !pv.TariffState.OffpeakOnly {
		t.Fatalf("offpeak only %+v", pv)
	}
	info := e.callErrInfo("PATCH", "/api/providers/"+id, ProviderInput{Tariff: json.RawMessage(`{"timezone":"Mars/Base"}`)}, http.StatusBadRequest)
	if info.Code != "tariff.invalid_timezone" || info.Params["timezone"] != "Mars/Base" {
		t.Fatalf("invalid tariff %+v", info)
	}
	if pv = patch(ProviderInput{Tariff: json.RawMessage(`null`)}); string(pv.Tariff) != "null" || pv.TariffState != nil || pv.OffpeakOnly {
		t.Fatalf("cleared %s %+v", pv.Tariff, pv)
	}
	e.callErrInfo("PATCH", "/api/providers/"+id, ProviderInput{OffpeakOnly: ptr(true)}, http.StatusBadRequest)
	if pv = patch(ProviderInput{Tariff: preset}); pv.TariffState == nil || string(pv.Tariff) == "null" {
		t.Fatalf("tariff back %+v", pv)
	}
}

// ---- P14.3 structured stale reasons and coded job errors ----

func TestStaleInfoAndErrorInfo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.allDone(l.ID)
	reason := model.StaleReasonJSON(model.StaleCodeGlossary, map[string]any{"from": 3, "to": 4})
	rep := e.moveJob(model.ScopeLecture, l.ID, model.StageRepair)
	if err := state.TransitionJob(ctx, e.st, rep.ID, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &reason}); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StageTranslate, "stale") // no reason recorded
	v := e.lectureView(l.ID)
	if v.Stale == nil || !eqStrings(v.Stale.Stages, []string{"repair", "translate"}) || len(v.Stale.Reasons) != 2 ||
		v.Stale.Reasons[0].Code != model.StaleCodeGlossary || v.Stale.Reasons[0].Params["from"] != float64(3) ||
		v.Stale.Reasons[1].Code != model.StaleCodeInput {
		t.Fatalf("stale summary %+v", v.Stale)
	}
	r := stageOf(v.Stages, model.StageRepair)
	if r.StaleInfo == nil || r.StaleInfo.Code != model.StaleCodeGlossary || r.StaleReason == nil || *r.StaleReason != "术语表已更新（v3 → v4）" {
		t.Fatalf("repair %+v", r)
	}
	if v.StaleText == nil || !strings.Contains(*v.StaleText, "术语表已更新（v3 → v4）") {
		t.Fatalf("stale text %v", v.StaleText)
	}
	if x := stageOf(v.Stages, model.StageSummary); x.StaleInfo != nil || x.StaleReason != nil {
		t.Fatalf("summary is not stale %+v", x)
	}

	f := e.lecture(c.ID, "F")
	j := e.moveJob(model.ScopeLecture, f.ID, model.StagePrep, "queued", "running")
	text := state.EncodeJobError(model.Errorf("media.corrupt", nil, errors.New("Invalid data found")))
	if _, err := state.FailJob(ctx, e.st, j.ID, text, false); err != nil {
		t.Fatal(err)
	}
	p := stageOf(e.lectureView(f.ID).Stages, model.StagePrep)
	if p.ErrorInfo == nil || p.ErrorInfo.Code != "media.corrupt" || p.Error == nil || *p.Error != "Invalid data found" {
		t.Fatalf("coded prep error %+v", p)
	}
	// Legacy text stays text.
	g := e.lecture(c.ID, "G")
	e.moveJob(model.ScopeLecture, g.ID, model.StagePrep, "queued", "running", "failed")
	if p := stageOf(e.lectureView(g.ID).Stages, model.StagePrep); p.ErrorInfo != nil || p.Error == nil || !strings.Contains(*p.Error, "boom") {
		t.Fatalf("legacy prep error %+v", p)
	}
}

// ---- P14.3: a walk over error responses ----

// Every error response is {"error": {"code", "params"}} (also checked for
// every request of every test by checkErrorBodies); here a sample of each
// area with its code and params, plus the per-id codes of batch results.
func TestErrorResponsesAreCoded(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "Running one")
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, "queued", "running")
	cases := []struct {
		method, path string
		body         any
		status       int
		code         string
		params       map[string]any
	}{
		{"GET", "/api/nope", nil, 404, "api.not_found", map[string]any{"method": "GET", "path": "/api/nope"}},
		{"POST", "/api/courses", "x", 400, "request.bad_json", nil},
		{"POST", "/api/courses", map[string]string{"name": " "}, 400, "course.name_empty", nil},
		{"GET", "/api/courses/nope", nil, 404, "course.not_found", nil},
		{"GET", "/api/lectures/nope", nil, 404, "lecture.not_found", nil},
		{"DELETE", "/api/lectures/" + l.ID, nil, 409, "lecture.running", map[string]any{"title": "Running one"}},
		{"POST", "/api/lectures/" + l.ID + "/stages/chapters/rerun", nil, 400, "stage.invalid", map[string]any{"stage": "chapters", "action": "rerun"}},
		{"POST", "/api/lectures/" + l.ID + "/launch", nil, 409, "lecture.already_launched", nil},
		{"PATCH", "/api/lectures/" + l.ID, LecturePatch{Seq: ptr(3)}, 400, "lecture.seq_readonly", nil},
		{"PATCH", "/api/lectures/" + l.ID, LecturePatch{RecordedAt: ptr("2026-02-30")}, 400, "lecture.recorded_at_invalid", map[string]any{"value": "2026-02-30"}},
		{"PATCH", "/api/courses/" + c.ID, CoursePatch{TermStart: ptr("x")}, 400, "course.term_start_invalid", map[string]any{"value": "x"}},
		{"GET", "/api/lectures/" + l.ID + "/sentences?limit=0", nil, 400, "request.bad_param", map[string]any{"name": "limit"}},
		{"POST", "/api/lectures/launch", BatchRequest{}, 400, "lecture.batch_empty", nil},
		{"GET", "/api/lectures/nope/chapters", nil, 404, "lecture.not_found", nil}, // never generated: 200 (power-brief ruling 21)
		{"POST", "/api/courses/" + c.ID + "/filename-rule/preview", FilenamePreviewInput{}, 400, "filename_rule.regex_empty", nil},
		{"GET", "/api/resources", nil, 503, "server.unavailable", map[string]any{"service": "resources"}},
		{"POST", "/api/courses/" + c.ID + "/terms", map[string]string{"en": " "}, 400, "term.en_empty", nil},
		{"PATCH", "/api/terms/nope", TermPatch{Enabled: ptr(true)}, 404, "term.not_found", nil},
		{"GET", "/api/courses/" + c.ID + "/glossary/report?version=7", nil, 404, "glossary.report_not_found", map[string]any{"version": float64(7)}},
	}
	for _, tc := range cases {
		info := e.callErrInfo(tc.method, tc.path, tc.body, tc.status)
		if info.Code != tc.code {
			t.Errorf("%s %s: code %q, want %q", tc.method, tc.path, info.Code, tc.code)
		}
		for k, want := range tc.params {
			if got := info.Params[k]; got != want {
				t.Errorf("%s %s: param %s = %v, want %v", tc.method, tc.path, k, got, want)
			}
		}
	}
	// Batch results carry the coded error and its transitional Chinese text.
	var br BatchResponse
	e.call("POST", "/api/lectures/launch", BatchRequest{IDs: []string{"nope", l.ID}}, http.StatusOK, &br)
	if len(br.Results) != 2 || br.Results[0].ErrorInfo == nil || br.Results[0].ErrorInfo.Code != "lecture.not_found" ||
		br.Results[0].Error != "找不到这节讲座（可能已被删除）" || br.Results[1].ErrorInfo == nil || br.Results[1].ErrorInfo.Code != "lecture.already_launched" {
		t.Fatalf("batch %+v", br.Results)
	}
}

// The dashboard's failures carry the coded error like StageView (v2
// P14.3): error_info from job.error (ParseJobError), error = its detail; a
// legacy text has no error_info; the §4.7 text reads as job.interrupted.
func TestDashboardFailureErrorInfo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	fail := func(title, raw string) Lecture {
		l := e.lecture(c.ID, title)
		j := e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
		if _, err := state.FailJob(ctx, e.st, j.ID, raw, false); err != nil {
			t.Fatal(err)
		}
		return l
	}
	coded := fail("Coded", state.EncodeJobError(model.Errorf("media.corrupt", nil, errors.New("退出码 1：moov atom not found"))))
	legacy := fail("Legacy", "boom: worker exited with code 1")
	restart := fail("Restart", model.ErrInterruptedByRestart)
	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	byID := map[string]FailureItem{}
	for _, f := range d.Failures {
		byID[f.LectureID] = f
	}
	if f := byID[coded.ID]; f.ErrorInfo == nil || f.ErrorInfo.Code != "media.corrupt" || f.ErrorInfo.Detail != "退出码 1：moov atom not found" ||
		f.Error == nil || *f.Error != "退出码 1：moov atom not found" {
		t.Errorf("coded failure %+v / %+v", f, f.ErrorInfo)
	}
	if f := byID[legacy.ID]; f.ErrorInfo != nil || f.Error == nil || *f.Error != "boom: worker exited with code 1" {
		t.Errorf("legacy failure %+v / %+v", f, f.ErrorInfo)
	}
	if f := byID[restart.ID]; f.ErrorInfo == nil || f.ErrorInfo.Code != state.CodeInterrupted || f.Error == nil || *f.Error != model.ErrInterruptedByRestart {
		t.Errorf("interrupted failure %+v / %+v", f, f.ErrorInfo)
	}
	// the same as the lecture's StageView
	var lv Lecture
	e.call("GET", "/api/lectures/"+coded.ID, nil, http.StatusOK, &lv)
	if sv, f := stageOf(lv.Stages, model.StagePrep), byID[coded.ID]; !reflect.DeepEqual(sv.ErrorInfo, f.ErrorInfo) || *sv.Error != *f.Error {
		t.Errorf("stage view %+v / %v differs from the failure item", sv.ErrorInfo, sv.Error)
	}
}

// SPEC-v2 D3 (lead ruling at the phase-2 gate): "今日用量" counts the
// requests without a price (cost_unpriced_requests) and shows no amount when
// all of the day's requests were unpriced; while a daily budget applies, a
// running LLM stage notes that the budget may be underestimated.
func TestUsageTodayUnpricedAndBudgetNote(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.set.UpdateProvider(ctx, "deepseek", settingsPrices(2, 0.5, 8, "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := e.set.SetGeneral(ctx, model.GeneralSettings{KeepOriginalMedia: true, Notifications: true, DailyBudget: 5}); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	if err := e.st.AddUsageDaily(ctx, model.UsageDaily{ProviderID: "deepseek", Day: today, Requests: 3, PromptTokens: 3000, EstimatedCost: 0.25}); err != nil {
		t.Fatal(err)
	}
	if err := settings.AddUnpricedRequest(ctx, e.st, "deepseek", today); err != nil {
		t.Fatal(err)
	}
	usage := func() ProviderUsage {
		var d Dashboard
		e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
		for _, u := range d.UsageToday {
			if u.ProviderID == "deepseek" {
				return u
			}
		}
		t.Fatalf("no deepseek row: %+v", d.UsageToday)
		return ProviderUsage{}
	}
	if u := usage(); u.Cost == nil || *u.Cost != 0.25 || u.CostUnpricedRequests != 1 || u.Currency == nil {
		t.Errorf("partly unpriced: %+v", u)
	}

	// A running LLM stage carries the note; prep (no LLM) does not.
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	e.moveJob(model.ScopeLecture, l.ID, model.StageSummary, model.StatusQueued, model.StatusRunning)
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	sum := stageOf(lv.Stages, model.StageSummary)
	if sum.NoteInfo == nil || sum.NoteInfo.Code != state.CodeNoteBudgetUnpriced || fmt.Sprint(sum.NoteInfo.Params["n"]) != "1" || sum.Note == nil {
		t.Errorf("summary note %+v / %v", sum.NoteInfo, sum.Note)
	}
	if p := stageOf(lv.Stages, model.StagePrep); p.NoteInfo != nil {
		t.Errorf("prep note %+v", p.NoteInfo)
	}

	// Every request unpriced: no amount, only tokens.
	for i := 0; i < 2; i++ {
		if err := settings.AddUnpricedRequest(ctx, e.st, "deepseek", today); err != nil {
			t.Fatal(err)
		}
	}
	if u := usage(); u.Cost != nil || u.Currency != nil || u.CostUnpricedRequests != 3 || u.PromptTokens != 3000 {
		t.Errorf("all unpriced: %+v", u)
	}

	// Without a budget there is nothing to underestimate.
	if err := e.set.SetGeneral(ctx, model.GeneralSettings{KeepOriginalMedia: true, Notifications: true}); err != nil {
		t.Fatal(err)
	}
	var lv2 Lecture // a fresh value: decoding into lv would keep its old note_info
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv2)
	if n := stageOf(lv2.Stages, model.StageSummary).NoteInfo; n != nil {
		t.Errorf("note without a budget %+v", n)
	}
}

// P8.1: the disk statistics show the playback files (every lecture's
// stream.mp4) on their own; they are part of the course and data totals.
func TestSystemDiskStreamMB(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l1 := e.lecture(c.ID, "L1")
	l2 := e.lecture(c.ID, "L2")
	write := func(path string, mb int) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, mb<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.dirOf(l1), model.StreamFile), 2)
	write(filepath.Join(e.dirOf(l2), model.StreamFile), 3)
	write(filepath.Join(e.dirOf(l1), "media.mp4"), 4)
	// a file of the same name that is not a lecture's playback file
	write(filepath.Join(model.CourseDir(e.cfg.DataDir, c.ID), "materials", "m1", model.StreamFile), 1)
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	if sys.Disk.StreamMB != 5 || sys.Disk.Courses[c.ID] != 10 || sys.Disk.DataMB < 10 {
		t.Errorf("disk %+v, want stream 5 MB of course 10 MB", sys.Disk)
	}
}

// P13.6: an export file downloads as "{number} {English title}" plus its
// ending, the lecture's current ones (also for files written under an older
// name); a citation JSON keeps the name that matches its content. The
// header carries an ASCII fallback and the UTF-8 name.
func TestExportDownloadNameUsesNumber(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "Stream Ciphers 流密码")
	dir := filepath.Join(e.dirOf(l), model.ExportDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Old title.bilingual.md", "Old title.cite.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	disposition := func(file string) string {
		res, _ := e.do("GET", "/api/lectures/"+l.ID+"/export/"+url.PathEscape(file), nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", file, res.StatusCode)
		}
		return res.Header.Get("Content-Disposition")
	}
	// unnumbered (no term start): the unique citation label, "?" → "_"
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	want := "W__-" + strings.TrimPrefix(lv.CiteLabel, "W??-") + " Stream Ciphers 流密码.bilingual.md"
	if !strings.HasPrefix(lv.CiteLabel, "W??-") || !strings.Contains(disposition("Old title.bilingual.md"), "filename*=UTF-8''"+url.PathEscape(want)) {
		t.Errorf("unnumbered: %q, want %q", disposition("Old title.bilingual.md"), want)
	}
	// numbered: term from Monday 2026-09-28, recorded in week 2 → W02L1
	termStart, rec := "2026-09-28", "2026-10-06T09:00"
	if err := e.st.SetCourseCalendar(ctx, c.ID, &termStart, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLectureRecordedAt(ctx, l.ID, &rec, model.RecordedAtUser); err != nil {
		t.Fatal(err)
	}
	got := disposition("Old title.bilingual.md")
	if !strings.Contains(got, `filename="W02L1 Stream Ciphers ___.bilingual.md"`) ||
		!strings.Contains(got, "filename*=UTF-8''"+url.PathEscape("W02L1 Stream Ciphers 流密码.bilingual.md")) {
		t.Errorf("numbered: %q", got)
	}
	if got := disposition("Old title.cite.json"); !strings.Contains(got, `filename="Old title.cite.json"`) {
		t.Errorf("citation JSON renamed: %q", got)
	}
}

// P13.6 (lead ruling): only the user's download of a citation JSON is an
// export (a lecture's *.cite.json or the course file), not other files.
func TestCiteExportRecordedOnDownload(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	dir := filepath.Join(e.dirOf(l), model.ExportDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"L.md", "L.cite.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exported := func() bool {
		var cv Course
		e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
		return cv.CiteExported
	}
	if res, _ := e.do("GET", "/api/lectures/"+l.ID+"/export/L.md", nil); res.StatusCode != http.StatusOK || exported() {
		t.Fatalf("markdown download: %d, exported %v", res.StatusCode, exported())
	}
	if res, _ := e.do("GET", "/api/lectures/"+l.ID+"/export/L.cite.json", nil); res.StatusCode != http.StatusOK || !exported() {
		t.Fatalf("cite download: %d, exported %v", res.StatusCode, exported())
	}
}
