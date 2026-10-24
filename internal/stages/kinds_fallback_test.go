// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"strings"
	"testing"

	"lectureflow/internal/calendar"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// The model's kind judgement survives a stronger source (lecture kinds
// review, item 1): a file-name or user kind replacing an llm kind keeps it
// in kv kind_llm:{lecture}, the title task remembers the kind it could not
// write, and a lecture whose file-name kind goes falls back to it (source
// llm) instead of none.

// noKindRegex is llmtest.PanoptoRegex without its kind group.
var noKindRegex = strings.Replace(llmtest.PanoptoRegex, "(?P<kind>[A-Z]+)", "[A-Z]+", 1)

func (e *env) remembered(id string) string {
	e.t.Helper()
	k, err := state.RememberedLLMKind(e.ctx, e.st, e.lec(id))
	e.must(err)
	return k
}

// TestFilenameKindFallsBackToLLM: llm → filename (the rule's kind_map) →
// a rule that gives no kind → the llm kind again; a lecture never judged
// by the model goes to none; a remembered kind that left the catalog is
// none.
func TestFilenameKindFallsBackToLLM(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	ls := e.uploadPanopto() // L, T, L, T
	name := "Lab"
	lab, err := e.st.InsertLectureKind(e.ctx, model.LectureKind{Code: "LAB", Name: &name, Group: model.KindGroupSession})
	e.must(err)
	// the title task's kinds (state.WriteLectureKind, as applyTitleKind writes them)
	e.must(state.WriteLectureKind(e.ctx, e.st, e.lec(ls[1].ID), model.KindSeminar, model.KindSourceLLM))
	e.must(state.WriteLectureKind(e.ctx, e.st, e.lec(ls[2].ID), lab.ID, model.KindSourceLLM))
	// a kind written before the record existed (no kv): still remembered
	e.setKind(ls[3].ID, model.KindSeminar, model.KindSourceLLM)

	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: llmtest.PanoptoRegex, KindMap: llmtest.PanoptoKindMap}))
	want := [][2]string{
		{model.KindLecture, model.KindSourceFilename},
		{model.KindTutorial, model.KindSourceFilename},
		{model.KindLecture, model.KindSourceFilename},
		{model.KindTutorial, model.KindSourceFilename},
	}
	for i, l := range ls {
		if k, src := e.kindOf(l.ID); k != want[i][0] || src != want[i][1] {
			t.Errorf("rule with kinds, %s: %q (%s), want %v", l.OriginalFilename, k, src, want[i])
		}
	}
	if got := []string{e.remembered(ls[0].ID), e.remembered(ls[1].ID), e.remembered(ls[2].ID), e.remembered(ls[3].ID)}; got[0] != "" ||
		got[1] != model.KindSeminar || got[2] != lab.ID || got[3] != model.KindSeminar {
		t.Errorf("remembered %v", got)
	}

	// the custom kind leaves the catalog (no lecture stores it now)
	e.must(e.st.DeleteLectureKind(e.ctx, lab.ID))
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: noKindRegex}))
	want = [][2]string{
		{"", model.KindSourceNone},               // never judged by the model
		{model.KindSeminar, model.KindSourceLLM}, // back to the model's kind
		{"", model.KindSourceNone},               // its kind was deleted
		{model.KindSeminar, model.KindSourceLLM}, // written before the record
	}
	for i, l := range ls {
		if k, src := e.kindOf(l.ID); k != want[i][0] || src != want[i][1] {
			t.Errorf("rule without kinds, %s: %q (%s), want %v", l.OriginalFilename, k, src, want[i])
		}
	}
	nums, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	if n := nums[ls[1].ID]; n.Kind != model.KindSeminar {
		t.Errorf("number of the fallen-back lecture %+v", n)
	}

	// the rule's kinds again, then none again: the same fallback
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: llmtest.PanoptoRegex, KindMap: llmtest.PanoptoKindMap}))
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: noKindRegex}))
	if k, src := e.kindOf(ls[1].ID); k != model.KindSeminar || src != model.KindSourceLLM {
		t.Errorf("second round: %q (%s)", k, src)
	}
}

// TestTitleKindRememberedUnderStrongerSource: a title answer refused
// because the file name or the user holds the lecture is remembered; when
// the file-name kind goes, the lecture falls back to it.
func TestTitleKindRememberedUnderStrongerSource(t *testing.T) {
	for _, src := range []string{model.KindSourceFilename, model.KindSourceUser} {
		t.Run(src, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			l := e.titleLecture("x.mp4")
			e.setKind(l.ID, model.KindTutorial, src)
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind("Seminar") })
			if r := e.runTitle(d, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			if k, s := e.kindOf(l.ID); k != model.KindTutorial || s != src {
				t.Fatalf("kind %q (%s): the stronger source must stay", k, s)
			}
			if got := e.remembered(l.ID); got != model.KindSeminar { // the catalog's spelling
				t.Fatalf("remembered %q", got)
			}
			// a rule that does not match x.mp4: a file-name kind falls
			// back to the remembered judgement, a user kind stays
			e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: noKindRegex}))
			wantKind, wantSrc := model.KindSeminar, model.KindSourceLLM
			if src == model.KindSourceUser {
				wantKind, wantSrc = model.KindTutorial, model.KindSourceUser
			}
			if k, s := e.kindOf(l.ID); k != wantKind || s != wantSrc {
				t.Errorf("after the rule: %q (%s), want %s (%s)", k, s, wantKind, wantSrc)
			}
		})
	}
	// an answer the title task writes is the latest judgement too
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	e.must(state.RememberLLMKind(e.ctx, e.st, l.ID, model.KindSeminar))
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind(model.KindTutorial) })
	if r := e.runTitle(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if k, s := e.kindOf(l.ID); k != model.KindTutorial || s != model.KindSourceLLM {
		t.Errorf("written kind %q (%s)", k, s)
	}
	if got := e.remembered(l.ID); got != model.KindTutorial {
		t.Errorf("remembered %q after a written answer", got)
	}
}

// TestUndoFilenameRuleRemembersLLMKind: undoing an automatic apply gives a
// lecture whose kind came from the rule what was below the file name: the
// model's latest judgement when one is remembered (judged while the rule
// held the lecture), else the kind as recorded.
func TestUndoFilenameRuleRemembersLLMKind(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	ls := e.uploadPanopto()
	for _, l := range ls {
		e.setKind(l.ID, model.KindTutorial, model.KindSourceFilename)
	}
	// ls[0]: none before, judged seminar since; ls[1]: llm lecture before,
	// nothing remembered; ls[2]: llm lecture before, judged seminar since;
	// ls[3]: an earlier rule's kind before
	e.must(state.RememberLLMKind(e.ctx, e.st, ls[0].ID, model.KindSeminar))
	e.must(state.RememberLLMKind(e.ctx, e.st, ls[2].ID, model.KindSeminar))
	rec := func(l model.Lecture, kind, src string) FilenameUndoLecture {
		return FilenameUndoLecture{ID: l.ID, RecordedAt: l.RecordedAt, RecordedAtSource: l.RecordedAtSource, Title: l.Title,
			TitleSource: l.TitleSource, Kind: kind, KindSource: src}
	}
	cur := func(i int) model.Lecture { return e.lec(ls[i].ID) }
	u := FilenameUndo{Lectures: []FilenameUndoLecture{
		rec(cur(0), "", model.KindSourceNone),
		rec(cur(1), model.KindLecture, model.KindSourceLLM),
		rec(cur(2), model.KindLecture, model.KindSourceLLM),
		rec(cur(3), model.KindSeminar, model.KindSourceFilename),
	}}
	b, err := json.Marshal(u)
	e.must(err)
	e.must(e.st.SetKV(e.ctx, model.KVFilenameUndo+e.course.ID, string(b)))
	e.must(UndoFilenameRule(e.ctx, d, e.course.ID))
	want := [][2]string{
		{model.KindSeminar, model.KindSourceLLM},
		{model.KindLecture, model.KindSourceLLM},
		{model.KindSeminar, model.KindSourceLLM},
		{model.KindSeminar, model.KindSourceFilename},
	}
	for i, l := range ls {
		if k, src := e.kindOf(l.ID); k != want[i][0] || src != want[i][1] {
			t.Errorf("%d: %q (%s), want %v", i, k, src, want[i])
		}
	}
}
