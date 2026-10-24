// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// v2 P4.2 (accept-fix): the mock has one lecture transcribed "before v2"
// (mockNoWordsLecture) without word timestamps: has_words false and
// fill_words in lecture_actions, as the real views.go; every other
// transcribed lecture has words. Rerunning its asr (the front end's
// fill_words) gives a transcript with words, so the action goes away.
func TestFillWords(t *testing.T) {
	srv := testServer(t)
	offered := func(id string) (bool, bool) {
		l := lectureOf(t, srv.URL, id)
		return l.HasWords, slices.Contains(l.LectureActions, model.LectureActionFillWords)
	}
	if words, fill := offered(mockNoWordsLecture); words || !fill {
		t.Errorf("lecture without words: has_words %v, fill_words %v", words, fill)
	}
	if words, fill := offered(lecID(c3631, 1)); !words || fill {
		t.Errorf("lecture with words: has_words %v, fill_words %v", words, fill)
	}
	if words, fill := offered(lecID(c3631, 7)); words || fill { // asr running
		t.Errorf("asr running: has_words %v, fill_words %v", words, fill)
	}
	code, b := rawCall(t, "POST", srv.URL+"/api/lectures/"+mockNoWordsLecture+"/stages/asr/rerun",
		map[string]string{"model_choice": model.ModelChoiceSnapshot})
	if code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("asr rerun: %d %s", code, b)
	}
	if words, fill := offered(mockNoWordsLecture); words || fill {
		t.Errorf("after the rerun (asr queued): has_words %v, fill_words %v", words, fill)
	}
}
