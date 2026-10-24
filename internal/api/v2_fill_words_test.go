// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// insertTranscript stores two sentences, with word timestamps or without
// (a lecture transcribed before v2), the way asr does: all or none.
func (e *env) insertTranscript(lectureID string, withWords bool) {
	e.t.Helper()
	ctx := context.Background()
	ss := []model.Sentence{
		{Idx: 0, StartMS: 1000, EndMS: 2000, ASRText: "A block cipher.", RepairedText: "A block cipher.", ZhStatus: model.ZhNone},
		{Idx: 1, StartMS: 2500, EndMS: 4000, ASRText: "It works on blocks.", RepairedText: "It works on blocks.", ZhStatus: model.ZhNone},
	}
	if withWords {
		ss[0].Words = []model.Word{{W: "A", S: 1000, E: 1100}, {W: "block", S: 1100, E: 1500}, {W: "cipher.", S: 1500, E: 2000}}
		ss[1].Words = []model.Word{{W: "It", S: 2500, E: 2700}, {W: "works", S: 2700, E: 3100}, {W: "on", S: 3100, E: 3300}, {W: "blocks.", S: 3300, E: 4000}}
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, lectureID, ss); err != nil {
			return err
		}
		return tx.RecomputeText(ctx, lectureID, nil)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// SPEC-v2 P4.2 "补全词级时间戳" (phase 2 acceptance: the entry was missing):
// Lecture.has_words says whether the transcript has word timestamps, and
// lecture_actions offers fill_words — an asr rerun, which the front end
// sends as usual — exactly when there are none, asr is done or stale and
// the rerun would be accepted now.
func TestLectureHasWordsAndFillWords(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	offered := func(l Lecture) bool { return slices.Contains(l.LectureActions, model.LectureActionFillWords) }
	inList := func(id string) Lecture {
		var list []Lecture
		e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
		for _, l := range list {
			if l.ID == id {
				return l
			}
		}
		t.Fatalf("lecture %s not in the course list", id)
		return Lecture{}
	}

	old := e.lecture(c.ID, "Old")
	e.allDone(old.ID)
	e.insertTranscript(old.ID, false)
	for _, v := range []Lecture{e.lectureView(old.ID), inList(old.ID)} {
		if v.HasWords || !offered(v) {
			t.Errorf("no words, asr done: has_words %v, actions %v", v.HasWords, v.LectureActions)
		}
	}
	// stale asr: still offered (its rerun is allowed)
	e.moveJob(model.ScopeLecture, old.ID, model.StageASR, model.StatusStale)
	if v := e.lectureView(old.ID); v.HasWords || !offered(v) {
		t.Errorf("no words, asr stale: has_words %v, actions %v", v.HasWords, v.LectureActions)
	}

	fresh := e.lecture(c.ID, "New")
	e.allDone(fresh.ID)
	e.insertTranscript(fresh.ID, true)
	for _, v := range []Lecture{e.lectureView(fresh.ID), inList(fresh.ID)} {
		if !v.HasWords || offered(v) {
			t.Errorf("with words: has_words %v, actions %v", v.HasWords, v.LectureActions)
		}
	}

	// asr not finished: no transcript to fill in yet
	running := e.lecture(c.ID, "Running")
	e.moveJob(model.ScopeLecture, running.ID, model.StagePrep, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, running.ID, model.StageASR, "queued", "running")
	if v := e.lectureView(running.ID); v.HasWords || offered(v) {
		t.Errorf("asr running: has_words %v, actions %v", v.HasWords, v.LectureActions)
	}

	// a downstream stage running: the asr rerun would be 409, not offered
	busy := e.lecture(c.ID, "Busy")
	e.moveJob(model.ScopeLecture, busy.ID, model.StagePrep, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, busy.ID, model.StageASR, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, busy.ID, model.StageTranslate, "queued", "running")
	e.insertTranscript(busy.ID, false)
	if v := e.lectureView(busy.ID); offered(v) {
		t.Errorf("translate running: actions %v", v.LectureActions)
	}

	// a draft: never
	d := e.draft(c.ID, "Draft")
	if v := e.lectureView(d.ID); v.HasWords || offered(v) {
		t.Errorf("draft: has_words %v, actions %v", v.HasWords, v.LectureActions)
	}
}
