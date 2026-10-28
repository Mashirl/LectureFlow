// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Batch rerun of one stage over lectures chosen on the course page (user
// request 2026-10-27, course-ux): "多选单一课程内的全部内容批量重跑某个阶段".
// Each lecture goes through Rerun, the path of the single-lecture rerun
// (transition, cleanup, snapshots and the A4.3 rules unchanged); like
// UpdateLectures (A2.3 "课程级批量更新时只询问一次") the model question is
// asked once for all of them.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Reasons a lecture cannot rerun a stage now (RerunBlock.Reason): the
// preconditions Rerun checks (state.RerunWith's, mirrored by rerunnable).
const (
	RerunBlockDraft  = "draft"  // not started yet (v1.1 A3): launch it instead
	RerunBlockStatus = "status" // the stage is not done or stale (Status says what it is)
	RerunBlockBusy   = "busy"   // the stage or a later one is running (Stage says which)
)

// RerunBlock says why a lecture is skipped by RerunLectures.
type RerunBlock struct {
	Reason string // RerunBlock*
	Stage  string // RerunBlockStatus: the stage asked for; RerunBlockBusy: the running stage
	Status string // RerunBlockStatus: its status ("" when it has no current job)
}

// RerunResult is one lecture's outcome of RerunLectures / PlanRerun.
type RerunResult struct {
	LectureID string
	// Block: skipped, nothing changed. Err: the rerun failed (or, in a
	// plan, would fail), e.g. db.ErrNotFound. Neither: rerun (planned).
	Block *RerunBlock
	Err   error
}

// rerunBlock is why lecture l with its current jobs cannot rerun stage
// now; nil when it can (the checks of Rerun: a draft, then rerunnable).
func rerunBlock(l model.Lecture, jobs []model.Job, stage string) *RerunBlock {
	if state.IsDraft(l) {
		return &RerunBlock{Reason: RerunBlockDraft}
	}
	self := findJob(jobs, stage)
	if self == nil {
		return &RerunBlock{Reason: RerunBlockStatus, Stage: stage}
	}
	if self.Status != model.StatusDone && self.Status != model.StatusStale {
		return &RerunBlock{Reason: RerunBlockStatus, Stage: stage, Status: self.Status}
	}
	for _, st := range withDownstream(stage) {
		if j := findJob(jobs, st); j != nil && j.Status == model.StatusRunning {
			return &RerunBlock{Reason: RerunBlockBusy, Stage: st}
		}
	}
	return nil
}

// uniqueIDs drops repeated ids, keeping the first occurrence.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// PlanRerun answers what RerunLectures(ids, stage, "") would do, changing
// nothing (the batch rerun's one confirmation): per lecture (repeated ids
// once, in order) its RerunBlock, or Err when its rerun would fail (an
// unknown lecture: db.ErrNotFound; settings that cannot give a model), or
// neither; and the model differences of the lectures that would be rerun
// (state.ModelDiff with LectureID; the diff of *state.NeedsModelChoiceError).
// stage must be a pipeline stage of a lecture.
func (s *Scheduler) PlanRerun(ctx context.Context, ids []string, stage string) ([]RerunResult, []state.ModelDiff, error) {
	if !slices.Contains(model.LectureStages, stage) {
		return nil, nil, fmt.Errorf("%w: %q is not a stage of the lecture pipeline", state.ErrBadInput, stage)
	}
	ids = uniqueIDs(ids)
	out := make([]RerunResult, 0, len(ids))
	var diffs []state.ModelDiff
	for _, id := range ids {
		r := RerunResult{LectureID: id}
		l, err := s.st.GetLecture(ctx, id)
		if err != nil {
			if !errors.Is(err, db.ErrNotFound) {
				return nil, nil, err
			}
			r.Err = err
			out = append(out, r)
			continue
		}
		jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, id)
		if err != nil {
			return nil, nil, err
		}
		if r.Block = rerunBlock(l, jobs, stage); r.Block == nil {
			var nm *state.NeedsModelChoiceError
			switch _, err := s.rerunSnapshots(id, jobs, stage, ""); {
			case errors.As(err, &nm):
				diffs = append(diffs, nm.Diffs...)
			case err != nil:
				r.Err = err
			}
		}
		out = append(out, r)
	}
	return out, diffs, nil
}

// RerunLectures reruns stage of several lectures with ONE A4.3 decision.
// Lectures that cannot rerun it now (a draft, the stage not done or stale,
// it or a later stage running) are skipped with their RerunBlock and left
// unchanged. With choice "" the model differences of the others are
// collected first (PlanRerun); if there are any it returns one
// *state.NeedsModelChoiceError listing all of them and changes nothing.
// Otherwise every other lecture goes through Rerun(ScopeLecture, id, stage,
// choice) — the single-lecture rerun — and its error, if any, is its Err.
// Results follow ids (repeated ids once).
func (s *Scheduler) RerunLectures(ctx context.Context, ids []string, stage, choice string) ([]RerunResult, error) {
	if err := checkChoice(choice); err != nil {
		return nil, err
	}
	plan, diffs, err := s.PlanRerun(ctx, ids, stage)
	if err != nil {
		return nil, err
	}
	if choice == "" && len(diffs) > 0 {
		return nil, &state.NeedsModelChoiceError{Diffs: diffs}
	}
	out := make([]RerunResult, 0, len(plan))
	for _, p := range plan {
		r := RerunResult{LectureID: p.LectureID, Block: p.Block}
		switch {
		case p.Block != nil:
		case errors.Is(p.Err, db.ErrNotFound):
			r.Err = p.Err
		default:
			// a planned failure is tried all the same: Rerun reports it (or
			// succeeds when the choice made it possible)
			r.Err = s.Rerun(ctx, model.ScopeLecture, p.LectureID, stage, choice)
		}
		out = append(out, r)
	}
	return out, nil
}
