// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// v2 P14.3 (codes-p2): a stage's hover-card note and a done stage's
// warning carry their coded forms; the API's own sentences and the stage's
// note form one coded note (main code + params.extra); a note stored
// without a code is legacy {text}.
func TestStageViewNoteInfo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Notes")
	l := e.lecture(c.ID, "L1")
	setStats := func(stage string, st model.JobStats) {
		t.Helper()
		j := e.moveJob(model.ScopeLecture, l.ID, stage, model.StatusQueued, model.StatusRunning, model.StatusDone)
		if err := e.st.SetJobStats(ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.moveJob(model.ScopeLecture, l.ID, model.StageSummary, model.StatusQueued, model.StatusRunning, model.StatusDone)
	// repair: a coded note with an extra part, and a warning
	note := state.JoinNote(
		model.ErrorInfo{Code: "repair.chunks_failed", Params: map[string]any{"total": 3, "chunks": "2", "ranges": "41–80", "attempts": 3}},
		model.ErrorInfo{Code: "glossary.fallback_applied", Params: map[string]any{"reference": 1, "disabled": 0}})
	setStats(model.StageRepair, model.JobStats{Note: "共 3 块…", NoteInfo: note, Warning: "第 41–80 句…",
		WarningInfo: &model.ErrorInfo{Code: "repair.sentences_unrepaired", Params: map[string]any{"ranges": "41–80", "attempts": 3}}})
	// translate: a note stored without a code (before v2)
	setStats(model.StageTranslate, model.JobStats{Note: "old note"})

	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	rp := stageOf(lv.Stages, model.StageRepair)
	parts := state.NoteParts(rp.NoteInfo)
	if rp.Note == nil || len(parts) != 2 || parts[0].Code != "repair.chunks_failed" || fmt.Sprint(parts[0].Params["ranges"]) != "41–80" ||
		parts[1].Code != "glossary.fallback_applied" {
		t.Fatalf("repair note %v %+v", rp.Note, rp.NoteInfo)
	}
	if rp.WarningInfo == nil || rp.WarningInfo.Code != "repair.sentences_unrepaired" {
		t.Fatalf("repair warning %+v", rp.WarningInfo)
	}
	tr := stageOf(lv.Stages, model.StageTranslate)
	if tr.NoteInfo == nil || tr.NoteInfo.Code != state.CodeNoteLegacy || tr.NoteInfo.Params["text"] != "old note" {
		t.Fatalf("legacy note %+v", tr.NoteInfo)
	}
	if sm := stageOf(lv.Stages, model.StageSummary); sm.NoteInfo != nil || sm.WarningInfo != nil || sm.Note != nil {
		t.Fatalf("summary without a note %+v", sm)
	}
}
