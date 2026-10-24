// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// StageView.checkpoint in the mock (real-machine check 6.8 item 2): the
// lecture suspended in power_battery_suspended (and in power_all_suspended)
// is queued with progress 0 and carries its checkpoint's position, in the
// lecture view, the course's lecture list and the dashboard; normal queued
// stages and every other stage have none; the field goes once the stage
// leaves the queue, and a retry starts without it.
func TestMockSuspendedCheckpoint(t *testing.T) {
	srv := testServer(t)
	const sc = "power_battery_suspended"
	held, running := lecID(c2120, 7), lecID(c3631, 7)

	var l api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+held+"?mock="+sc, nil, http.StatusOK, &l)
	want := api.ASRCheckpointView{ResumeMS: 1276460, DurationMS: 3287340, Progress: 1276460.0 / 3287340.0}
	st := stageOfView(t, l.Stages, model.StageASR)
	if st.Status != model.StatusQueued || st.Progress != 0 || st.WaitingFor == nil || *st.WaitingFor != model.WaitPowerWaitAC {
		t.Fatalf("held asr %s %v %v", st.Status, st.Progress, st.WaitingFor)
	}
	if st.Checkpoint == nil || *st.Checkpoint != want {
		t.Fatalf("held checkpoint %+v, want %+v", st.Checkpoint, want)
	}
	// the check's 38.8 % (util.js pct floors it: "38%")
	if p := st.Checkpoint.Progress; p < 0.388 || p >= 0.389 {
		t.Errorf("held checkpoint %v, want 38.8 %%", p)
	}
	if l.DurationMS == nil || *l.DurationMS != 3287340 {
		t.Errorf("held duration %v", l.DurationMS)
	}
	for _, s := range l.Stages {
		if s.Stage != model.StageASR && s.Checkpoint != nil {
			t.Errorf("held %s has a checkpoint %+v", s.Stage, s.Checkpoint)
		}
	}
	// the lecture running on the NPU shows its own progress only
	if st := asrStageOf(t, srv.URL, running, sc); st.Status != model.StatusRunning || st.Checkpoint != nil {
		t.Errorf("running lecture %s checkpoint %+v", st.Status, st.Checkpoint)
	}
	// the same view in the course list and on the dashboard
	var ls []api.Lecture
	doJSON(t, "GET", srv.URL+"/api/courses/"+l.CourseID+"/lectures?mock="+sc, nil, http.StatusOK, &ls)
	found := false
	for _, x := range ls {
		if x.ID == held {
			found = true
			if c := stageOfView(t, x.Stages, model.StageASR).Checkpoint; c == nil || *c != want {
				t.Errorf("course list checkpoint %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("held lecture missing from its course list")
	}
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock="+sc, nil, http.StatusOK, &d)
	found = false
	for _, x := range d.InProgress {
		if x.ID == held {
			found = true
			if c := stageOfView(t, x.Stages, model.StageASR).Checkpoint; c == nil || *c != want {
				t.Errorf("dashboard checkpoint %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("held lecture missing from the dashboard")
	}

	// power_all_suspended: the lecture suspended at 42 % keeps 40.8 %; the
	// one queued for a power-saving device never started and has none
	all := asrStageOf(t, srv.URL, lecID(c3631, 7), "power_all_suspended")
	if c := all.Checkpoint; all.Status != model.StatusQueued || all.Progress != 0 || c == nil ||
		*c != (api.ASRCheckpointView{ResumeMS: 1172000, DurationMS: 2873000, Progress: 1172000.0 / 2873000.0}) {
		t.Errorf("all_suspended held %s %v %+v", all.Status, all.Progress, c)
	}
	if st := asrStageOf(t, srv.URL, lecID(c2120, 7), "power_all_suspended"); st.Status != model.StatusQueued || st.Checkpoint != nil {
		t.Errorf("all_suspended queued %s %+v", st.Status, st.Checkpoint)
	}

	// The default world: the same lecture is queued the normal way, no
	// checkpoint anywhere (not even the field).
	code, raw := rawCall(t, "GET", srv.URL+"/api/lectures/"+held+"?mock=1", nil)
	if code != http.StatusOK || strings.Contains(string(raw), `"checkpoint"`) {
		t.Errorf("default world: %d, checkpoint field present: %v", code, strings.Contains(string(raw), `"checkpoint"`))
	}
	if st := asrStageOf(t, srv.URL, held, "1"); st.Status != model.StatusQueued || st.Checkpoint != nil {
		t.Errorf("default queued %s %+v", st.Status, st.Checkpoint)
	}

	// Cancelled: no longer queued, the field goes; a retry is a new job.
	// (fresh values: decoding into l would keep the omitted field)
	var cancelled, retried api.Lecture
	doJSON(t, "POST", srv.URL+"/api/lectures/"+held+"/stages/asr/cancel?mock="+sc, nil, http.StatusOK, &cancelled)
	if st := stageOfView(t, cancelled.Stages, model.StageASR); st.Status != model.StatusCancelled || st.Checkpoint != nil {
		t.Errorf("cancelled %s %+v", st.Status, st.Checkpoint)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/"+held+"/stages/asr/retry?mock="+sc, nil, http.StatusOK, &retried)
	if st := stageOfView(t, retried.Stages, model.StageASR); st.Status != model.StatusQueued || st.Checkpoint != nil {
		t.Errorf("retried %s %+v", st.Status, st.Checkpoint)
	}
}

func stageOfView(t *testing.T, stages []api.StageView, name string) api.StageView {
	t.Helper()
	for _, s := range stages {
		if s.Stage == name {
			return s
		}
	}
	t.Fatalf("no %s stage", name)
	return api.StageView{}
}
