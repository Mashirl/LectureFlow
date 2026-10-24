// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// End-to-end tests of SPEC-v1.1 through the real HTTP API, scheduler and
// stages (fake LLM, fake ASR): manual launch (A3), glossary change → update
// (A1, A2), model change → rerun decision (A4), forced delete (A7) and the
// course-level citation JSON (A11).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// doRaw sends a request and returns the status and body without failing on
// error statuses.
func (e *e2e) doRaw(method, path string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func stageOf(l api.Lecture, stage string) api.StageView {
	for _, s := range l.Stages {
		if s.Stage == stage {
			return s
		}
	}
	return api.StageView{}
}

func (e *e2e) jobID(lectureID, stage string) string {
	e.t.Helper()
	j, err := e.app.Store.GetCurrentJob(context.Background(), model.ScopeLecture, lectureID, stage)
	if err != nil {
		e.t.Fatal(err)
	}
	return j.ID
}

// A1 + A2: a glossary change makes repair and translate stale (never asr),
// with a readable reason; "更新" reruns the earliest stale stage and the rest
// follows automatically.
func TestV11GlossaryChangeAndUpdate(t *testing.T) {
	e := newE2E(t)
	courseID, lectureID := e.runPipeline()
	asrJob, prepJob := e.jobID(lectureID, model.StageASR), e.jobID(lectureID, model.StagePrep)

	e.postJSON("/api/courses/"+courseID+"/terms", map[string]string{"en": "write-back cache", "zh": "写回缓存"}, nil)
	e.waitFor("repair and translate stale", func() bool {
		l := e.lecture(lectureID)
		return stageOf(l, model.StageRepair).Status == model.StatusStale && stageOf(l, model.StageTranslate).Status == model.StatusStale
	}, lectureID)

	l := e.lecture(lectureID)
	for _, st := range []string{model.StagePrep, model.StageASR, model.StageSummary} {
		if s := stageOf(l, st); s.Status != model.StatusDone || s.DisplayStatus != model.StatusDone {
			t.Fatalf("%s after a glossary change: status %s display %s (A1: only repair and downstream)", st, s.Status, s.DisplayStatus)
		}
	}
	repair := stageOf(l, model.StageRepair)
	if repair.StaleReason == nil || !strings.HasPrefix(*repair.StaleReason, "术语表已更新（v") {
		t.Fatalf("repair stale_reason = %v", repair.StaleReason)
	}
	if exp := stageOf(l, model.StageExport); exp.Status != model.StatusDone || exp.DisplayStatus != model.DisplayOutdated {
		t.Fatalf("export: status %s display %s, want done/outdated (A2.1)", exp.Status, exp.DisplayStatus)
	}
	if l.StaleText == nil || !strings.Contains(*l.StaleText, "需要更新：修复、翻译") {
		t.Fatalf("stale_text = %v", l.StaleText)
	}
	if !contains(l.LectureActions, model.LectureActionUpdate) {
		t.Fatalf("lecture actions %v lack update", l.LectureActions)
	}

	var updated api.Lecture
	e.postJSON("/api/lectures/"+lectureID+"/update", struct{}{}, &updated)
	e.waitFor("update to finish", func() bool { return allDone(e.lecture(lectureID)) }, lectureID)
	if e.jobID(lectureID, model.StageASR) != asrJob || e.jobID(lectureID, model.StagePrep) != prepJob {
		t.Fatal("update must not rerun prep or asr")
	}
	if l := e.lecture(lectureID); l.StaleText != nil || contains(l.LectureActions, model.LectureActionUpdate) {
		t.Fatalf("after update: stale_text %v actions %v", l.StaleText, l.LectureActions)
	}
}

// A4: changing a role's model never makes anything stale; rerunning asks
// for a choice (409 needs_model_choice); "current" sends the new requests to
// the new model, "snapshot" would have kept the old one.
func TestV11ModelChangeRerunDecision(t *testing.T) {
	e := newE2E(t)
	courseID, lectureID := e.runPipeline()
	ctx := context.Background()

	var set api.Settings
	e.do(http.MethodGet, "/api/settings", nil, "", &set)
	var providerID string
	for _, r := range set.Roles {
		if r.Role == model.RoleTranslate && r.ProviderID != nil {
			providerID = *r.ProviderID
		}
	}
	if providerID == "" {
		t.Fatalf("translate role not configured: %+v", set.Roles)
	}
	b, _ := json.Marshal(api.RoleInput{ProviderID: providerID, Model: "fake-translate-v2"})
	e.do(http.MethodPut, "/api/settings/roles/"+model.RoleTranslate, bytes.NewReader(b), "application/json", nil)

	// Model changes never make a stage stale (A4.3 last line), even after an
	// invalidation pass.
	if err := e.app.Sched.InvalidateCourseWith(ctx, courseID, state.StaleCause{}); err != nil {
		t.Fatal(err)
	}
	if l := e.lecture(lectureID); !allDone(l) {
		t.Fatalf("a model change made a stage leave done: %+v", l.Stages)
	}

	path := "/api/lectures/" + lectureID + "/stages/translate/rerun"
	status, body := e.doRaw(http.MethodPost, path, nil)
	if status != http.StatusConflict {
		t.Fatalf("rerun without a choice: HTTP %d %s, want 409", status, body)
	}
	var nm api.NeedsModelChoice
	if err := json.Unmarshal(body, &nm); err != nil {
		t.Fatal(err)
	}
	// P14.3: the error is {"code": "needs_model_choice"} (was a bare string).
	if nm.Error.Code != model.ErrNeedsModelChoice || len(nm.Diff) != 1 || nm.Diff[0].Stage != model.StageTranslate ||
		nm.Diff[0].Current.Model != "fake-translate-v2" || nm.Diff[0].Snapshot.Model == "fake-translate-v2" || !nm.Diff[0].SnapshotUsable {
		t.Fatalf("409 body: %s", body)
	}
	if l := e.lecture(lectureID); !allDone(l) {
		t.Fatal("a 409 must not change anything")
	}

	e.fake.Reset()
	status, body = e.doRaw(http.MethodPost, path, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent})
	if status != http.StatusOK {
		t.Fatalf("rerun with current: HTTP %d %s", status, body)
	}
	e.waitFor("rerun with the new model", func() bool { return allDone(e.lecture(lectureID)) }, lectureID)
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if len(reqs) == 0 {
		t.Fatal("no translate requests after the rerun")
	}
	for _, r := range reqs {
		if r.Model != "fake-translate-v2" {
			t.Fatalf("translate request went to %q, want the current model", r.Model)
		}
	}
	tr, _ := e.app.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if snap, ok := model.ParseLLMSnapshot(tr.ConfigSnapshot); !ok || snap.Model != "fake-translate-v2" {
		t.Fatalf("translate snapshot after choosing current: %s", tr.ConfigSnapshot)
	}
	// Upstream snapshots are untouched.
	rp, _ := e.app.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageRepair)
	if snap, _ := model.ParseLLMSnapshot(rp.ConfigSnapshot); snap.Model == "fake-translate-v2" {
		t.Fatal("repair snapshot must not change when translate is rerun")
	}
}

// A7: deleting a lecture whose stage is running needs force; force cancels
// first and removes only the lecture's data directory.
func TestV11ForceDeleteRunningLecture(t *testing.T) {
	e := newE2E(t)
	var c api.Course
	e.postJSON("/api/courses", map[string]string{"name": "Delete Course"}, &c)
	var l api.Lecture
	e.upload("/api/courses/"+c.ID+"/lectures", map[string]string{"title": "Week 9"}, "week9.wav", sampleWAV(e.t, e.cfg.FFmpeg, e.dir), &l)
	e.fake.SetDelay(30 * time.Second)
	defer e.fake.SetDelay(0)
	e.launch(l.ID)
	e.waitFor("summary running", func() bool {
		return stageOf(e.lecture(l.ID), model.StageSummary).Status == model.StatusRunning
	}, l.ID)
	if got := e.lecture(l.ID); got.CanDelete || !contains(got.LectureActions, model.LectureActionDelete) {
		t.Fatalf("running lecture: can_delete %v actions %v", got.CanDelete, got.LectureActions)
	}
	lec, err := e.app.Store.GetLecture(context.Background(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := model.LectureDir(e.cfg.DataDir, lec.CourseID, lec.ID)

	if status, body := e.doRaw(http.MethodDelete, "/api/lectures/"+l.ID, nil); status != http.StatusConflict {
		t.Fatalf("delete without force: HTTP %d %s, want 409", status, body)
	}
	start := time.Now()
	if status, body := e.doRaw(http.MethodDelete, "/api/lectures/"+l.ID+"?force=1", nil); status != http.StatusNoContent {
		t.Fatalf("force delete: HTTP %d %s", status, body)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("force delete took %v", d)
	}
	if status, _ := e.doRaw(http.MethodGet, "/api/lectures/"+l.ID, nil); status != http.StatusNotFound {
		t.Fatalf("deleted lecture: HTTP %d", status)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lecture dir still exists: %v", err)
	}
}

// A11: the course-level citation JSON merges the finished lectures with
// stable refs; without a course code the course part is the name's initials.
func TestV11CourseCitationJSON(t *testing.T) {
	e := newE2E(t)
	courseID, lectureID := e.runPipeline()
	status, body := e.doRaw(http.MethodGet, "/api/courses/"+courseID+"/export/cite.json", nil)
	if status != http.StatusOK {
		t.Fatalf("cite.json: HTTP %d %s", status, body)
	}
	var doc struct {
		Schema   string `json:"schema"`
		Course   struct{ Code *string }
		Lectures []struct {
			Label     string
			Sentences []struct {
				ID  string `json:"id"`
				Ref string `json:"ref"`
				Zh  *string
			}
		}
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	// v2 P13.6: the label is the calendar number; a course without a term
	// start numbers nothing, so the only lecture is "W??-{id prefix}"
	// (schema v3 since lecture kinds; was "L01" in v1.1).
	label := "W??-" + lectureID[:6]
	if doc.Schema != "lectureflow.cite/v3" || doc.Course.Code != nil || len(doc.Lectures) != 1 || doc.Lectures[0].Label != label {
		t.Fatalf("cite.json header: %s", body[:min(len(body), 400)])
	}
	ref := regexp.MustCompile(`^EC ` + regexp.QuoteMeta(label) + `@\d{2}:\d{2}$`)
	id := regexp.MustCompile(`^EC\.` + regexp.QuoteMeta(label) + `\.s\d{4}$`)
	if len(doc.Lectures[0].Sentences) != 14 {
		t.Fatalf("want 14 sentences, got %d", len(doc.Lectures[0].Sentences))
	}
	for _, s := range doc.Lectures[0].Sentences {
		if !ref.MatchString(s.Ref) || !id.MatchString(s.ID) {
			t.Fatalf("sentence ref %q id %q", s.Ref, s.ID)
		}
	}
}
