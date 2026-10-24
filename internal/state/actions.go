// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"os"
	"path/filepath"

	"lectureflow/internal/model"
)

// StagesOf returns the stages of a scope in dependency order.
func StagesOf(scope string) []string {
	switch scope {
	case model.ScopeLecture:
		return model.LectureStages
	case model.ScopeMaterial:
		return model.MaterialStages
	case model.ScopeCourse:
		return []string{model.StageGlossarySeed}
	}
	return nil
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// Downstream returns the stages after stage in its scope, in order.
func Downstream(stage string) []string {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	if i < 0 {
		return nil
	}
	return append([]string(nil), order[i+1:]...)
}

// Upstream returns the stage immediately before stage in its scope, or "".
func Upstream(stage string) string {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	if i <= 0 {
		return ""
	}
	return order[i-1]
}

// ActionOpts carries the context-dependent rules on top of the SPEC §4.5 table.
type ActionOpts struct {
	// MediaDeleted: storage.keep_original_media = false and the original
	// media is gone, so prep offers no rerun (SPEC §17.5).
	MediaDeleted bool
	// DownstreamRunning: a later stage of the same scope is running. Rerun
	// would have to replace it, so rerun is not offered (it would be 409).
	DownstreamRunning bool
}

// AllowedActions implements the SPEC §4.5 table. The result is never nil.
// An auxiliary task (v2) has AuxActions instead: retry and cancel, no rerun.
func AllowedActions(j model.Job, o ActionOpts) []string {
	if model.IsAuxStage(j.Stage) {
		return AuxActions(j)
	}
	switch j.Status {
	case model.StatusQueued, model.StatusRunning:
		return []string{model.ActionCancel}
	case model.StatusDone, model.StatusStale:
		if o.DownstreamRunning || (j.Stage == model.StagePrep && o.MediaDeleted) {
			return []string{}
		}
		return []string{model.ActionRerun}
	case model.StatusFailed, model.StatusCancelled:
		return []string{model.ActionRetry}
	}
	return []string{}
}

// StageActions computes AllowedActions for every current job of one scope
// (a lecture's six jobs, a material's two, a course's glossary_seed), and
// AuxActions for its auxiliary tasks (v2). A running auxiliary task never
// blocks the rerun of a stage. mediaDeleted only affects prep. Keyed by stage.
func StageActions(jobs []model.Job, mediaDeleted bool) map[string][]string {
	running := map[string]bool{}
	for _, j := range jobs {
		if j.Status == model.StatusRunning {
			running[j.Stage] = true
		}
	}
	out := make(map[string][]string, len(jobs))
	for _, j := range jobs {
		o := ActionOpts{MediaDeleted: mediaDeleted}
		for _, d := range Downstream(j.Stage) {
			if running[d] {
				o.DownstreamRunning = true
			}
		}
		out[j.Stage] = AllowedActions(j, o)
	}
	return out
}

// CanDelete: a lecture (or its media) may be deleted/replaced only while no
// stage is running (SPEC §4.5). Auxiliary tasks (v2) do not count: deleting
// the lecture cancels them first (sched.DeleteLecture).
func CanDelete(jobs []model.Job) bool {
	for _, j := range jobs {
		if j.Status == model.StatusRunning && !model.IsAuxStage(j.Stage) {
			return false
		}
	}
	return true
}

// Reviewable: review operations are refused (409) while repair or translate
// is running (SPEC §4.5, §9.7).
func Reviewable(jobs []model.Job) bool {
	for _, j := range jobs {
		if j.Status == model.StatusRunning && (j.Stage == model.StageRepair || j.Stage == model.StageTranslate) {
			return false
		}
	}
	return true
}

// MediaExists reports whether the lecture's original media file exists.
// media_path may be absolute, relative to the working directory, or
// relative to the lecture directory.
func MediaExists(dataDir string, l model.Lecture) bool {
	if l.MediaPath == "" {
		return false
	}
	if _, err := os.Stat(l.MediaPath); err == nil {
		return true
	}
	if filepath.IsAbs(l.MediaPath) {
		return false
	}
	_, err := os.Stat(filepath.Join(model.LectureDir(dataDir, l.CourseID, l.ID), l.MediaPath))
	return err == nil
}
