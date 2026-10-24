// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

const testX = 2

// reviewLecture: 10 translated sentences (zh_hash = real ctxbuild hashes),
// all stages done, a pending decide patch on 5 and 0, an auto patch on 2,
// glossary (one disabled term) and summary.
type reviewEnv struct {
	*fixture
	l     model.Lecture
	p5    model.Patch // decide, pending, sentence 5
	p0    model.Patch // decide, pending, sentence 0
	pAuto model.Patch // auto, review none, sentence 2
}

func newReviewEnv(t *testing.T) *reviewEnv {
	t.Helper()
	f := newFixture(t)
	ctx := f.ctx
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "cache", Zh: "缓存", Source: model.SourceManual, Enabled: true}))
	must(f.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: f.course.ID, En: "TLB", Source: model.SourceMaterial, Enabled: false}))
	l := f.lecture()
	for _, s := range model.LectureStages {
		f.done(model.ScopeLecture, l.ID, s)
	}
	var ss []model.Sentence
	for i := 0; i < 10; i++ {
		asr := fmt.Sprintf("sentence number %d about the cash", i)
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i+1) * 1000, ASRText: asr, RepairedText: asr})
	}
	must(f.st.InsertSentences(ctx, l.ID, ss))
	must(f.st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "A lecture about caches.", InputHash: "h"}))
	e := &reviewEnv{fixture: f, l: l}
	mk := func(idx int, after, class, review string) model.Patch {
		p := model.Patch{ID: db.NewID(), LectureID: l.ID, SentenceIdx: idx, Before: ss[idx].ASRText, After: after, Class: class, Review: review}
		must(f.st.InsertPatches(ctx, []model.Patch{p}))
		must(f.st.SetRepairedText(ctx, l.ID, idx, after)) // patches are applied automatically
		return p
	}
	e.p5 = mk(5, "sentence number 5 about the cache", model.ClassDecide, model.ReviewPending)
	e.p0 = mk(0, "sentence number 0 about the cache", model.ClassDecide, model.ReviewPending)
	e.pAuto = mk(2, "sentence number 2 about the cache", model.ClassAuto, model.ReviewNone)
	must(f.st.RecomputeText(ctx, l.ID, nil))
	e.translateAll()
	return e
}

// translateAll simulates a translate run: every sentence gets zh, ok and
// the hash computed from the current snapshot (as the translate stage does).
func (e *reviewEnv) translateAll() {
	e.t.Helper()
	snap, err := LoadSnapshot(e.ctx, e.st, e.l.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	for i, h := range ctxbuild.TranslateHashes(snap, testX) {
		h := h
		if err := e.st.SetTranslation(e.ctx, e.l.ID, i, db.SentenceTr{Zh: sp(fmt.Sprintf("译文 %d", i)), ZhStatus: model.ZhOK, ZhHash: &h}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *reviewEnv) stale() []int {
	e.t.Helper()
	ss, err := e.st.ListSentences(e.ctx, e.l.ID, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []int{}
	for _, s := range ss {
		if s.ZhStatus == model.ZhStale {
			out = append(out, s.Idx)
		}
	}
	return out
}

func (e *reviewEnv) sentence(idx int) model.Sentence {
	e.t.Helper()
	s, err := e.st.GetSentence(e.ctx, e.l.ID, idx)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func rng(lo, hi int) []int {
	out := []int{}
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

func TestReviewAccept(t *testing.T) {
	e := newReviewEnv(t)
	p, n, err := ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionAccept, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Review != model.ReviewAccepted || p.EditedText != nil {
		t.Fatalf("patch = %+v", p)
	}
	s := e.sentence(5)
	if s.HumanText == nil || *s.HumanText != e.p5.After || s.Text != e.p5.After {
		t.Fatalf("sentence = %+v", s)
	}
	// The decide patch was already applied: text unchanged → no hash changes.
	if n != 0 || len(e.stale()) != 0 {
		t.Fatalf("accept of an applied patch retranslated %d (%v)", n, e.stale())
	}
}

func TestReviewRejectInvalidatesExactlyTheWindow(t *testing.T) {
	e := newReviewEnv(t)
	p, n, err := ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Review != model.ReviewRejected {
		t.Fatalf("patch = %+v", p)
	}
	s := e.sentence(5)
	if s.HumanText == nil || *s.HumanText != s.ASRText || s.Text != s.ASRText || s.RepairedText != e.p5.After {
		t.Fatalf("sentence = %+v", s)
	}
	if want := rng(5-testX, 5+testX); n != len(want) || !reflect.DeepEqual(e.stale(), want) {
		t.Fatalf("stale = %v (n=%d), want exactly %v", e.stale(), n, want)
	}
	// zh is kept while stale (the UI shows "重翻中").
	if s.Zh == nil {
		t.Fatal("zh must be kept")
	}
}

func TestReviewEditAndWindowEdges(t *testing.T) {
	e := newReviewEnv(t)
	p, n, err := ReviewPatch(e.ctx, e.st, testX, e.p0.ID, model.ReviewActionEdit, "  my own version  ")
	if err != nil {
		t.Fatal(err)
	}
	if p.Review != model.ReviewEdited || p.EditedText == nil || *p.EditedText != "my own version" {
		t.Fatalf("patch = %+v", p)
	}
	if s := e.sentence(0); s.HumanText == nil || *s.HumanText != "my own version" || s.Text != "my own version" {
		t.Fatalf("sentence = %+v", s)
	}
	if want := rng(0, testX); n != len(want) || !reflect.DeepEqual(e.stale(), want) {
		t.Fatalf("idx 0: stale = %v n=%d, want %v", e.stale(), n, want)
	}
	// Last sentence: window [n-1-x, n-1].
	e.translateAll()
	_, n, err = EditSentence(e.ctx, e.st, testX, e.l.ID, 9, "the very end")
	if err != nil {
		t.Fatal(err)
	}
	if want := rng(9-testX, 9); n != len(want) || !reflect.DeepEqual(e.stale(), want) {
		t.Fatalf("idx 9: stale = %v n=%d, want %v", e.stale(), n, want)
	}
}

func TestReviewResetAfterRetranslation(t *testing.T) {
	e := newReviewEnv(t)
	if _, _, err := ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, ""); err != nil {
		t.Fatal(err)
	}
	e.translateAll() // the incremental run translated the rejected text
	p, n, err := ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionReset, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Review != model.ReviewPending || p.EditedText != nil {
		t.Fatalf("patch = %+v", p)
	}
	s := e.sentence(5)
	if s.HumanText != nil || s.Text != e.p5.After {
		t.Fatalf("reset must clear human_text: %+v", s)
	}
	if want := rng(3, 7); n != 5 || !reflect.DeepEqual(e.stale(), want) {
		t.Fatalf("stale = %v n=%d", e.stale(), n)
	}
	// Already-stale sentences are not counted again.
	if _, n, _ := ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, ""); n != 0 {
		t.Fatalf("re-reject counted %d already-stale sentences", n)
	}
}

func TestReviewStateErrors(t *testing.T) {
	e := newReviewEnv(t)
	ctx := e.ctx
	// Only pending patches can be accepted/rejected/edited.
	for _, a := range []string{model.ReviewActionAccept, model.ReviewActionReject, model.ReviewActionEdit} {
		if _, _, err := ReviewPatch(ctx, e.st, testX, e.pAuto.ID, a, "x"); !errors.Is(err, ErrConflict) {
			t.Errorf("%s on review=none: %v", a, err)
		}
	}
	// reset only for reviewed patches.
	if _, _, err := ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionReset, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset of pending: %v", err)
	}
	if _, _, err := ReviewPatch(ctx, e.st, testX, e.pAuto.ID, model.ReviewActionReset, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset of none: %v", err)
	}
	ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionAccept, "")
	if _, _, err := ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject of accepted: %v", err)
	}
	// Empty edit text → 400; unknown action → 400; unknown ids → 404.
	for _, txt := range []string{"", "   \n\t "} {
		if _, _, err := ReviewPatch(ctx, e.st, testX, e.p0.ID, model.ReviewActionEdit, txt); !errors.Is(err, ErrBadInput) {
			t.Fatalf("empty edit: %v", err)
		}
		if _, _, err := EditSentence(ctx, e.st, testX, e.l.ID, 3, txt); !errors.Is(err, ErrBadInput) {
			t.Fatalf("empty sentence edit: %v", err)
		}
	}
	if _, _, err := ReviewPatch(ctx, e.st, testX, e.p0.ID, "approve", ""); !errors.Is(err, ErrBadInput) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, _, err := ReviewPatch(ctx, e.st, testX, "nope", model.ReviewActionAccept, ""); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown patch: %v", err)
	}
	if _, _, err := EditSentence(ctx, e.st, testX, e.l.ID, 99, "x"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown sentence: %v", err)
	}
	if _, err := RetranslateSentence(ctx, e.st, e.l.ID, 99); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown sentence retranslate: %v", err)
	}
	// Nothing above changed the database state of p0/p5 wrongly.
	p0, _ := e.st.GetPatch(ctx, e.p0.ID)
	if p0.Review != model.ReviewPending {
		t.Fatalf("p0 = %+v", p0)
	}
}

func TestReviewBusyWhileRepairOrTranslateRuns(t *testing.T) {
	e := newReviewEnv(t)
	ctx := e.ctx
	// translate running (an incremental run).
	if started, err := StartIncrementalTranslate(ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatal(started, err)
	}
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning)
	checks := map[string]func() error{
		"review": func() error {
			_, _, err := ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, "")
			return err
		},
		"edit":        func() error { _, _, err := EditSentence(ctx, e.st, testX, e.l.ID, 3, "x"); return err },
		"reset":       func() error { _, _, err := ResetSentence(ctx, e.st, testX, e.l.ID, 3); return err },
		"retranslate": func() error { _, err := RetranslateSentence(ctx, e.st, e.l.ID, 3); return err },
	}
	for name, fn := range checks {
		if err := fn(); !errors.Is(err, ErrBusy) || !errors.Is(err, ErrConflict) {
			t.Errorf("%s while translate runs: %v", name, err)
		}
	}
	if len(e.stale()) != 0 || e.sentence(3).HumanText != nil {
		t.Fatal("refused operations must not change anything")
	}
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusDone)
	// repair running.
	if err := Rerun(ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, model.StageRepair); err != nil {
		t.Fatal(err)
	}
	e.drive(model.ScopeLecture, e.l.ID, model.StageRepair, model.StatusQueued, model.StatusRunning)
	if _, _, err := EditSentence(ctx, e.st, testX, e.l.ID, 3, "x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("edit while repair runs: %v", err)
	}
}

func TestEditResetRetranslateSentence(t *testing.T) {
	e := newReviewEnv(t)
	ctx := e.ctx
	// Edit a sentence with a pending patch → the patch becomes edited.
	s, n, err := EditSentence(ctx, e.st, testX, e.l.ID, 5, " typed by hand ")
	if err != nil {
		t.Fatal(err)
	}
	if s.HumanText == nil || *s.HumanText != "typed by hand" || s.Text != "typed by hand" || n != 5 {
		t.Fatalf("edit: %+v n=%d", s, n)
	}
	p, _ := e.st.GetPatch(ctx, e.p5.ID)
	if p.Review != model.ReviewEdited || p.EditedText == nil || *p.EditedText != "typed by hand" {
		t.Fatalf("pending patch after sentence edit: %+v", p)
	}
	// Edit a sentence without a patch.
	e.translateAll()
	if _, n, err := EditSentence(ctx, e.st, testX, e.l.ID, 8, "no patch here"); err != nil || n != 4 { // window [6, 9]
		t.Fatalf("edit without patch: n=%d err=%v stale=%v", n, err, e.stale())
	}
	// Editing to the current text changes no hash → nothing stale.
	e.translateAll()
	if _, n, err := EditSentence(ctx, e.st, testX, e.l.ID, 8, "no patch here"); err != nil || n != 0 || len(e.stale()) != 0 {
		t.Fatalf("same-text edit: n=%d err=%v stale=%v", n, err, e.stale())
	}
	// Edit of a sentence whose patch is already reviewed leaves the patch alone.
	ReviewPatch(ctx, e.st, testX, e.p0.ID, model.ReviewActionAccept, "")
	EditSentence(ctx, e.st, testX, e.l.ID, 0, "zero")
	if p0, _ := e.st.GetPatch(ctx, e.p0.ID); p0.Review != model.ReviewAccepted {
		t.Fatalf("reviewed patch changed by sentence edit: %+v", p0)
	}

	// Reset: human_text cleared, the reviewed patch goes back to pending.
	e.translateAll()
	s, n, err = ResetSentence(ctx, e.st, testX, e.l.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if s.HumanText != nil || s.Text != e.p5.After || n != 5 {
		t.Fatalf("reset: %+v n=%d", s, n)
	}
	if p, _ := e.st.GetPatch(ctx, e.p5.ID); p.Review != model.ReviewPending || p.EditedText != nil {
		t.Fatalf("patch after sentence reset: %+v", p)
	}
	// Reset of a sentence without human_text is a no-op.
	e.translateAll()
	if _, n, err := ResetSentence(ctx, e.st, testX, e.l.ID, 4); err != nil || n != 0 {
		t.Fatalf("no-op reset: n=%d err=%v", n, err)
	}

	// Retranslate marks exactly this one sentence.
	e.translateAll()
	s, err = RetranslateSentence(ctx, e.st, e.l.ID, 6)
	if err != nil || s.ZhStatus != model.ZhStale || !reflect.DeepEqual(e.stale(), []int{6}) {
		t.Fatalf("retranslate: %+v err=%v stale=%v", s, err, e.stale())
	}
}

func TestUntranslatedSentencesStayNone(t *testing.T) {
	e := newReviewEnv(t)
	ctx := e.ctx
	// Sentence 4 was never translated.
	if err := e.st.SetTranslation(ctx, e.l.ID, 4, db.SentenceTr{ZhStatus: model.ZhNone}); err != nil {
		t.Fatal(err)
	}
	_, n, err := ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{3, 5, 6, 7}; n != 4 || !reflect.DeepEqual(e.stale(), want) {
		t.Fatalf("stale = %v n=%d, want %v", e.stale(), n, want)
	}
	if s := e.sentence(4); s.ZhStatus != model.ZhNone {
		t.Fatalf("untranslated sentence = %s", s.ZhStatus)
	}
	if s, err := RetranslateSentence(ctx, e.st, e.l.ID, 4); err != nil || s.ZhStatus != model.ZhNone {
		t.Fatalf("retranslate untranslated: %+v %v", s, err)
	}
	// qa_failed sentences whose hash changes become stale too.
	e.translateAll()
	e.st.SetTranslation(ctx, e.l.ID, 1, db.SentenceTr{Zh: sp("坏译文"), ZhStatus: model.ZhQAFailed, ZhHash: e.sentence(1).ZhHash, QAError: sp("numbers")})
	if _, n, _ := EditSentence(ctx, e.st, testX, e.l.ID, 0, "zero"); n != 3 || !reflect.DeepEqual(e.stale(), []int{0, 1, 2}) {
		t.Fatalf("qa_failed in window: n=%d stale=%v", n, e.stale())
	}
}

func TestReviewNeverTouchesTheGlossary(t *testing.T) {
	e := newReviewEnv(t)
	ctx := e.ctx
	before, _ := e.st.ListTerms(ctx, e.course.ID)
	c0, _ := e.st.GetCourse(ctx, e.course.ID)
	ReviewPatch(ctx, e.st, testX, e.p5.ID, model.ReviewActionEdit, "a brand new term here")
	ReviewPatch(ctx, e.st, testX, e.p0.ID, model.ReviewActionReject, "")
	EditSentence(ctx, e.st, testX, e.l.ID, 3, "something")
	after, _ := e.st.ListTerms(ctx, e.course.ID)
	c1, _ := e.st.GetCourse(ctx, e.course.ID)
	if !reflect.DeepEqual(before, after) || c0.GlossaryVersion != c1.GlossaryVersion {
		t.Fatal("review operations must not modify the glossary")
	}
}

func TestIncrementalTranslateJobIsMarked(t *testing.T) {
	e := newReviewEnv(t)
	ReviewPatch(e.ctx, e.st, testX, e.p5.ID, model.ReviewActionReject, "")
	if started, err := StartIncrementalTranslate(e.ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatal(started, err)
	}
	// The translate job that completed had no input_hash → plain marker.
	tr := e.lj(e.l.ID, model.StageTranslate)
	if tr.Stats.Note != NoteIncremental || tr.Status != model.StatusPending || !IsIncremental(tr.Stats.Note) {
		t.Fatalf("incremental translate job = %+v", tr)
	}
	if h, ok := IncrementalBaseHash(tr.Stats.Note); ok || h != "" {
		t.Fatalf("plain note has no base hash: %q %v", h, ok)
	}
	// With the previous round's input_hash: "incremental:<hash>".
	e.done(model.ScopeLecture, e.l.ID, model.StageTranslate)
	e.done(model.ScopeLecture, e.l.ID, model.StageExport)
	e.setHash(model.ScopeLecture, e.l.ID, model.StageTranslate, "prefix-hash-1")
	if started, err := StartIncrementalTranslate(e.ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatal(started, err)
	}
	tr = e.lj(e.l.ID, model.StageTranslate)
	if tr.Stats.Note != "incremental:prefix-hash-1" || !IsIncremental(tr.Stats.Note) {
		t.Fatalf("note = %q", tr.Stats.Note)
	}
	if h, ok := IncrementalBaseHash(tr.Stats.Note); !ok || h != "prefix-hash-1" {
		t.Fatalf("IncrementalBaseHash = %q %v", h, ok)
	}
	if ex := e.lj(e.l.ID, model.StageExport); ex.Stats.Note != "" {
		t.Fatalf("export job note = %q", ex.Stats.Note)
	}
	// A regular rerun is not incremental.
	e.done(model.ScopeLecture, e.l.ID, model.StageTranslate)
	e.done(model.ScopeLecture, e.l.ID, model.StageExport)
	if err := Rerun(e.ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, model.StageTranslate); err != nil {
		t.Fatal(err)
	}
	if tr := e.lj(e.l.ID, model.StageTranslate); tr.Stats.Note != "" {
		t.Fatalf("rerun translate note = %q", tr.Stats.Note)
	}
}

func TestLoadSnapshot(t *testing.T) {
	e := newReviewEnv(t)
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p", PagesMD: []string{"page"}}
	e.st.CreateMaterial(e.ctx, m)
	snap, err := LoadSnapshot(context.Background(), e.st, e.l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sentences) != 10 || len(snap.Terms) != 2 || snap.Summary != "A lecture about caches." || len(snap.Materials) != 1 {
		t.Fatalf("snapshot = %d sentences, %d terms, summary %q, %d materials", len(snap.Sentences), len(snap.Terms), snap.Summary, len(snap.Materials))
	}
	if snap.Sentences[5].Text != e.p5.After || snap.Sentences[5].ASRText == snap.Sentences[5].Text {
		t.Fatalf("sentence 5 = %+v", snap.Sentences[5])
	}
}

func TestIncrementalNoteParsing(t *testing.T) {
	cases := []struct {
		note string
		inc  bool
		hash string
		ok   bool
	}{
		{"incremental:abc123", true, "abc123", true},
		{"incremental", true, "", false},
		{"incremental:", true, "", false},
		{"", false, "", false},
		{"chunk 2 failed", false, "", false},
		{"incrementalX:abc", false, "", false},
	}
	for _, c := range cases {
		if IsIncremental(c.note) != c.inc {
			t.Errorf("IsIncremental(%q) = %v", c.note, !c.inc)
		}
		if h, ok := IncrementalBaseHash(c.note); h != c.hash || ok != c.ok {
			t.Errorf("IncrementalBaseHash(%q) = %q, %v", c.note, h, ok)
		}
	}
}

// translateAllX is translateAll with the hashes of window x (what a
// translate run with that window writes).
func (e *reviewEnv) translateAllX(x int) {
	e.t.Helper()
	snap, err := LoadSnapshot(e.ctx, e.st, e.l.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	for i, h := range ctxbuild.TranslateHashes(snap, x) {
		h := h
		if err := e.st.SetTranslation(e.ctx, e.l.ID, i, db.SentenceTr{Zh: sp(fmt.Sprintf("译文 %d", i)), ZhStatus: model.ZhOK, ZhHash: &h}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// x < 0: the window is the one of the lecture's translate snapshot (lead
// decision 8), read inside the review transaction.
func TestReviewUsesSnapshotWindowWhenXNegative(t *testing.T) {
	e := newReviewEnv(t)
	w := 1
	temp := 0.3
	snap := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "p", ProviderName: "P", Model: "m",
		Params: model.RoleParams{Temperature: &temp, Window: &w}})
	if err := e.st.SetJobSnapshot(e.ctx, e.lj(e.l.ID, model.StageTranslate).ID, snap); err != nil {
		t.Fatal(err)
	}
	e.translateAllX(1)
	_, n, err := ReviewPatch(e.ctx, e.st, -1, e.p5.ID, model.ReviewActionReject, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.stale(); !reflect.DeepEqual(got, rng(4, 6)) || n != 3 {
		t.Fatalf("window 1: stale = %v (n=%d), want [4 5 6]", got, n)
	}
	// EditSentence and ResetSentence follow the same rule.
	e.translateAllX(1)
	if _, n, err := EditSentence(e.ctx, e.st, -1, e.l.ID, 8, "sentence number 8 about the cache"); err != nil || n != 3 {
		t.Fatalf("edit: %d %v", n, err)
	}
	if got := e.stale(); !reflect.DeepEqual(got, rng(7, 9)) {
		t.Fatalf("edit window 1: stale = %v", got)
	}
	e.translateAllX(1)
	if _, n, err := ResetSentence(e.ctx, e.st, -1, e.l.ID, 8); err != nil || n != 3 {
		t.Fatalf("reset: %d %v", n, err)
	}
	// No snapshot: the default window.
	if err := e.st.SetJobSnapshot(e.ctx, e.lj(e.l.ID, model.StageTranslate).ID, model.EmptySnapshot); err != nil {
		t.Fatal(err)
	}
	e.translateAllX(model.DefaultTranslateWindow)
	if _, n, err := EditSentence(e.ctx, e.st, -1, e.l.ID, 5, "sentence five, edited"); err != nil || n != 7 {
		t.Fatalf("default window: %d %v", n, err)
	}
	if got := e.stale(); !reflect.DeepEqual(got, rng(2, 8)) {
		t.Fatalf("default window: stale = %v", got)
	}
}
