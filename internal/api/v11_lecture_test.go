// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// fakeStage is a scheduler stage for the API tests: it counts runs and,
// when block is set, runs until it is cancelled (to have a running job).
type fakeStage struct {
	name  string
	block bool
	runs  atomic.Int32
}

func (f *fakeStage) Name() string                 { return f.name }
func (f *fakeStage) Resource() sched.ResourceKind { return sched.ResourceCPU }
func (f *fakeStage) InputHash(context.Context, string) (string, error) {
	return "", nil
}
func (f *fakeStage) Run(ctx context.Context, _ string, progress func(float64)) error {
	f.runs.Add(1)
	if f.block {
		progress(0.5)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func fastScan(stages ...sched.Stage) func(*sched.Options) {
	return func(o *sched.Options) {
		o.ScanInterval = 20 * time.Millisecond
		o.ProgressMinInterval = 20 * time.Millisecond
		o.CancelWait = 3 * time.Second
		o.DeleteWait = 3 * time.Second
		o.Stages = map[string]sched.Stage{}
		for _, s := range stages {
			o.Stages[s.Name()] = s
		}
	}
}

// waitStage polls the lecture until stage reaches status.
func (e *env) waitStage(lectureID, stage, status string) Lecture {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var l Lecture
		e.call("GET", "/api/lectures/"+lectureID, nil, http.StatusOK, &l)
		if stageOf(l.Stages, stage).Status == status {
			return l
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s did not reach %s: %+v", stage, status, stageOf(l.Stages, stage))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// allDone moves every pending stage of a lecture to done.
func (e *env) allDone(lectureID string) {
	e.t.Helper()
	for _, st := range model.LectureStages {
		if j := e.moveJob(model.ScopeLecture, lectureID, st); j.Status == model.StatusPending {
			e.moveJob(model.ScopeLecture, lectureID, st, "queued", "running", "done")
		}
	}
}

func snapshotModel(t *testing.T, j model.Job) string {
	t.Helper()
	s, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		t.Fatalf("%s has no LLM snapshot: %q", j.Stage, j.ConfigSnapshot)
	}
	return s.Model
}

// A3: a draft is never scheduled; launching it starts the pipeline.
func TestDraftRunsOnlyAfterLaunch(t *testing.T) {
	prep := &fakeStage{name: model.StagePrep}
	e := newEnvWith(t, envConfig{sched: fastScan(prep), start: true})
	c := e.course("C")
	l := e.draft(c.ID, "Week 1")
	time.Sleep(200 * time.Millisecond) // ten scans
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if prep.runs.Load() != 0 || stageOf(lv.Stages, "prep").Status != model.StatusPending {
		t.Fatalf("draft was scheduled: runs=%d prep=%+v", prep.runs.Load(), stageOf(lv.Stages, "prep"))
	}
	if w := stageOf(lv.Stages, "prep").WaitingFor; w == nil || *w != model.WaitNotStarted {
		t.Fatalf("draft prep waiting_for = %v", w)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.DraftCount != 1 || !eqStrings(cv.CourseActions, []string{CourseActionLaunchDrafts}) {
		t.Fatalf("course with a draft: %+v", cv)
	}

	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, &lv)
	if lv.Draft || lv.LaunchedAt == nil {
		t.Fatalf("launched lecture %+v", lv)
	}
	for _, a := range lv.LectureActions {
		if a == model.LectureActionLaunch {
			t.Fatalf("launch still offered: %v", lv.LectureActions)
		}
	}
	// A4.1: the snapshots written at launch are summarised per stage.
	set := e.set.Roles()
	tr := *set[model.RoleTranslate].Model
	if m := stageOf(lv.Stages, "translate").Model; m == nil || m.Model != tr || m.ProviderID != "deepseek" ||
		m.Label != "deepseek / "+tr+" · 温度 0.3 · 窗口 3" {
		t.Errorf("translate model %+v", m)
	}
	if m := stageOf(lv.Stages, "asr").Model; m == nil || m.Label != "large-v3 · auto · cuda · en" {
		t.Errorf("asr model %+v", m)
	}
	if m := stageOf(lv.Stages, "prep").Model; m != nil {
		t.Errorf("prep model %+v", m)
	}
	lv = e.waitStage(l.ID, "prep", model.StatusDone)
	if prep.runs.Load() != 1 {
		t.Fatalf("prep ran %d times", prep.runs.Load())
	}
	if msg := e.callErr("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusConflict); !strings.Contains(msg, "已经启动") {
		t.Errorf("second launch: %q", msg)
	}
	e.callErr("POST", "/api/lectures/nope/launch", nil, http.StatusNotFound)
}

// A3: launching needs the lecture roles; the settings error is shown as is.
func TestLaunchNeedsConfiguredRoles(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.draft(c.ID, "L")
	ctx := context.Background()
	// The translate role loses its provider and model.
	if err := e.st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleTranslate, Params: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := e.set.Load(ctx); err != nil {
		t.Fatal(err)
	}
	msg := e.callErr("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusConflict)
	if !strings.Contains(msg, "翻译") {
		t.Fatalf("launch without a translate role: %q", msg)
	}
	msg = e.callErr("POST", "/api/lectures/launch", BatchRequest{IDs: []string{l.ID}}, http.StatusConflict)
	if !strings.Contains(msg, "翻译") {
		t.Fatalf("batch launch without a translate role: %q", msg)
	}
	e.callErr("POST", "/api/courses/"+c.ID+"/launch-drafts", nil, http.StatusConflict)
}

// A4.3: a rerun whose stages' snapshots differ from the current settings
// answers 409 needs_model_choice; "snapshot" keeps, "current" replaces.
func TestRerunNeedsModelChoice(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.allDone(l.ID)
	old := snapshotModel(t, e.moveJob(model.ScopeLecture, l.ID, "translate"))
	var sv Settings
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, &sv)

	path := "/api/lectures/" + l.ID + "/stages/translate/rerun"
	var nm NeedsModelChoice
	e.call("POST", path, nil, http.StatusConflict, &nm)
	if nm.Error.Code != model.ErrNeedsModelChoice || len(nm.Diff) != 1 {
		t.Fatalf("409 body %+v", nm)
	}
	d := nm.Diff[0]
	if d.Stage != "translate" || d.Snapshot.Model != old || d.Current.Model != "deepseek-reasoner" || !d.SnapshotUsable ||
		d.Snapshot.ProviderName != "deepseek" || !strings.Contains(d.Current.Label, "deepseek-reasoner · 温度 0.3 · 窗口 3") ||
		d.LectureID != "" || d.Snapshot.Params == nil {
		t.Fatalf("diff %+v", d)
	}
	// Nothing changed yet.
	if j := e.moveJob(model.ScopeLecture, l.ID, "translate"); j.Status != model.StatusDone {
		t.Fatalf("translate after 409: %s", j.Status)
	}
	e.callErr("POST", path, ModelChoiceRequest{ModelChoice: "maybe"}, http.StatusBadRequest)

	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, http.StatusOK, &lv)
	j := e.moveJob(model.ScopeLecture, l.ID, "translate")
	if j.Status != model.StatusPending || snapshotModel(t, j) != old {
		t.Fatalf("rerun keeping the snapshot: %s %q", j.Status, j.ConfigSnapshot)
	}
	if m := stageOf(lv.Stages, "translate").Model; m == nil || m.Model != old {
		t.Fatalf("view model %+v", m)
	}
	e.allDone(l.ID)
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent}, http.StatusOK, &lv)
	if j := e.moveJob(model.ScopeLecture, l.ID, "translate"); snapshotModel(t, j) != "deepseek-reasoner" {
		t.Fatalf("rerun with the current settings: %q", j.ConfigSnapshot)
	}
	// Retry never asks while the snapshot is usable (A4.2).
	e.moveJob(model.ScopeLecture, l.ID, "translate", "queued", "running", "failed")
	e.call("POST", "/api/lectures/"+l.ID+"/stages/translate/retry", nil, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "translate"); st.Status != model.StatusQueued {
		t.Fatalf("retry: %+v", st)
	}
}

// A2.3 + A4.3: POST /api/lectures/{id}/update with changed settings; the
// 409 body has exactly the shape of NeedsModelChoice (the front end reads
// body.diff instead of showing a toast).
func TestUpdateLectureNeedsModelChoice(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.allDone(l.ID)
	e.moveJob(model.ScopeLecture, l.ID, "summary", "stale")
	e.call("PUT", "/api/settings/roles/repair", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, nil)
	path := "/api/lectures/" + l.ID + "/update"
	res, b := e.do("POST", path, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("update: %d %s", res.StatusCode, b)
	}
	// Exact top-level keys and diff entry keys.
	var raw map[string]json.RawMessage
	// v2 P14.3 (lead ruling 6): {"error": {"code": "needs_model_choice"}, "diff": [...]}.
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 2 || string(raw["error"]) != `{"code":"needs_model_choice"}` {
		t.Fatalf("409 body %s", b)
	}
	var diffs []map[string]json.RawMessage
	if err := json.Unmarshal(raw["diff"], &diffs); err != nil || len(diffs) != 1 {
		t.Fatalf("diff %s", raw["diff"])
	}
	for _, k := range []string{"stage", "snapshot", "current", "snapshot_usable"} {
		if _, ok := diffs[0][k]; !ok {
			t.Errorf("diff entry lacks %q: %s", k, raw["diff"])
		}
	}
	var nm NeedsModelChoice
	decodeStrict(t, b, &nm)
	// summary is the stage rerun; only repair (downstream) differs.
	if nm.Diff[0].Stage != "repair" || nm.Diff[0].Current.Model != "deepseek-reasoner" || !nm.Diff[0].SnapshotUsable {
		t.Fatalf("diff %+v", nm.Diff[0])
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, "summary"); j.Status != model.StatusStale {
		t.Fatalf("409 changed summary to %s", j.Status)
	}
	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent}, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "summary"); st.Status != model.StatusPending {
		t.Fatalf("summary after update %+v", st)
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, "repair"); j.Status != model.StatusPending || snapshotModel(t, j) != "deepseek-reasoner" {
		t.Fatalf("repair after update: %s %q", j.Status, j.ConfigSnapshot)
	}
}

// A4.3: "沿用原模型" is not available when the snapshot's provider is gone.
func TestUnusableSnapshotChoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p2, err := e.llm.ConfigureSettings(ctx, e.set, "Other", model.ProviderOpenAI, "other-model")
	if err != nil {
		t.Fatal(err)
	}
	c := e.course("C")
	l := e.lecture(c.ID, "L") // every LLM stage runs with "Other"
	e.allDone(l.ID)
	if _, err := e.llm.ConfigureSettings(ctx, e.set, "deepseek", model.ProviderOpenAI, "deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	var del ProviderDeleteResult
	e.call("DELETE", "/api/providers/"+p2.ID, nil, http.StatusOK, &del)
	if del.SnapshotRefs != 3 || del.Warning == "" {
		t.Fatalf("delete result %+v", del)
	}
	path := "/api/lectures/" + l.ID + "/stages/translate/rerun"
	var nm NeedsModelChoice
	e.call("POST", path, nil, http.StatusConflict, &nm)
	if len(nm.Diff) != 1 || nm.Diff[0].SnapshotUsable || nm.Diff[0].ReasonInfo == nil ||
		nm.Diff[0].ReasonInfo.Code != settings.CodeSnapshotProviderDeleted || nm.Diff[0].ReasonInfo.Params["provider"] != "Other" ||
		nm.Diff[0].Snapshot.ProviderName != "Other" || nm.Diff[0].Current.ProviderName != "deepseek" {
		t.Fatalf("409 body %+v", nm)
	}
	msg := e.callErr("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, http.StatusConflict)
	if !strings.Contains(msg, "原模型不可用") || strings.Contains(msg, "conflict") {
		t.Fatalf("keeping an unusable snapshot: %q", msg)
	}
	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent}, http.StatusOK, &lv)
	if m := stageOf(lv.Stages, "translate").Model; m == nil || m.ProviderName != "deepseek" {
		t.Fatalf("after current: %+v", m)
	}
	// The view of a stage whose provider is gone shows the recorded name.
	if m := stageOf(lv.Stages, "summary").Model; m == nil || m.ProviderName != "Other" {
		t.Fatalf("summary model %+v", m)
	}
}

// A2: display_status, stale_reason, stale_text, the update action and A2.3 update.
func TestStaleViewsAndUpdate(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	h := termHasher{e.st}
	e.sc.Register(h)
	ctx := context.Background()
	e.allDone(l.ID)
	rep := e.moveJob(model.ScopeLecture, l.ID, "repair")
	hash, _ := h.InputHash(ctx, l.ID)
	if err := e.st.SetJobInputHash(ctx, rep.ID, hash); err != nil {
		t.Fatal(err)
	}
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "cache", "zh": "缓存"}, http.StatusCreated, &term)
	e.srv.WaitInvalidations()
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	st := stageOf(lv.Stages, "repair")
	wantReason := "术语表已更新（现为 v1）"
	if st.Status != model.StatusStale || st.DisplayStatus != model.StatusStale || st.StaleReason == nil || *st.StaleReason != wantReason {
		t.Fatalf("repair %+v (reason %v)", st, st.StaleReason)
	}
	if tr := stageOf(lv.Stages, "translate"); tr.DisplayStatus != model.DisplayOutdated || tr.Status != model.StatusDone || tr.StaleReason != nil {
		t.Fatalf("translate %+v", tr)
	}
	if lv.StaleText == nil || *lv.StaleText != wantReason+"，需要更新：修复" {
		t.Fatalf("stale_text %v", lv.StaleText)
	}
	if !eqStrings(lv.LectureActions, []string{model.LectureActionUpdate, model.LectureActionDelete}) {
		t.Fatalf("lecture_actions %v", lv.LectureActions)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.StaleLectureCount != 1 || !eqStrings(cv.CourseActions, []string{CourseActionUpdateStale, CourseActionExportCite}) {
		t.Fatalf("course %+v", cv)
	}
	// While a downstream stage runs the rerun would be 409: no update button.
	e.moveJob(model.ScopeLecture, l.ID, "export", "stale")
	e.call("POST", "/api/lectures/"+l.ID+"/stages/export/rerun", nil, http.StatusOK, &lv)
	e.moveJob(model.ScopeLecture, l.ID, "export", "queued", "running")
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if !eqStrings(lv.LectureActions, []string{model.LectureActionCancel, model.LectureActionDelete}) || lv.CanDelete {
		t.Fatalf("lecture_actions while export runs %v", lv.LectureActions)
	}
	e.moveJob(model.ScopeLecture, l.ID, "export", "cancelled")

	e.call("POST", "/api/lectures/"+l.ID+"/update", nil, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "repair"); st.Status != model.StatusPending || st.StaleReason != nil {
		t.Fatalf("after update: %+v", st)
	}
	if lv.StaleText != nil {
		t.Fatalf("stale_text after update %v", *lv.StaleText)
	}
	// Nothing stale: update is a no-op that returns the lecture.
	e.call("POST", "/api/lectures/"+l.ID+"/update", nil, http.StatusOK, &lv)
	d := e.draft(c.ID, "Draft")
	e.callErr("POST", "/api/lectures/"+d.ID+"/update", nil, http.StatusConflict)
}

// A7: deleting a running lecture needs force; force cancels it first.
func TestDeleteRunningLecture(t *testing.T) {
	prep := &fakeStage{name: model.StagePrep, block: true}
	e := newEnvWith(t, envConfig{sched: fastScan(prep), start: true})
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	lv := e.waitStage(l.ID, "prep", model.StatusRunning)
	if lv.CanDelete || !eqStrings(lv.LectureActions, []string{model.LectureActionCancel, model.LectureActionDelete}) {
		t.Fatalf("running lecture %+v", lv)
	}
	msg := e.callErr("DELETE", "/api/lectures/"+l.ID, nil, http.StatusConflict)
	if !strings.Contains(msg, "正在运行") {
		t.Fatalf("delete without force: %q", msg)
	}
	e.callErr("DELETE", "/api/lectures/"+l.ID+"?force=maybe", nil, http.StatusBadRequest)
	dir := model.LectureDir(e.cfg.DataDir, c.ID, l.ID)
	e.call("DELETE", "/api/lectures/"+l.ID+"?force=1", nil, http.StatusNoContent, nil)
	e.callErr("GET", "/api/lectures/"+l.ID, nil, http.StatusNotFound)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lecture dir not removed: %v", err)
	}

	// Batch delete with force: per-id results, no lecture objects.
	l2 := e.lecture(c.ID, "L2")
	e.waitStage(l2.ID, "prep", model.StatusRunning)
	var br BatchResponse
	e.call("POST", "/api/lectures/delete", BatchRequest{IDs: []string{l2.ID}}, http.StatusOK, &br)
	if len(br.Results) != 1 || br.Results[0].OK || !strings.Contains(br.Results[0].Error, "正在运行") {
		t.Fatalf("batch delete without force %+v", br)
	}
	e.call("POST", "/api/lectures/delete", BatchRequest{IDs: []string{l2.ID, "nope"}, Force: true}, http.StatusOK, &br)
	if len(br.Results) != 2 || !br.Results[0].OK || br.Results[0].Lecture != nil || br.Results[1].OK || br.Results[1].Error == "" {
		t.Fatalf("batch delete %+v", br)
	}
}

// A3.2 / A7 batch operations report one result per id.
func TestBatchLaunchCancelUpdate(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	a, b := e.draft(c.ID, "A"), e.draft(c.ID, "B")
	var br BatchResponse
	e.callErr("POST", "/api/lectures/launch", BatchRequest{}, http.StatusBadRequest)
	e.call("POST", "/api/lectures/launch", BatchRequest{IDs: []string{a.ID, "nope", a.ID}}, http.StatusOK, &br)
	if len(br.Results) != 3 || !br.Results[0].OK || br.Results[0].Lecture == nil || br.Results[0].Lecture.Draft ||
		br.Results[1].OK || br.Results[1].Error == "" || br.Results[2].OK || !strings.Contains(br.Results[2].Error, "已经启动") {
		t.Fatalf("batch launch %+v", br)
	}
	var op CourseOpResult
	e.call("POST", "/api/courses/"+c.ID+"/launch-drafts", nil, http.StatusOK, &op)
	if len(op.Results) != 1 || op.Results[0].ID != b.ID || !op.Results[0].OK || op.Course.DraftCount != 0 {
		t.Fatalf("launch-drafts %+v", op)
	}

	e.moveJob(model.ScopeLecture, a.ID, "prep", "queued")
	e.call("POST", "/api/lectures/cancel", BatchRequest{IDs: []string{a.ID, b.ID}}, http.StatusOK, &br)
	if len(br.Results) != 2 || !br.Results[0].OK || stageOf(br.Results[0].Lecture.Stages, "prep").Status != model.StatusCancelled ||
		!br.Results[1].OK {
		t.Fatalf("batch cancel %+v", br)
	}

	// Both lectures stale on translate, settings changed: ONE question for both.
	e.call("POST", "/api/lectures/"+a.ID+"/stages/prep/retry", nil, http.StatusOK, nil)
	e.moveJob(model.ScopeLecture, a.ID, "prep", "running", "done")
	for _, l := range []Lecture{a, b} {
		e.allDone(l.ID)
		e.moveJob(model.ScopeLecture, l.ID, "translate", "stale")
	}
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, nil)
	var nm NeedsModelChoice
	e.call("POST", "/api/lectures/update", BatchRequest{IDs: []string{a.ID, b.ID}}, http.StatusConflict, &nm)
	if len(nm.Diff) != 2 || nm.Diff[0].LectureID != a.ID || nm.Diff[0].LectureTitle != "A" || nm.Diff[1].LectureTitle != "B" {
		t.Fatalf("batch needs_model_choice %+v", nm)
	}
	e.call("POST", "/api/courses/"+c.ID+"/update-stale", nil, http.StatusConflict, &nm)
	if len(nm.Diff) != 2 || nm.Diff[1].LectureID != b.ID {
		t.Fatalf("course needs_model_choice %+v", nm)
	}
	e.call("POST", "/api/lectures/update", BatchRequest{IDs: []string{a.ID}, ModelChoice: model.ModelChoiceCurrent}, http.StatusOK, &br)
	if len(br.Results) != 1 || !br.Results[0].OK || stageOf(br.Results[0].Lecture.Stages, "translate").Status != model.StatusPending {
		t.Fatalf("batch update %+v", br)
	}
	if j := e.moveJob(model.ScopeLecture, a.ID, "translate"); snapshotModel(t, j) != "deepseek-reasoner" {
		t.Fatalf("translate snapshot %q", j.ConfigSnapshot)
	}
	e.call("POST", "/api/courses/"+c.ID+"/update-stale", ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, http.StatusOK, &op)
	if len(op.Results) != 1 || op.Results[0].ID != b.ID || !op.Results[0].OK || op.Course.StaleLectureCount != 0 {
		t.Fatalf("update-stale %+v", op)
	}
	if j := e.moveJob(model.ScopeLecture, b.ID, "translate"); snapshotModel(t, j) == "deepseek-reasoner" {
		t.Fatalf("snapshot choice replaced the snapshot: %q", j.ConfigSnapshot)
	}
}

func TestPatchLectureSeqAndTitle(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	a, b := e.draft(c.ID, "A"), e.draft(c.ID, "B")
	var lv Lecture
	seq := func(n int) *int { return &n }
	e.callErr("PATCH", "/api/lectures/"+a.ID, LecturePatch{}, http.StatusBadRequest)
	// v2 P13.2: seq is computed from the recording times and cannot be set.
	for _, n := range []int{0, 2, 7} {
		if info := e.callErrInfo("PATCH", "/api/lectures/"+a.ID, LecturePatch{Seq: seq(n)}, http.StatusBadRequest); info.Code != "lecture.seq_readonly" {
			t.Fatalf("seq %d: %+v", n, info)
		}
	}
	title := "Week 7"
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{Title: &title}, http.StatusOK, &lv)
	if lv.Title != title || lv.TitleSource != model.TitleSourceUser {
		t.Fatalf("after title %+v", lv)
	}
	// The list is ordered by recorded_at (P13.4): A recorded after B.
	e.call("PATCH", "/api/lectures/"+a.ID, LecturePatch{RecordedAt: ptr("2026-10-08T10:00")}, http.StatusOK, nil)
	e.call("PATCH", "/api/lectures/"+b.ID, LecturePatch{RecordedAt: ptr("2026-10-06T10:00")}, http.StatusOK, nil)
	var list []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID || list[0].Seq != 1 || list[1].Seq != 2 {
		t.Fatalf("lectures are not ordered by recorded_at: %s, %s", list[0].Title, list[1].Title)
	}
}

// A3.1: a draft's media can be replaced; the old upload is removed.
func TestReplaceDraftMedia(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.draft(c.ID, "L")
	ctx := context.Background()
	before, _ := e.st.GetLecture(ctx, l.ID)
	body, ctype := multipartBody(t, nil, "better.MKV", []byte("new media bytes"))
	req, _ := http.NewRequest("PUT", e.ts.URL+"/api/lectures/"+l.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replace media: %d", res.StatusCode)
	}
	after, _ := e.st.GetLecture(ctx, l.ID)
	if filepath.Base(after.MediaPath) != "media.mkv" {
		t.Fatalf("media path %q", after.MediaPath)
	}
	// v2 P13.3.1: the replacement's file name is the original file name now.
	if after.OriginalFilename != "better.MKV" {
		t.Fatalf("original_filename %q", after.OriginalFilename)
	}
	if b, err := os.ReadFile(after.MediaPath); err != nil || string(b) != "new media bytes" {
		t.Fatalf("new media: %v %q", err, b)
	}
	if _, err := os.Stat(before.MediaPath); !os.IsNotExist(err) {
		t.Fatalf("old media %s still there: %v", before.MediaPath, err)
	}
	// A media path outside data_dir is never deleted.
	outside := filepath.Join(e.dir, "Lectures", "orig.mp4")
	os.MkdirAll(filepath.Dir(outside), 0o755)
	os.WriteFile(outside, []byte("user's original"), 0o644)
	if err := e.st.SetLectureMedia(ctx, l.ID, outside); err != nil {
		t.Fatal(err)
	}
	body, ctype = multipartBody(t, nil, "again.mp4", []byte("x"))
	req, _ = http.NewRequest("PUT", e.ts.URL+"/api/lectures/"+l.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("second replace: %v %v", err, res)
	}
	res.Body.Close()
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the original outside data_dir was touched: %v", err)
	}
	// Launched lectures keep their media.
	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, nil)
	body, ctype = multipartBody(t, nil, "late.mp4", []byte("x"))
	req, _ = http.NewRequest("PUT", e.ts.URL+"/api/lectures/"+l.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusConflict {
		t.Fatalf("replace after launch: %v %v", err, res)
	}
	res.Body.Close()
}

// writeWAV writes a 16 kHz mono 16-bit silent WAV of the given length.
func writeWAV(t *testing.T, path string, ms int) {
	t.Helper()
	n := 16000 * 2 * ms / 1000
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(16000))
	binary.Write(&b, binary.LittleEndian, uint32(32000))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(n))
	b.Write(make([]byte, n))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWavDuration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.wav")
	writeWAV(t, p, 6000)
	if ms, ok := wavDurationMS(p); !ok || ms != 6000 {
		t.Fatalf("duration %d %v", ms, ok)
	}
	os.WriteFile(p, []byte("not a wav"), 0o644)
	if _, ok := wavDurationMS(p); ok {
		t.Fatal("garbage parsed as WAV")
	}
}

// A13.3: the clip and sentence bounds are in the response headers.
func TestAudioClipHeaders(t *testing.T) {
	e := newEnv(t)
	if _, err := exec.LookPath(e.cfg.FFmpeg); err != nil {
		t.Skip("ffmpeg not found; audio clip test skipped (SPEC §14)")
	}
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	writeWAV(t, filepath.Join(model.LectureDir(e.cfg.DataDir, c.ID, l.ID), model.AudioFile), 6000)
	base := "/api/lectures/" + l.ID + "/audio"
	for _, tc := range []struct {
		q                      string
		start, ss, se, clipEnd string
	}{
		{"?start_ms=2000&end_ms=3000", "1000", "2000", "3000", "4000"},
		{"?start_ms=300&end_ms=900", "0", "300", "900", "1900"},
		{"?start_ms=5500&end_ms=5900", "4500", "5500", "5900", "6000"}, // clipped to the audio length
	} {
		res, b := e.do("GET", base+tc.q, nil)
		if res.StatusCode != http.StatusOK || !bytes.HasPrefix(b, []byte("RIFF")) {
			t.Fatalf("%s: %d", tc.q, res.StatusCode)
		}
		h := res.Header
		if h.Get("X-Clip-Start-Ms") != tc.start || h.Get("X-Clip-Sentence-Start-Ms") != tc.ss ||
			h.Get("X-Clip-Sentence-End-Ms") != tc.se || h.Get("X-Clip-End-Ms") != tc.clipEnd {
			t.Errorf("%s: headers %v", tc.q, h)
		}
	}
	e.callErr("GET", base+"?start_ms=9000&end_ms=9500", nil, http.StatusBadRequest)
}

// The background invalidation merges the requests that arrive during a run
// per cause kind (last of each kind) and runs the glossary first (A2.2).
func TestInvalidatorCauses(t *testing.T) {
	var mu sync.Mutex
	var got []state.StaleCause
	release := make(chan struct{})
	first := true
	iv := newInvalidator(func(_ string, c state.StaleCause) {
		mu.Lock()
		got = append(got, c)
		wait := first
		first = false
		mu.Unlock()
		if wait {
			<-release
		}
	})
	iv.request("C", state.StaleCause{Kind: state.StaleGlossary})
	time.Sleep(20 * time.Millisecond)
	iv.request("C", state.StaleCause{Kind: state.StaleMaterial, Material: "a.pdf"})
	iv.request("C", state.StaleCause{Kind: state.StaleGlossary})
	iv.request("C", state.StaleCause{Kind: state.StaleMaterial, Material: "b.pdf"})
	close(release)
	iv.wait()
	if len(got) != 3 || got[0].Kind != state.StaleGlossary || got[1].Kind != state.StaleGlossary || got[2].Material != "b.pdf" {
		t.Fatalf("runs %+v (want glossary, then the merged glossary, then material b.pdf)", got)
	}
}

// Review operations read the window from the translate snapshot (x = -1).
func TestReviewUsesSnapshotWindow(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	ctx := context.Background()
	var ss []model.Sentence
	for i := 0; i < 9; i++ {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: "s", RepairedText: "s", ZhStatus: model.ZhNone})
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error { return tx.InsertSentences(ctx, l.ID, ss) }); err != nil {
		t.Fatal(err)
	}
	for i := range ss {
		zh, hash := "译", "h"
		if err := e.st.SetTranslation(ctx, l.ID, i, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK, ZhHash: &hash}); err != nil {
			t.Fatal(err)
		}
	}
	// Window 1 in the lecture's translate snapshot (the setting says 3).
	j := e.moveJob(model.ScopeLecture, l.ID, "translate")
	snap, _ := model.ParseLLMSnapshot(j.ConfigSnapshot)
	w := 1
	snap.Params.Window = &w
	if err := e.st.SetJobSnapshot(ctx, j.ID, model.MarshalSnapshot(snap)); err != nil {
		t.Fatal(err)
	}
	var sv Sentence
	e.call("POST", "/api/lectures/"+l.ID+"/sentences/4/edit", map[string]string{"text": "changed"}, http.StatusOK, &sv)
	var page SentencePage
	e.call("GET", "/api/lectures/"+l.ID+"/sentences", nil, http.StatusOK, &page)
	stale := 0
	for _, x := range page.Sentences {
		if x.ZhStatus == model.ZhStale {
			stale++
			if x.Idx < 3 || x.Idx > 5 {
				t.Errorf("sentence %d outside [idx-1, idx+1] marked stale", x.Idx)
			}
		}
	}
	if stale != 3 {
		t.Fatalf("%d stale sentences, want 3 (window 1 from the snapshot)", stale)
	}
}

func decodeStrict(t *testing.T, b []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %T: %v; body %s", v, err, b)
	}
}
