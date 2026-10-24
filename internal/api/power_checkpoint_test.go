// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// StageView.checkpoint (power management, real-machine check
// docs/benchmarks/power-checkpoint.md §6.8 item 2): an asr job the power
// policy put back in line (queued, stats.asr_resume) shows how much of the
// lecture its checkpoint already holds — resume_ms over the lecture's
// duration_ms — in the lecture view, the course's lecture list and the
// dashboard; progress itself stays the job's 0. A normal queued stage, a
// running one, or a checkpoint the next attempt would not continue from
// (another job, another format, no parts) shows nothing.
func TestASRQueuedCheckpointProgress(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "W03 L1 Block ciphers")
	const durMS, resumeMS = 80000, 31040 // 38.8 %, as in the check
	writeWAV(t, filepath.Join(e.dirOf(l), model.AudioFile), durMS)
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	j := e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued, model.StatusRunning)

	// The scheduler's power interrupt: running → queued, then its stats.
	if err := state.PowerRequeue(ctx, e.st, j.ID); err != nil {
		t.Fatal(err)
	}
	setResume := func(r *model.ASRResume) {
		t.Helper()
		cur, err := e.st.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		cur.Stats.ASRResume = r
		if r != nil {
			cur.Stats.PowerInterrupts = 1
		}
		if err := e.st.SetJobStats(ctx, j.ID, cur.Stats); err != nil {
			t.Fatal(err)
		}
	}
	setResume(&model.ASRResume{DeviceID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, At: "2026-10-24T08:55:47Z"})

	ckPath := filepath.Join(e.dirOf(l), model.ASRCheckpointFile)
	mtime := time.Now()
	writeCk := func(ck devices.Checkpoint) {
		t.Helper()
		b, err := json.Marshal(ck)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ckPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
		// every rewrite gets a later time: the cache follows size and mtime
		mtime = mtime.Add(time.Minute)
		if err := os.Chtimes(ckPath, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	seek := int64(107004)
	nsp, alp := 0.01, -0.2
	valid := func() devices.Checkpoint {
		return devices.Checkpoint{
			Version: devices.CheckpointVersion, JobID: j.ID, InputHash: "h", Backend: model.BackendFasterWhisper,
			Engine: "faster-whisper 1.2.1", VAD: "VadOptions()", ResumeMS: resumeMS, ResumeSeek: seek,
			Parts: []model.ASRPart{{FromMS: 0, ToMS: resumeMS, DeviceID: model.DeviceCUDA0, Model: "large-v2", Precision: "auto"}},
			Segments: []devices.CheckpointSegment{{StartMS: 1000, EndMS: 4000, Seek: &seek, NoSpeechProb: &nsp, AvgLogprob: &alp,
				Words: []devices.CheckpointWord{{Word: " Block", Start: 1, End: 1.4, Probability: 0.9}}}},
		}
	}

	asrView := func(what string) (StageView, string) {
		t.Helper()
		res, raw := e.do("GET", "/api/lectures/"+l.ID, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", what, res.StatusCode, raw)
		}
		var lv Lecture
		if err := json.Unmarshal(raw, &lv); err != nil {
			t.Fatal(err)
		}
		for _, st := range lv.Stages {
			if st.Stage != model.StageASR && st.Checkpoint != nil {
				t.Errorf("%s: stage %s has a checkpoint %+v", what, st.Stage, st.Checkpoint)
			}
		}
		return stageOf(lv.Stages, model.StageASR), string(raw)
	}
	none := func(what string) {
		t.Helper()
		st, raw := asrView(what)
		if st.Checkpoint != nil || strings.Contains(raw, `"checkpoint"`) {
			t.Errorf("%s: checkpoint %+v (raw has the field: %v)", what, st.Checkpoint, strings.Contains(raw, `"checkpoint"`))
		}
	}
	want := ASRCheckpointView{ResumeMS: resumeMS, DurationMS: durMS, Progress: 0.388}
	check := func(what string, st StageView, w ASRCheckpointView) {
		t.Helper()
		if st.Status != model.StatusQueued || st.Progress != 0 {
			t.Errorf("%s: status %s progress %v, want queued 0", what, st.Status, st.Progress)
		}
		if st.Checkpoint == nil || *st.Checkpoint != w {
			t.Errorf("%s: checkpoint %+v, want %+v", what, st.Checkpoint, w)
		}
	}

	// asr_resume but no checkpoint file yet (the worker had written none).
	none("no file")

	writeCk(valid())
	st, raw := asrView("power requeue")
	check("lecture view", st, want)
	if !strings.Contains(raw, `"checkpoint":{"resume_ms":31040,"duration_ms":80000,"progress":0.388}`) {
		t.Errorf("JSON of the checkpoint: %s", raw)
	}
	// The course's lecture list and the dashboard carry the same view.
	var ls []Lecture
	e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &ls)
	found := false
	for _, x := range ls {
		if x.ID == l.ID {
			found = true
			check("course list", stageOf(x.Stages, model.StageASR), want)
		}
	}
	if !found {
		t.Fatal("lecture missing from the course list")
	}
	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	found = false
	for _, x := range d.InProgress {
		if x.ID == l.ID {
			found = true
			check("dashboard", stageOf(x.Stages, model.StageASR), want)
		}
	}
	if !found {
		t.Fatal("lecture missing from the dashboard's in_progress")
	}

	// The cache follows the file: a later checkpoint moves the share.
	later := valid()
	later.ResumeMS = 60000
	later.Parts[0].ToMS = 60000
	writeCk(later)
	st, _ = asrView("later checkpoint")
	check("later checkpoint", st, ASRCheckpointView{ResumeMS: 60000, DurationMS: durMS, Progress: 0.75})

	// Checkpoints the next attempt would not continue from.
	other := valid()
	other.JobID = "01JDOTHERJOB000000000000000"
	writeCk(other)
	none("another job's checkpoint")
	v1 := valid()
	v1.Version = 1
	writeCk(v1)
	none("format version 1")
	noParts := valid()
	noParts.Parts = nil
	writeCk(noParts)
	none("no parts")
	nothing := valid()
	nothing.ResumeMS = 0
	writeCk(nothing)
	none("resume_ms 0")
	if err := os.WriteFile(ckPath, []byte(`{"version":2,"job_id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime = mtime.Add(time.Minute)
	os.Chtimes(ckPath, mtime, mtime)
	none("unreadable file")

	writeCk(valid())
	if st, _ := asrView("valid again"); st.Checkpoint == nil {
		t.Fatal("valid again: no checkpoint")
	}
	// A normal queued job (no power interrupt, e.g. §4.7 recovery) is shown
	// as before even with a checkpoint on disk.
	setResume(nil)
	none("queued without asr_resume")
	setResume(&model.ASRResume{DeviceID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, At: "2026-10-24T08:55:47Z"})

	// Without a known duration there is no share to show.
	if err := os.Remove(filepath.Join(e.dirOf(l), model.AudioFile)); err != nil {
		t.Fatal(err)
	}
	if lv := e.lectureView(l.ID); lv.DurationMS != nil {
		t.Fatalf("duration without audio.wav and transcript: %d", *lv.DurationMS)
	}
	none("no duration")
	writeWAV(t, filepath.Join(e.dirOf(l), model.AudioFile), durMS)
	st, _ = asrView("duration back")
	check("duration back", st, want)

	// Running again: the job's own progress, no checkpoint.
	e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusRunning)
	if st, raw := asrView("running"); st.Status != model.StatusRunning || st.Checkpoint != nil || strings.Contains(raw, `"checkpoint"`) {
		t.Errorf("running: status %s checkpoint %+v", st.Status, st.Checkpoint)
	}
}

// A lecture whose asr is queued the normal way (not started yet, no power
// interrupt) has no checkpoint field at all.
func TestASRNormalQueuedNoCheckpoint(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	writeWAV(t, filepath.Join(e.dirOf(l), model.AudioFile), 5000)
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	e.moveJob(model.ScopeLecture, l.ID, model.StageASR, model.StatusQueued)
	res, raw := e.do("GET", "/api/lectures/"+l.ID, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, raw)
	}
	var lv Lecture
	if err := json.Unmarshal(raw, &lv); err != nil {
		t.Fatal(err)
	}
	if st := stageOf(lv.Stages, model.StageASR); st.Status != model.StatusQueued || st.Checkpoint != nil || strings.Contains(string(raw), `"checkpoint"`) {
		t.Fatalf("normal queued asr: status %s checkpoint %+v", st.Status, st.Checkpoint)
	}
}
