// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// v2 (SPEC-v2.md) state functions. Signatures FROZEN by the lead; bodies
// by core-builder (see .claude/v2-brief.md).

// StaleJanitor is the StaleCause kind of a glossary janitor run or undo
// (G3.1): stale_reason "术语表已整理（vN → vM）" (model.StaleReasonJanitor).
// It covers the same stages as StaleGlossary.
const StaleJanitor = "janitor"

// JanitorConfigured reports whether the glossary_janitor role is usable:
// its role_setting has a provider and a model, and that provider exists and
// has a non-empty api_key (G3.1, appendix D2). Like
// settings.SnapshotUsable, a provider whose base_url is this machine
// (localhost, 127.0.0.0/8, ::1) needs no key (lead, NOTES.md v2 core-builder).
func JanitorConfigured(ctx context.Context, st db.Store) (bool, error) {
	return RoleConfigured(ctx, st, model.RoleGlossaryJanitor)
}

// EnqueueGlossaryReview asks for one glossary_review run of the course
// (G3.1). It does nothing and returns (false, nil) when the janitor is not
// configured. Otherwise, by the status of the course's current
// glossary_review job:
//   - none: insert a pending job;
//   - pending or queued: nothing (already waiting; several triggers merge);
//   - done, stale, failed or cancelled: replace it with a new pending job
//     (like a rerun: the old job is retired, attempt 0, no error);
//   - running: set kv glossary_review_dirty:{course}; the scheduler queues
//     one more run when the running one finishes.
//
// queued is true when a pending job exists afterwards or a rerun was
// recorded. It may be called inside or outside a transaction.
// An unknown course → db.ErrNotFound.
func EnqueueGlossaryReview(ctx context.Context, st db.Store, courseID string) (queued bool, err error) {
	err = st.Tx(ctx, func(tx db.Store) error {
		if _, err := tx.GetCourse(ctx, courseID); err != nil {
			return err
		}
		ok, err := JanitorConfigured(ctx, tx)
		if err != nil || !ok {
			return err
		}
		var cur *model.Job
		j, err := tx.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossaryReview)
		switch {
		case errors.Is(err, db.ErrNotFound):
		case err != nil:
			return err
		default:
			cur = &j
		}
		if cur != nil {
			switch cur.Status {
			case model.StatusPending, model.StatusQueued:
				queued = true
				return nil
			case model.StatusRunning:
				if err := tx.SetKV(ctx, model.KVGlossaryReviewDirtyPrefix+courseID, "1"); err != nil {
					return err
				}
				queued = true
				return nil
			case model.StatusDone, model.StatusStale, model.StatusFailed, model.StatusCancelled:
			default:
				return fmt.Errorf("glossary_review job %s has unknown status %q", cur.ID, cur.Status)
			}
		}
		// Course-level jobs have no snapshot (v1.1 A4.1, lead ruling 2).
		if err := newPending(ctx, tx, model.ScopeCourse, courseID, model.StageGlossaryReview, cur, model.EmptySnapshot, model.JobStats{}); err != nil {
			return err
		}
		queued = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return queued, nil
}

// reviewDeps is Deps for glossary_review (lead ruling 2): it waits for the
// syllabus terms when the course has a syllabus and glossary_seed is
// pending, queued or running → "upstream:glossary_seed", and for the term
// extraction of every material of the course (bound or course-wide) that is
// still in progress → "material_terms:{filename}": material_extract or
// material_terms queued/running, or material_terms pending while its
// material_extract has not failed or been cancelled. An upstream that
// failed, was cancelled or does not exist never holds the review, so it can
// never wait forever.
func reviewDeps(ctx context.Context, st db.Store, courseID string) (bool, string, error) {
	_, err := st.GetSyllabus(ctx, courseID)
	switch {
	case err == nil:
		g, err := st.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossarySeed)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return false, "", err
		}
		if err == nil && inProgress(g.Status) {
			return false, model.WaitUpstream(model.StageGlossarySeed), nil
		}
	case !errors.Is(err, db.ErrNotFound):
		return false, "", err
	}
	mats, err := st.ListMaterials(ctx, courseID)
	if err != nil {
		return false, "", err
	}
	for _, m := range mats {
		jobs, err := st.ListCurrentJobs(ctx, model.ScopeMaterial, m.ID)
		if err != nil {
			return false, "", err
		}
		var extract, terms string
		for _, j := range jobs {
			switch j.Stage {
			case model.StageMaterialExtract:
				extract = j.Status
			case model.StageMaterialTerms:
				terms = j.Status
			}
		}
		running := func(s string) bool { return s == model.StatusQueued || s == model.StatusRunning }
		extractEnded := extract == model.StatusFailed || extract == model.StatusCancelled
		if running(extract) || running(terms) || (terms == model.StatusPending && !extractEnded) {
			return false, model.WaitMaterialTerms(m.Filename), nil
		}
	}
	return true, "", nil
}

// inProgress: pending, queued or running — a job that will still run.
func inProgress(status string) bool {
	return status == model.StatusPending || status == model.StatusQueued || status == model.StatusRunning
}

// reviewHoldsASR reports whether the course's current glossary_review holds
// asr (G3.1, lead ruling 3): only while it is pending, queued or running.
// done, stale, failed, cancelled or no job at all never block (appendix D2:
// a failed janitor leaves the course on the G2 fallback, asr goes on).
func reviewHoldsASR(ctx context.Context, st db.Store, courseID string) (bool, error) {
	j, err := st.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossaryReview)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return inProgress(j.Status), nil
}

// withPromptVersion returns the LLM snapshot raw of stage with
// prompt_version = v (0 = none, i.e. version 1); provider, model and params
// are kept. Snapshots of other stages, and raw that is not a valid LLM
// snapshot, are returned unchanged.
func withPromptVersion(stage, raw string, v int) string {
	if model.PromptVersionOf(stage) == 0 {
		return raw
	}
	s, ok := model.ParseLLMSnapshot(raw)
	if !ok || s.PromptVersion == v {
		return raw
	}
	s.PromptVersion = v
	return model.MarshalSnapshot(s)
}

// janitorReasonPrefix is the fixed part of model.StaleReasonJanitor
// ("术语表已整理"), derived so the text lives in one place.
var janitorReasonPrefix, _, _ = strings.Cut(model.StaleReasonJanitor(nil, 0), "（")

// isGlossaryKind: causes that change the glossary (an edit, a merge, a
// janitor run or undo). They make the same stages stale and both refresh
// the glossary reasons of jobs already stale.
func isGlossaryKind(kind string) bool { return kind == StaleGlossary || kind == StaleJanitor }
