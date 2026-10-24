// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// StageView.checkpoint (power management; real-machine check,
// docs/benchmarks/power-checkpoint.md §6.8 item 2). When the power policy
// suspends or migrates an asr run, the scheduler puts the job back in line
// (queued, progress 0, job.stats.asr_resume) while the worker's checkpoint
// already holds part of the lecture — 38.8 % in the check — and the next
// attempt continues from there. Showing 0 % until then is a state that does
// not match reality (SPEC §0.1), so the view of such a queued stage carries
// the checkpoint's position: resume_ms over the lecture's duration_ms.
//
// The scheduler never reads the checkpoint file (devices/checkpoint.go);
// the view reads only its header fields, through a cache checked against
// the file's size and modification time (views are built for every list
// and SSE refresh, and a queued job's checkpoint does not change).

// checkpointHeads caches checkpoint headers by path.
var checkpointHeads sync.Map // path → checkpointHead

type checkpointHead struct {
	size     int64
	mod      time.Time
	jobID    string
	resumeMS int64
	ok       bool
}

// checkpointFile is the part of devices.Checkpoint a view needs (the
// segments are skipped; only the number of parts matters).
type checkpointFile struct {
	Version  int               `json:"version"`
	JobID    string            `json:"job_id"`
	ResumeMS int64             `json:"resume_ms"`
	Parts    []json.RawMessage `json:"parts"`
}

// cachedCheckpoint returns the job id and resume_ms of the checkpoint at
// path. ok is false when there is none, or when the worker would not
// continue from it whatever the job: another format version, no parts
// (asr.go checkpointResumable) or nothing transcribed yet.
func cachedCheckpoint(path string) (jobID string, resumeMS int64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return "", 0, false
	}
	if v, found := checkpointHeads.Load(path); found {
		if e := v.(checkpointHead); e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
			return e.jobID, e.resumeMS, e.ok
		}
	}
	e := checkpointHead{size: fi.Size(), mod: fi.ModTime()}
	if b, err := os.ReadFile(path); err == nil {
		var f checkpointFile
		if json.Unmarshal(b, &f) == nil && f.Version == devices.CheckpointVersion && len(f.Parts) > 0 && f.ResumeMS > 0 {
			e.jobID, e.resumeMS, e.ok = f.JobID, f.ResumeMS, true
		}
	}
	checkpointHeads.Store(path, e)
	return e.jobID, e.resumeMS, e.ok
}

// asrCheckpointView is the StageView.checkpoint of the lecture's asr job
// among jobs: only while it is queued after a power interrupt
// (stats.asr_resume) with a checkpoint of this very job (a rerun is a new
// job, whose worker voids an older file) and a known duration (durationMS,
// Lecture.duration_ms). nil otherwise — a normal queued stage is shown as
// before.
func (s *Server) asrCheckpointView(l model.Lecture, jobs []model.Job, durationMS *int64) *ASRCheckpointView {
	j, found := findJob(jobs, model.StageASR)
	if !found || j.Status != model.StatusQueued || j.Stats.ASRResume == nil || durationMS == nil || *durationMS <= 0 {
		return nil
	}
	id, at, ok := cachedCheckpoint(filepath.Join(s.lectureDir(l), model.ASRCheckpointFile))
	if !ok || id != j.ID {
		return nil
	}
	p := float64(at) / float64(*durationMS)
	if p > 1 {
		p = 1
	}
	return &ASRCheckpointView{ResumeMS: at, DurationMS: *durationMS, Progress: p}
}
