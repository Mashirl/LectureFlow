// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import "lectureflow/internal/model"

// Course page requests of 2026-10-27 (course-ux; NOTES.md): batch rerun of
// one stage, teaching materials hung under a week, make-up lectures ("补").

// WeekOverride is Lecture.week_override: the user placed the lecture in a
// teaching week (model.WeekOverride). Absent when it has none.
type WeekOverride struct {
	Week int    `json:"week"`
	Kind string `json:"kind"` // WeekOverrideSupplement | WeekOverrideMakeup
	// Applied: the course has a term start, so the lecture is numbered and
	// grouped in Week and uses that week's slides. Without one the override
	// is kept but changes nothing.
	Applied   bool   `json:"applied"`
	CreatedAt string `json:"created_at"`
}

// Week override kinds (model.WeekOverride*).
const (
	WeekOverrideSupplement = model.WeekOverrideSupplement
	WeekOverrideMakeup     = model.WeekOverrideMakeup
)

// WeekOverrideSet is the body of PUT /api/lectures/{id}/week. Confirm: the
// renumbering of P13.6 was confirmed (409 calendar.renumber otherwise, when
// the course's citations were exported and numbers change).
type WeekOverrideSet struct {
	Week    int    `json:"week"`
	Kind    string `json:"kind"`
	Confirm bool   `json:"confirm,omitempty"`
}

// CourseRerunRequest is the body of POST /api/courses/{id}/rerun: rerun
// Stage (a lecture pipeline stage) of the lectures LectureIDs of the
// course, with one model decision (ModelChoice, v1.1 A4.3; "" = ask:
// 409 needs_model_choice listing every lecture's differences). DryRun:
// only say what would happen (CourseRerunResult.Diff holds the question).
type CourseRerunRequest struct {
	Stage       string   `json:"stage"`
	LectureIDs  []string `json:"lecture_ids"`
	ModelChoice string   `json:"model_choice,omitempty"`
	DryRun      bool     `json:"dry_run,omitempty"`
}

// Results of one lecture of a batch rerun (CourseRerunItem.Result).
const (
	RerunQueued  = "queued"  // rerun (dry run: would be): the stage and those after it run again
	RerunSkipped = "skipped" // cannot rerun the stage now; nothing changed (Reason says why)
	RerunFailed  = "failed"  // the rerun failed (dry run: would fail); Reason is the error
)

// CourseRerunItem is one lecture's outcome.
type CourseRerunItem struct {
	LectureID string           `json:"lecture_id"`
	Result    string           `json:"result"`            // Rerun*
	Reason    *model.ErrorInfo `json:"reason,omitempty"`  // skipped / failed: rerun.* or the error's code
	Lecture   *Lecture         `json:"lecture,omitempty"` // the updated lecture after a rerun (not in a dry run)
}

// CourseRerunResult is the response of POST /api/courses/{id}/rerun.
type CourseRerunResult struct {
	Stage   string            `json:"stage"`
	DryRun  bool              `json:"dry_run,omitempty"`
	Results []CourseRerunItem `json:"results"` // in request order, repeated ids once
	Queued  int               `json:"queued"`
	Skipped int               `json:"skipped"`
	Failed  int               `json:"failed"`
	// Diff (dry run): the model differences of the lectures that would be
	// rerun, as in the 409 needs_model_choice body; empty = no question.
	Diff []ModelDiffView `json:"diff"`
}
