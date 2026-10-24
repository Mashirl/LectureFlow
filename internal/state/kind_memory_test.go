// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"testing"

	"lectureflow/internal/model"
)

// Tests of the remembered model judgement of a lecture's kind (lecture
// kinds review, item 1; kind_memory.go).

func (f *fixture) stored(id string) model.Lecture {
	f.t.Helper()
	l, err := f.st.GetLecture(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fixture) rememberedKind(id string) string {
	f.t.Helper()
	k, err := RememberedLLMKind(f.ctx, f.st, f.stored(id))
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}

func (f *fixture) kvValue(key string) string {
	f.t.Helper()
	v, _, err := f.st.GetKV(f.ctx, key)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func TestWriteLectureKindRemembersLLM(t *testing.T) {
	f := newFixture(t)
	kinds, err := f.st.ListLectureKinds(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := f.lecture()
	write := func(kind, source string) {
		t.Helper()
		if err := WriteLectureKind(f.ctx, f.st, f.stored(l.ID), kind, source); err != nil {
			t.Fatal(err)
		}
	}
	fallback := func() (string, string) {
		t.Helper()
		k, s, err := FallbackKind(f.ctx, f.st, f.stored(l.ID), kinds)
		if err != nil {
			t.Fatal(err)
		}
		return k, s
	}
	if k := f.rememberedKind(l.ID); k != "" {
		t.Fatalf("fresh lecture remembers %q", k)
	}
	if k, s := fallback(); k != "" || s != model.KindSourceNone {
		t.Fatalf("fresh fallback %q (%s)", k, s)
	}
	// an llm kind written is the judgement; replaced by filename, then by
	// the user, it stays
	write(model.KindTutorial, model.KindSourceLLM)
	if v := f.kvValue(KVKindLLMPrefix + l.ID); v != model.KindTutorial {
		t.Errorf("kv after an llm write %q", v)
	}
	write(model.KindLecture, model.KindSourceFilename)
	write(model.KindSeminar, model.KindSourceUser)
	if got := f.stored(l.ID); got.Kind != model.KindSeminar || got.KindSource != model.KindSourceUser {
		t.Errorf("stored %s/%s", got.Kind, got.KindSource)
	}
	if k, s := fallback(); k != model.KindTutorial || s != model.KindSourceLLM {
		t.Errorf("fallback %q (%s), want tutorial (llm)", k, s)
	}

	// a lecture whose llm kind predates the record: replacing it saves it
	l2 := f.lecture()
	if err := f.st.SetLectureKind(f.ctx, l2.ID, model.KindSeminar, model.KindSourceLLM); err != nil {
		t.Fatal(err)
	}
	if k := f.rememberedKind(l2.ID); k != model.KindSeminar {
		t.Errorf("llm kind without a record: %q", k)
	}
	if err := WriteLectureKind(f.ctx, f.st, f.stored(l2.ID), model.KindLecture, model.KindSourceUser); err != nil {
		t.Fatal(err)
	}
	if k := f.rememberedKind(l2.ID); k != model.KindSeminar {
		t.Errorf("replaced llm kind not remembered: %q", k)
	}
	// RememberLLMKind (a judgement the title task could not write) is the
	// newest; an empty kind changes nothing
	if err := RememberLLMKind(f.ctx, f.st, l2.ID, model.KindTutorial); err != nil {
		t.Fatal(err)
	}
	if err := RememberLLMKind(f.ctx, f.st, l2.ID, " "); err != nil {
		t.Fatal(err)
	}
	if k := f.rememberedKind(l2.ID); k != model.KindTutorial {
		t.Errorf("remembered %q", k)
	}

	// a remembered kind not in the catalog is no fallback
	if k, s := LLMFallback("01JGONE", kinds); k != "" || s != model.KindSourceNone {
		t.Errorf("unknown kind fallback %q (%s)", k, s)
	}
	if k, s := LLMFallback("", kinds); k != "" || s != model.KindSourceNone {
		t.Errorf("empty fallback %q (%s)", k, s)
	}
	// deleting the lecture clears the record
	if err := ForgetLectureKind(f.ctx, f.st, l2.ID); err != nil {
		t.Fatal(err)
	}
	if v := f.kvValue(KVKindLLMPrefix + l2.ID); v != "" {
		t.Errorf("record after forget %q", v)
	}
}
