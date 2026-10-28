// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// "修复断句" (textrepair-int): the asr stage offers rerun_text_only on a
// transcript worth repairing, and the asr rerun with text_only queues it.

const trNormal = "So the key is random. We use XOR for this step, okay. The receiver does the same thing."

var trDegenerate = strings.Repeat("and then we take the key and the message ", 5) + "right."

type trLecture struct {
	language   string // the asr snapshot's
	degenerate bool
	words      bool
}

// textRepairLecture is a launched lecture whose six stages are done, with
// three stored sentences (the middle one a degenerate stretch when asked).
func (e *env) textRepairLecture(o trLecture) Lecture {
	e.t.Helper()
	ctx := context.Background()
	c := e.course("C " + db.NewID())
	l := e.lecture(c.ID, "L")
	for _, s := range model.LectureStages {
		e.moveJob(model.ScopeLecture, l.ID, s, model.StatusQueued, model.StatusRunning, model.StatusDone)
	}
	mid := trNormal
	if o.degenerate {
		mid = trDegenerate
	}
	var ss []model.Sentence
	for i, text := range []string{trNormal, mid, trNormal} {
		x := model.Sentence{Idx: i, StartMS: int64(i) * 20000, EndMS: int64(i)*20000 + 15000, ASRText: text, RepairedText: text,
			ZhStatus: model.ZhNone}
		if o.words {
			for k, w := range strings.Fields(text) {
				x.Words = append(x.Words, model.Word{W: w, S: x.StartMS + int64(k)*100, E: x.StartMS + int64(k)*100 + 90})
			}
		}
		ss = append(ss, x)
	}
	if err := e.st.InsertSentences(ctx, l.ID, ss); err != nil {
		e.t.Fatal(err)
	}
	asr := e.moveJob(model.ScopeLecture, l.ID, model.StageASR)
	snap, _ := model.ParseASRSnapshot(asr.ConfigSnapshot)
	snap.Language = o.language
	if err := e.st.SetJobSnapshot(ctx, asr.ID, model.MarshalSnapshot(snap)); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) asrActions(lectureID string) []string {
	e.t.Helper()
	var v Lecture
	e.call("GET", "/api/lectures/"+lectureID, nil, http.StatusOK, &v)
	return stageOf(v.Stages, model.StageASR).AllowedActions
}

func TestTextRepairOffer(t *testing.T) {
	e := newEnv(t)
	offered := []string{model.ActionRerun, model.ActionRerunTextOnly}
	plain := []string{model.ActionRerun}
	cases := []struct {
		name string
		o    trLecture
		want []string
	}{
		{"degenerate English with words", trLecture{language: "en", degenerate: true, words: true}, offered},
		{"English variant", trLecture{language: "en-GB", degenerate: true, words: true}, offered},
		{"normal text", trLecture{language: "en", words: true}, plain},
		{"another language", trLecture{language: "de", degenerate: true, words: true}, plain},
		{"no language", trLecture{language: "", degenerate: true, words: true}, plain},
		{"no word timestamps", trLecture{language: "en", degenerate: true}, plain},
	}
	for _, c := range cases {
		l := e.textRepairLecture(c.o)
		if got := e.asrActions(l.ID); !slices.Equal(got, c.want) {
			t.Errorf("%s: asr actions %v, want %v", c.name, got, c.want)
		}
	}
	ctx := context.Background()
	// not while a stage after asr runs (its rerun would be 409)
	l := e.textRepairLecture(trLecture{language: "en", degenerate: true, words: true})
	if err := state.Rerun(ctx, e.st, e.dir, model.ScopeLecture, l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning)
	if got := e.asrActions(l.ID); len(got) != 0 {
		t.Errorf("export running: %v", got)
	}
	// a transcript that went through the repair (a new transcription, or a
	// "修复断句" run) is not offered it again
	l = e.textRepairLecture(trLecture{language: "en", degenerate: true, words: true})
	asr := e.moveJob(model.ScopeLecture, l.ID, model.StageASR)
	st := asr.Stats
	st.ASRTiming = &model.ASRTiming{AudioMS: 1, Postprocess: &model.ASRPostprocess{TextRepair: &model.ASRTextRepair{Words: 3}}}
	if err := e.st.SetJobStats(ctx, asr.ID, st); err != nil {
		t.Fatal(err)
	}
	if got := e.asrActions(l.ID); !slices.Equal(got, plain) {
		t.Errorf("already repaired: %v", got)
	}
}

func TestTextRepairOfferIsCachedPerTranscript(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.textRepairLecture(trLecture{language: "en", degenerate: true, words: true})
	if got := e.asrActions(l.ID); !slices.Contains(got, model.ActionRerunTextOnly) {
		t.Fatalf("not offered: %v", got)
	}
	// the same transcript: the cached verdict (the sentences are not read again)
	e.srv.trOffers.mu.Lock()
	c := e.srv.trOffers.m[l.ID]
	c.offer = false
	e.srv.trOffers.m[l.ID] = c
	e.srv.trOffers.mu.Unlock()
	if got := e.asrActions(l.ID); slices.Contains(got, model.ActionRerunTextOnly) {
		t.Errorf("the verdict was not cached: %v", got)
	}
	// a new asr run (here the transcript replaced by normal text): measured again
	if err := e.st.DeleteSentences(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertSentences(ctx, l.ID, []model.Sentence{{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: trNormal,
		RepairedText: trNormal, Words: []model.Word{{W: "So", S: 0, E: 100}}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.Rerun(ctx, e.st, e.dir, model.ScopeLecture, l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.srv.trOffers.mu.Lock()
	c = e.srv.trOffers.m[l.ID]
	c.key = "another transcript"
	c.offer = true
	e.srv.trOffers.m[l.ID] = c
	e.srv.trOffers.mu.Unlock()
	if got := e.asrActions(l.ID); slices.Contains(got, model.ActionRerunTextOnly) {
		t.Errorf("not measured again for another transcript: %v", got)
	}
}

func TestTextRepairRerunAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.textRepairLecture(trLecture{language: "en", degenerate: true, words: true})
	h := "So the key is random!"
	if err := e.st.SetHumanText(ctx, l.ID, 0, &h); err != nil {
		t.Fatal(err)
	}
	old := e.moveJob(model.ScopeLecture, l.ID, model.StageASR)
	path := "/api/lectures/" + l.ID + "/stages/asr/rerun"

	// the request field round trip
	var req ModelChoiceRequest
	if err := json.Unmarshal([]byte(`{"text_only":true,"model_choice":"snapshot"}`), &req); err != nil ||
		!req.TextOnly || req.ModelChoice != model.ModelChoiceSnapshot {
		t.Fatalf("%+v %v", req, err)
	}
	if b, _ := json.Marshal(ModelChoiceRequest{}); string(b) != `{}` {
		t.Errorf("empty request %s", b)
	}

	// text_only belongs to the asr rerun only
	for _, p := range []string{"/api/lectures/" + l.ID + "/stages/summary/rerun", "/api/lectures/" + l.ID + "/stages/asr/retry"} {
		if code := e.callErrInfo("POST", p, map[string]any{"text_only": true}, http.StatusBadRequest).Code; code != "stage.invalid" {
			t.Errorf("%s: %s", p, code)
		}
	}
	var v Lecture
	e.call("POST", path, map[string]any{"text_only": true}, http.StatusOK, &v)
	asr := e.moveJob(model.ScopeLecture, l.ID, model.StageASR)
	if asr.ID == old.ID || asr.Status != model.StatusPending || asr.Stats.ASRMode != model.ASRModeTextRepair ||
		asr.ConfigSnapshot != old.ConfigSnapshot {
		t.Fatalf("asr job %+v", asr)
	}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		if st := stageOf(v.Stages, s); st.Status != model.StatusPending {
			t.Errorf("%s is %s", s, st.Status)
		}
	}
	if st := stageOf(v.Stages, model.StagePrep); st.Status != model.StatusDone {
		t.Errorf("prep is %s", st.Status)
	}
	// the sentences stay for the repair, the reviewed one with its text
	if n, _ := e.st.CountSentences(ctx, l.ID); n != 3 {
		t.Errorf("%d sentences", n)
	}
	if x, _ := e.st.GetSentence(ctx, l.ID, 0); x.HumanText == nil || x.Text != h {
		t.Errorf("sentence 0 %+v", x)
	}
	// a second one while it waits: 409 as any rerun
	e.callErrInfo("POST", path, map[string]any{"text_only": true}, http.StatusConflict)

	// refusals: no word timestamps, not English
	nw := e.textRepairLecture(trLecture{language: "en", degenerate: true})
	if code := e.callErrInfo("POST", "/api/lectures/"+nw.ID+"/stages/asr/rerun", map[string]any{"text_only": true},
		http.StatusConflict).Code; code != stages.CodeTextRepairNoWords {
		t.Errorf("no words: %s", code)
	}
	de := e.textRepairLecture(trLecture{language: "de", degenerate: true, words: true})
	info := e.callErrInfo("POST", "/api/lectures/"+de.ID+"/stages/asr/rerun", map[string]any{"text_only": true}, http.StatusConflict)
	if info.Code != stages.CodeTextRepairLanguage || info.Params["language"] != "de" {
		t.Errorf("German: %+v", info)
	}
	// nothing changed by the refusals
	if j := e.moveJob(model.ScopeLecture, de.ID, model.StageASR); j.Status != model.StatusDone || j.Stats.ASRMode != "" {
		t.Errorf("German lecture's asr %+v", j)
	}
	// a plain rerun is still a transcription (the test changed the snapshot's
	// language, so it asks which model to use: keep the snapshot)
	e.call("POST", "/api/lectures/"+de.ID+"/stages/asr/rerun", map[string]any{"model_choice": "snapshot"}, http.StatusOK, nil)
	if j := e.moveJob(model.ScopeLecture, de.ID, model.StageASR); j.Stats.ASRMode != "" {
		t.Errorf("plain rerun %+v", j.Stats)
	}
	if n, _ := e.st.CountSentences(ctx, de.ID); n != 0 {
		t.Errorf("a plain asr rerun keeps %d sentences", n)
	}
}
