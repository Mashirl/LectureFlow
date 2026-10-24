// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Deleting a lecture clears the remembered model judgement of its kind
// (kv kind_llm:{lecture}; lecture kinds review, item 1): kv rows do not
// cascade with the lecture.
func TestDeleteLectureForgetsKindJudgement(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	d := e.draft()
	keep := e.draft()
	for _, l := range []model.Lecture{d, keep} {
		if err := state.RememberLLMKind(e.ctx, e.st, l.ID, model.KindTutorial); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.DeleteLecture(e.ctx, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if v, _, err := e.st.GetKV(e.ctx, state.KVKindLLMPrefix+d.ID); err != nil || v != "" {
		t.Fatalf("record of the deleted lecture %q %v", v, err)
	}
	if v, _, _ := e.st.GetKV(e.ctx, state.KVKindLLMPrefix+keep.ID); v != model.KindTutorial {
		t.Fatalf("record of another lecture %q", v)
	}
}
