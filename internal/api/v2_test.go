// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// v2 (SPEC-v2 G1, G3, G5.5, G6) HTTP tests: term tiers and locking, the
// glossary janitor's review / report / undo endpoints, the course's Janitor
// status and glossary_review stage, the settings row of the new role and the
// prompt version in the A4.3 diff.

// insertTerm adds a term the way an extractor would (unlocked, not via the
// API, which always creates locked manual terms) and bumps the version.
func (e *env) insertTerm(courseID string, t model.Term) model.Term {
	e.t.Helper()
	ctx := context.Background()
	t.ID, t.CourseID = db.NewID(), courseID
	if t.Source == "" {
		t.Source = model.SourceSyllabus
	}
	t.Enabled = true
	err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertTerm(ctx, t); err != nil {
			return err
		}
		_, err := tx.BumpGlossaryVersion(ctx, courseID)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetTerm(ctx, t.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) termByEn(courseID, en string) Term {
	e.t.Helper()
	var ts []Term
	e.call("GET", "/api/courses/"+courseID+"/terms", nil, http.StatusOK, &ts)
	for _, t := range ts {
		if t.En == en {
			return t
		}
	}
	e.t.Fatalf("term %q not in the glossary", en)
	return Term{}
}

func (e *env) glossaryVersion(courseID string) int {
	e.t.Helper()
	var cv Course
	e.call("GET", "/api/courses/"+courseID, nil, http.StatusOK, &cv)
	return cv.GlossaryVersion
}

// configureJanitor assigns glossary_janitor to the test provider (which has
// a key, so state.JanitorConfigured is true).
func (e *env) configureJanitor() {
	e.t.Helper()
	ps := e.set.Providers()
	if len(ps) == 0 {
		e.t.Fatal("no provider in the test settings")
	}
	if err := e.set.SetRole(context.Background(), model.RoleGlossaryJanitor, ps[0].ID, "fake-opus", model.RoleParams{}); err != nil {
		e.t.Fatal(err)
	}
}

// Lead (core-builder report): a janitor invalidation merged into a running
// one must not be dropped; it runs after the glossary cause.
func TestInvalidatorJanitorCause(t *testing.T) {
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
	// A term edit, then an undo of a janitor change, while the first run is busy.
	iv.request("C", state.StaleCause{Kind: state.StaleJanitor})
	iv.request("C", state.StaleCause{Kind: state.StaleGlossary})
	close(release)
	iv.wait()
	if len(got) != 3 || got[0].Kind != state.StaleGlossary || got[1].Kind != state.StaleGlossary || got[2].Kind != state.StaleJanitor {
		t.Fatalf("runs %+v (want glossary, then the merged glossary, then janitor)", got)
	}
}

// G1, G6: a term the user adds is manual, locked, its zh from the user; the
// list carries every v2 field.
func TestTermV2FieldsAndManualCreate(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "Kasiski examination", "zh": "卡西斯基试验"}, http.StatusCreated, &term)
	if term.Source != model.SourceManual || !term.Locked || term.ZhSource != model.ZhSourceUser || term.Tier != model.TierEnforce || !term.Enabled {
		t.Fatalf("manual term %+v", term)
	}
	e.insertTerm(c.ID, model.Term{En: "key", Zh: "密钥", Tier: model.TierReference, CommonWord: true,
		JanitorNote: "Common word; technical sense only in 'secret key'.", ZhSource: model.ZhSourceJanitor})
	got := e.termByEn(c.ID, "key")
	if got.Tier != model.TierReference || got.Locked || !got.CommonWord || got.ZhSource != model.ZhSourceJanitor ||
		got.JanitorNote != "Common word; technical sense only in 'secret key'." {
		t.Fatalf("listed term %+v", got)
	}
	// An extractor term without v2 values reads as enforce / extractor.
	e.insertTerm(c.ID, model.Term{En: "Feistel network", Zh: "Feistel 网络"})
	if f := e.termByEn(c.ID, "Feistel network"); f.Tier != model.TierEnforce || f.ZhSource != model.ZhSourceExtractor || f.Locked || f.JanitorNote != "" {
		t.Fatalf("defaults %+v", f)
	}
}

// Ruling 8: every user change of zh, tier or enabled locks the term (zh also
// makes zh_source user) and bumps the version; {"locked": false} only unlocks.
func TestTermPatchLocks(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	pub := e.insertTerm(c.ID, model.Term{En: "public", Zh: "公钥", CommonWord: true})
	blk := e.insertTerm(c.ID, model.Term{En: "block", Zh: "分组", CommonWord: true})
	wld := e.insertTerm(c.ID, model.Term{En: "world", Zh: "世界", CommonWord: true, JanitorNote: "kept"})
	v := e.glossaryVersion(c.ID)

	var term Term
	e.call("PATCH", "/api/terms/"+pub.ID, TermPatch{Zh: ptr("公开")}, http.StatusOK, &term)
	if term.Zh != "公开" || !term.Locked || term.ZhSource != model.ZhSourceUser || term.Tier != model.TierEnforce {
		t.Fatalf("after zh %+v", term)
	}
	if got := e.glossaryVersion(c.ID); got != v+1 {
		t.Fatalf("version %d, want %d", got, v+1)
	}

	e.call("PATCH", "/api/terms/"+blk.ID, TermPatch{Tier: ptr(model.TierReference)}, http.StatusOK, &term)
	if term.Tier != model.TierReference || !term.Locked || term.ZhSource != model.ZhSourceExtractor || term.Zh != "分组" {
		t.Fatalf("after tier %+v", term)
	}
	e.call("PATCH", "/api/terms/"+wld.ID, TermPatch{Enabled: ptr(false)}, http.StatusOK, &term)
	if term.Enabled || !term.Locked || term.ZhSource != model.ZhSourceExtractor || term.JanitorNote != "kept" {
		t.Fatalf("after disable %+v", term)
	}
	if got := e.glossaryVersion(c.ID); got != v+3 {
		t.Fatalf("version %d, want %d", got, v+3)
	}

	// The same value again is no change: no version bump.
	e.call("PATCH", "/api/terms/"+blk.ID, TermPatch{Tier: ptr(model.TierReference)}, http.StatusOK, &term)
	if got := e.glossaryVersion(c.ID); got != v+3 {
		t.Fatalf("unchanged tier bumped the version to %d", got)
	}

	// Unlock: nothing else changes, the version stays.
	e.call("PATCH", "/api/terms/"+blk.ID, TermPatch{Locked: ptr(false)}, http.StatusOK, &term)
	if term.Locked || term.Tier != model.TierReference || term.Zh != "分组" || term.ZhSource != model.ZhSourceExtractor {
		t.Fatalf("after unlock %+v", term)
	}
	e.call("PATCH", "/api/terms/"+pub.ID, TermPatch{Locked: ptr(false)}, http.StatusOK, &term)
	if term.Locked || term.ZhSource != model.ZhSourceUser || term.Zh != "公开" {
		t.Fatalf("unlocking keeps the user's zh: %+v", term)
	}
	if got := e.glossaryVersion(c.ID); got != v+3 {
		t.Fatalf("unlock bumped the version to %d", got)
	}

	// Invalid requests.
	e.callErr("PATCH", "/api/terms/"+blk.ID, map[string]any{}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/terms/"+blk.ID, TermPatch{Locked: ptr(true)}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/terms/"+blk.ID, TermPatch{Locked: ptr(false), Zh: ptr("块")}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/terms/"+blk.ID, TermPatch{Tier: ptr("strict")}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/terms/nope", TermPatch{Enabled: ptr(true)}, http.StatusNotFound)
	if got := e.glossaryVersion(c.ID); got != v+3 {
		t.Fatalf("refused requests changed the version to %d", got)
	}
}

// Ruling 5: without a janitor "整理术语表" applies the G2 rule at once and
// answers with the fallback report; a change makes repair stale with the
// janitor reason; the course shows the report as 常用词规则.
func TestGlossaryReviewFallback(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	for _, r := range sv.Roles {
		if r.Role == model.RoleGlossaryJanitor && r.Configured {
			t.Fatal("the test settings must start without a janitor")
		}
	}
	l := e.lecture(c.ID, "L")
	h := termHasher{e.st}
	e.sc.Register(h)
	e.insertTerm(c.ID, model.Term{En: "algorithm", Zh: "算法"})
	e.insertTerm(c.ID, model.Term{En: "algorithms", Zh: "算法"})
	e.allDone(l.ID)
	ctx := context.Background()
	rep := e.moveJob(model.ScopeLecture, l.ID, "repair")
	hash, _ := h.InputHash(ctx, l.ID)
	if err := e.st.SetJobInputHash(ctx, rep.ID, hash); err != nil {
		t.Fatal(err)
	}
	v := e.glossaryVersion(c.ID)

	var res GlossaryReviewResult
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusOK, &res)
	if res.Queued || res.Report == nil || !res.Report.Fallback || res.Report.Version != v+1 || res.Report.CourseID != c.ID {
		t.Fatalf("review result %+v", res)
	}
	var plural *model.JanitorItem
	for i := range res.Report.Items {
		if res.Report.Items[i].En == "algorithms" {
			plural = &res.Report.Items[i]
		}
	}
	if plural == nil || plural.Action != model.JanitorDisable || !plural.Before.Enabled || plural.After.Enabled {
		t.Fatalf("plural item %+v in %+v", plural, res.Report.Items)
	}
	if got := e.termByEn(c.ID, "algorithms"); got.Enabled || got.Locked {
		t.Fatalf("plural after the fallback %+v", got)
	}

	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	j := cv.Janitor
	if j.Configured || j.Running || !j.LastFallback || j.LastModel != "常用词规则" || j.LastVersion != v+1 ||
		j.LastChanges != len(res.Report.Items) || j.LastAt == "" {
		t.Fatalf("janitor status %+v", j)
	}
	if cv.GlossaryVersion != v+1 {
		t.Fatalf("version %d, want %d", cv.GlossaryVersion, v+1)
	}
	e.srv.WaitInvalidations()
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "repair"); st.Status != model.StatusStale || st.StaleReason == nil || !strings.HasPrefix(*st.StaleReason, "术语表已整理") {
		t.Fatalf("repair after the fallback %+v (reason %v)", st, st.StaleReason)
	}

	// The report endpoints.
	var got model.JanitorReport
	e.call("GET", "/api/courses/"+c.ID+"/glossary/report", nil, http.StatusOK, &got)
	if got.Version != v+1 || !got.Fallback || len(got.Items) != len(res.Report.Items) {
		t.Fatalf("latest report %+v", got)
	}
	e.call("GET", "/api/courses/"+c.ID+"/glossary/report?version="+itoa(v+1), nil, http.StatusOK, &got)
	if got.Version != v+1 {
		t.Fatalf("report by version %+v", got)
	}
	e.callErr("GET", "/api/courses/"+c.ID+"/glossary/report?version=99", nil, http.StatusNotFound)
	e.callErr("GET", "/api/courses/"+c.ID+"/glossary/report?version=x", nil, http.StatusBadRequest)
	e.callErr("GET", "/api/courses/"+c.ID+"/glossary/report?run=first", nil, http.StatusBadRequest)
	e.callErr("GET", "/api/courses/"+c.ID+"/glossary/report?run=last&version=1", nil, http.StatusBadRequest)
	e.callErr("POST", "/api/courses/nope/glossary/review", nil, http.StatusNotFound)

	// A second run changes nothing: it is the latest run (Last*), while the
	// change list and LastChanges stay those of the report with changes.
	var again GlossaryReviewResult
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusOK, &again)
	if again.Report == nil || len(again.Report.Items) != 0 || again.Report.Version != v+1 {
		t.Fatalf("second run %+v", again.Report)
	}
	var last model.JanitorReport
	e.call("GET", "/api/courses/"+c.ID+"/glossary/report?run=last", nil, http.StatusOK, &last)
	if len(last.Items) != 0 || last.CreatedAt != again.Report.CreatedAt {
		t.Fatalf("last run %+v", last)
	}
	e.call("GET", "/api/courses/"+c.ID+"/glossary/report", nil, http.StatusOK, &got)
	if len(got.Items) != len(res.Report.Items) || got.Version != v+1 {
		t.Fatalf("latest report with changes %+v", got)
	}
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if cv.Janitor.LastAt != again.Report.CreatedAt || cv.Janitor.LastChanges != len(res.Report.Items) || cv.Janitor.LastModel != "常用词规则" {
		t.Fatalf("janitor after a run without changes %+v", cv.Janitor)
	}
}

// G3.5: undo restores one item and locks the term (version + 1, janitor
// stale reason); a second undo is 409; unknown items and courses without a
// report are 404.
func TestGlossaryReportUndo(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	empty := e.course("Empty")
	e.callErr("GET", "/api/courses/"+empty.ID+"/glossary/report", nil, http.StatusNotFound)
	e.callErr("GET", "/api/courses/"+empty.ID+"/glossary/report?run=last", nil, http.StatusNotFound)

	e.insertTerm(c.ID, model.Term{En: "algorithm", Zh: "算法"})
	e.insertTerm(c.ID, model.Term{En: "algorithms", Zh: "算法"})
	var res GlossaryReviewResult
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusOK, &res)
	if res.Report == nil {
		t.Fatalf("no report: %+v", res)
	}
	plural := e.termByEn(c.ID, "algorithms")
	v := e.glossaryVersion(c.ID)

	var rep model.JanitorReport
	e.call("POST", "/api/courses/"+c.ID+"/glossary/report/undo", JanitorUndoInput{Version: res.Report.Version, TermID: plural.ID}, http.StatusOK, &rep)
	undone := false
	for _, it := range rep.Items {
		if it.TermID == plural.ID {
			undone = it.Undone && it.UndoneAt != ""
		}
	}
	if !undone {
		t.Fatalf("item not marked undone: %+v", rep.Items)
	}
	if got := e.termByEn(c.ID, "algorithms"); !got.Enabled || !got.Locked {
		t.Fatalf("term after undo %+v", got)
	}
	if got := e.glossaryVersion(c.ID); got != v+1 {
		t.Fatalf("version after undo %d, want %d", got, v+1)
	}
	e.callErr("POST", "/api/courses/"+c.ID+"/glossary/report/undo", JanitorUndoInput{Version: res.Report.Version, TermID: plural.ID}, http.StatusConflict)
	e.callErr("POST", "/api/courses/"+c.ID+"/glossary/report/undo", JanitorUndoInput{Version: res.Report.Version, TermID: "nope"}, http.StatusNotFound)
	e.callErr("POST", "/api/courses/"+c.ID+"/glossary/report/undo", JanitorUndoInput{Version: 99, TermID: plural.ID}, http.StatusNotFound)
	e.callErr("POST", "/api/courses/"+c.ID+"/glossary/report/undo", JanitorUndoInput{}, http.StatusBadRequest)
	e.callErr("POST", "/api/courses/nope/glossary/report/undo", JanitorUndoInput{Version: 1, TermID: plural.ID}, http.StatusNotFound)
}

// With a janitor the review is queued (202); the course lists the
// glossary_review stage with its actions, and the course stage endpoint
// retries it.
func TestGlossaryReviewQueued(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	e.configureJanitor()
	var res GlossaryReviewResult
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusAccepted, &res)
	if !res.Queued || res.Report != nil {
		t.Fatalf("review result %+v", res)
	}
	var cv Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	st := stageOf(cv.Stages, model.StageGlossaryReview)
	if !cv.Janitor.Configured || st.Stage != model.StageGlossaryReview || st.Status != model.StatusPending || !cv.GlossaryPending {
		t.Fatalf("course %+v / stage %+v", cv.Janitor, st)
	}
	// Triggers merge while it waits.
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusAccepted, &res)

	e.moveJob(model.ScopeCourse, c.ID, model.StageGlossaryReview, "queued", "running")
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if st := stageOf(cv.Stages, model.StageGlossaryReview); !cv.Janitor.Running || !eqStrings(st.AllowedActions, []string{model.ActionCancel}) {
		t.Fatalf("running: %+v / %+v", cv.Janitor, st)
	}
	e.moveJob(model.ScopeCourse, c.ID, model.StageGlossaryReview, "failed")
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &cv)
	if st := stageOf(cv.Stages, model.StageGlossaryReview); cv.Janitor.Running || cv.GlossaryPending || !eqStrings(st.AllowedActions, []string{model.ActionRetry}) {
		t.Fatalf("failed: %+v / %+v", cv.Janitor, st)
	}
	e.call("POST", "/api/courses/"+c.ID+"/stages/glossary_review/retry", nil, http.StatusOK, &cv)
	if st := stageOf(cv.Stages, model.StageGlossaryReview); st.Status == model.StatusFailed || st.Status == "" {
		t.Fatalf("after retry %+v", st)
	}
	e.callErr("POST", "/api/courses/"+c.ID+"/stages/glossary_janitor/retry", nil, http.StatusBadRequest)
}

// SSE: the end of a glossary_review run tells the page to reload the
// glossary and the course.
func TestJanitorEndAnnouncesTerms(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	e.configureJanitor()
	e.call("POST", "/api/courses/"+c.ID+"/glossary/review", nil, http.StatusAccepted, nil)
	sse := openSSE(t, e.ts.URL+"/api/events")
	waitClients(t, e.hub, 1)
	j := e.moveJob(model.ScopeCourse, c.ID, model.StageGlossaryReview, "queued", "running", "done")
	e.hub.Job(j)
	sse.next(2*time.Second, func(f sseFrame) bool {
		var ev ChangedEvent
		return f.event == "changed" && json.Unmarshal([]byte(f.data), &ev) == nil && ev.Kind == "terms" && ev.ID == c.ID
	})
}

// The settings page lists glossary_janitor with its hint (G3.1).
func TestSettingsJanitorRole(t *testing.T) {
	e := newEnv(t)
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	var row *RoleView
	for i := range sv.Roles {
		if sv.Roles[i].Role == model.RoleGlossaryJanitor {
			row = &sv.Roles[i]
		}
	}
	if row == nil || row.Hint != settings.RoleHint(model.RoleGlossaryJanitor) || row.Hint == "" || row.Default {
		t.Fatalf("janitor row %+v", row)
	}
	for _, r := range sv.Roles {
		if r.Role != model.RoleGlossaryJanitor && r.Hint != settings.RoleHint(r.Role) {
			t.Fatalf("hint of %s: %q", r.Role, r.Hint)
		}
	}
}

// G5.5: an old snapshot (no prompt_version) differs from the current
// settings; the 409 shows "提示词 v1 → v2" through prompt_version.
func TestNeedsModelChoicePromptVersion(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.allDone(l.ID)
	j := e.moveJob(model.ScopeLecture, l.ID, "translate")
	snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		t.Fatalf("no snapshot %q", j.ConfigSnapshot)
	}
	snap.PromptVersion = 0 // written before v2
	if err := e.st.SetJobSnapshot(context.Background(), j.ID, model.MarshalSnapshot(snap)); err != nil {
		t.Fatal(err)
	}
	var nm NeedsModelChoice
	e.call("POST", "/api/lectures/"+l.ID+"/stages/translate/rerun", nil, http.StatusConflict, &nm)
	if len(nm.Diff) != 1 {
		t.Fatalf("409 %+v", nm)
	}
	d := nm.Diff[0]
	if d.Stage != "translate" || d.Snapshot.PromptVersion != 1 || d.Current.PromptVersion != model.PromptVersionTranslate || d.Snapshot.Model != d.Current.Model {
		t.Fatalf("diff %+v", d)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The v2 example files under testdata/api decode into the frozen types
// without unknown fields (like TestExampleFilesMatchTypes).
func TestV2ExampleFiles(t *testing.T) {
	cases := map[string]any{
		"v2_janitor_reports.json":   &[]model.JanitorReport{},
		"v2_janitor_last_runs.json": &[]model.JanitorReport{},
	}
	for name, v := range cases {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
