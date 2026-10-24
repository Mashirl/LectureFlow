// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// The automatic kind again (PUT /api/lectures/{id}/kind {kind: ""}) and the
// renumbering check of PUT filename-rule fall back to the model's
// remembered judgement (lecture kinds review, item 1): llm → user →
// automatic gives the llm kind back; with a file-name kind, the file name;
// a rule that no longer gives a kind is predicted (and applied) as the llm
// kind, so the P13.6 check sees that renumbering.
func TestKindAutomaticFallsBackToLLM(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, ls := kindCourse(e)
	l2 := ls["L2"].ID // C-L01-02, week 2 after L1, T1, T2
	// the title task judged it a seminar (as applyTitleKind writes it)
	if err := state.WriteLectureKind(ctx, e.st, e.storedLecture(l2), model.KindSeminar, model.KindSourceLLM); err != nil {
		t.Fatal(err)
	}
	var lv Lecture
	e.call("PUT", "/api/lectures/"+l2+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, &lv)
	if lv.Kind != model.KindTutorial || lv.KindSource != KindSourceUser {
		t.Fatalf("user kind %+v", lv)
	}
	e.call("PUT", "/api/lectures/"+l2+"/kind", LectureKindSet{Kind: ""}, http.StatusOK, &lv)
	if lv.Kind != model.KindSeminar || lv.KindSource != KindSourceLLM || lv.Label != "W02S1" {
		t.Fatalf("automatic again: %s (%s) %s, want the model's seminar", lv.Kind, lv.KindSource, lv.Label)
	}

	// a rule whose kind_map gives L → lecture: the file name wins over llm
	base := "/api/courses/" + c.ID + "/filename-rule"
	withKind := `^C-(?P<kind>[A-Z])\d{2}-\d{2} (?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	e.call("PUT", base, FilenamePreviewInput{Regex: withKind, KindMap: map[string]string{"L": model.KindLecture}, Confirm: true}, http.StatusOK, nil)
	if st := e.storedLecture(l2); st.Kind != model.KindLecture || st.KindSource != model.KindSourceFilename {
		t.Fatalf("rule applied: %s/%s", st.Kind, st.KindSource)
	}
	e.call("PUT", "/api/lectures/"+l2+"/kind", LectureKindSet{Kind: model.KindTutorial}, http.StatusOK, &lv)
	e.call("PUT", "/api/lectures/"+l2+"/kind", LectureKindSet{Kind: ""}, http.StatusOK, &lv)
	if lv.Kind != model.KindLecture || lv.KindSource != KindSourceFilename || lv.Label != "W02L4" {
		t.Fatalf("automatic with a file-name kind: %s (%s) %s", lv.Kind, lv.KindSource, lv.Label)
	}

	// a rule without the kind group, after a citation export: the check
	// predicts the fallback to the model's seminar (W02L4 → W02S1)
	if err := stages.MarkCiteExported(ctx, e.st, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	noKind := `^C-[A-Z]\d{2}-\d{2} (?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	res, raw := e.do("PUT", base, FilenamePreviewInput{Regex: noKind})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("rule without kinds: %d %s (the renumbering was not predicted)", res.StatusCode, raw)
	}
	var rc RenumberConflict
	decodeStrict(t, raw, &rc)
	if len(rc.Changes) != 1 || rc.Changes[0].LectureID != l2 || rc.Changes[0].From != "W02L4" || rc.Changes[0].To != "W02S1" {
		t.Fatalf("changes %+v", rc.Changes)
	}
	e.call("PUT", base, FilenamePreviewInput{Regex: noKind, Confirm: true}, http.StatusOK, nil)
	if lv = e.lectureView(l2); lv.Kind != model.KindSeminar || lv.KindSource != KindSourceLLM || lv.Label != "W02S1" {
		t.Fatalf("applied: %s (%s) %s, want what was predicted", lv.Kind, lv.KindSource, lv.Label)
	}
	// L1 had no model judgement: its file-name kind went to none
	if st := e.storedLecture(ls["L1"].ID); st.Kind != "" || st.KindSource != model.KindSourceNone {
		t.Errorf("L1 %s/%s", st.Kind, st.KindSource)
	}
}
