// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
	"log/slog"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Hasher has the shape of sched.Stage.InputHash (state must not import sched).
// Hashers are keyed by stage name.
type Hasher interface {
	InputHash(ctx context.Context, scopeID string) (string, error)
}

// HasherFunc adapts a function to Hasher.
type HasherFunc func(ctx context.Context, scopeID string) (string, error)

func (f HasherFunc) InputHash(ctx context.Context, scopeID string) (string, error) {
	return f(ctx, scopeID)
}

// HashError wraps a failure of Hasher.InputHash (as opposed to a database error).
type HashError struct {
	Stage string
	Err   error
}

func (e *HashError) Error() string { return "input hash of " + e.Stage + ": " + e.Err.Error() }
func (e *HashError) Unwrap() error { return e.Err }

// reasonFunc gives the stale_reason of a job that becomes stale (v1.1 A2.2).
type reasonFunc func(j model.Job) string

// refresher rewrites the glossary reason of jobs that are already stale for
// the glossary when the glossary changes again (NOTES "v1.1 冻结与裁决" 9:
// "v1 → v4" after later edits). A janitor run or undo (v2 StaleJanitor)
// refreshes like an edit does. The reason keeps its own code
// (glossary_updated or glossary_janitor): only the version params follow
// (v2 lead ruling 11; phase 2 ruling 5: compare codes, update params).
// nil: reasons are only written with done → stale (other causes never
// overwrite a reason).
type refresher struct {
	courseID string
}

// wants: a stale job of a stage the glossary feeds whose reason is a
// glossary reason.
func (rf *refresher) wants(j model.Job) bool {
	return rf != nil && j.Status == model.StatusStale && j.StaleReason != nil && isGlossaryReason(*j.StaleReason) &&
		causeCovers(StaleGlossary, j.Stage)
}

// refreshStaleReason rewrites the reason of a stale job whose input still
// differs from its recorded hash (a job whose input went back to what it ran
// with keeps its reason: it is still stale, nothing to announce). The
// course's glossary_version is read in the same transaction as the write,
// so two checks running concurrently can never move a reason back to an
// older version (versions only grow). Returns the reason written.
func refreshStaleReason(ctx context.Context, st db.Store, h Hasher, j model.Job, rf *refresher) (string, bool, error) {
	if h == nil || j.InputHash == nil || *j.InputHash == "" {
		return "", false, nil
	}
	cur, err := h.InputHash(ctx, j.ScopeID)
	if err != nil {
		return "", false, &HashError{Stage: j.Stage, Err: err}
	}
	if cur == "" || cur == *j.InputHash {
		return "", false, nil
	}
	var reason string
	var ok bool
	err = st.Tx(ctx, func(tx db.Store) error {
		c, err := tx.GetCourse(ctx, rf.courseID)
		if err != nil {
			return err
		}
		reason = refreshedReason(*j.StaleReason, j, c.GlossaryVersion)
		ok, err = tx.SetStaleReason(ctx, j.ID, reason)
		return err
	})
	return reason, ok, err
}

// refreshedReason is the glossary reason old (of stale job j; JSON, or a
// text not converted yet) as JSON with its version params brought up to the
// course's current glossaryVersion; the code of old is kept:
// glossary_janitor {from 3, to 4} becomes glossary_janitor {from 3, to 5}.
// from is the job's recorded glossary_version (absent when not recorded).
func refreshedReason(old string, j model.Job, glossaryVersion int) string {
	code := model.StaleCodeGlossary
	if parseReason(old).Code == model.StaleCodeJanitor {
		code = model.StaleCodeJanitor
	}
	return model.StaleReasonJSON(code, versionParams(j.Stats.GlossaryVersion, glossaryVersion))
}

// noCause is the reason of a check without a cause: input_changed.
func noCause(j model.Job) string { return StaleReason(StaleCause{}, j, 0) }

// CheckStaleJob recomputes the input hash of one job and marks it stale
// (done → stale) when it differs from the recorded job.input_hash (SPEC §4.4).
// j is the caller's current snapshot of the job. Nothing happens (false, nil)
// unless j is done, has a recorded hash and h returns a non-empty hash.
// h is called outside any transaction, so this must not run inside Store.Tx.
// The job gets stale_reason input_changed (no cause, v1.1 A2.2; JSON since
// P14.3). An auxiliary task never becomes stale (P13.5).
func CheckStaleJob(ctx context.Context, st db.Store, h Hasher, j model.Job) (bool, error) {
	return checkStaleJob(ctx, st, h, j, noCause(j))
}

// checkStaleJob is CheckStaleJob writing reason as stale_reason in the same
// done → stale transition.
func checkStaleJob(ctx context.Context, st db.Store, h Hasher, j model.Job, reason string) (bool, error) {
	if h == nil || j.Status != model.StatusDone || j.InputHash == nil || *j.InputHash == "" || model.IsAuxStage(j.Stage) {
		return false, nil
	}
	cur, err := h.InputHash(ctx, j.ScopeID)
	if err != nil {
		return false, &HashError{Stage: j.Stage, Err: err}
	}
	if cur == "" || cur == *j.InputHash {
		return false, nil
	}
	if err := TransitionJob(ctx, st, j.ID, model.StatusDone, model.StatusStale, db.JobFinish{StaleReason: &reason}); err != nil {
		if errors.Is(err, ErrConflict) {
			return false, nil // changed concurrently (e.g. rerun); nothing to mark
		}
		return false, err
	}
	return true, nil
}

// CheckStaleScope runs CheckStaleJob for every current job of one scope.
// It never reruns anything. Stages without a hasher, stages whose hasher
// returns "" and jobs without a recorded hash are skipped; a hasher error is
// logged and that stage skipped.
//
// Hashers are called outside any transaction (they read through their own
// Store), so CheckStaleScope must not be called inside Store.Tx.
// Returns the jobs that became stale (Status and StaleReason updated).
// The reason written is input_changed ("输入已变化", no cause).
func CheckStaleScope(ctx context.Context, st db.Store, hashers map[string]Hasher, scope, scopeID string) ([]model.Job, error) {
	return checkStaleScope(ctx, st, hashers, scope, scopeID, noCause, nil)
}

func checkStaleScope(ctx context.Context, st db.Store, hashers map[string]Hasher, scope, scopeID string, reason reasonFunc, rf *refresher) ([]model.Job, error) {
	changed, _, err := checkScope(ctx, st, hashers, scope, scopeID, reason, rf)
	return changed, err
}

// checkScope is checkStaleScope also reporting whether a hasher failed for
// some stage (that stage was skipped and logged).
func checkScope(ctx context.Context, st db.Store, hashers map[string]Hasher, scope, scopeID string, reason reasonFunc, rf *refresher) (changed []model.Job, hashFailed bool, err error) {
	jobs, err := st.ListCurrentJobs(ctx, scope, scopeID)
	if err != nil {
		return nil, false, err
	}
	for _, j := range jobs {
		if model.IsAuxStage(j.Stage) {
			continue // auxiliary tasks never take part in the invalidation (P13.5)
		}
		if rf.wants(j) {
			r, ok, err := refreshStaleReason(ctx, st, hashers[j.Stage], j, rf)
			if err != nil {
				if ctx.Err() != nil {
					return changed, hashFailed, ctx.Err()
				}
				var he *HashError
				if !errors.As(err, &he) {
					return changed, hashFailed, err
				}
				slog.Warn("invalidation: input hash failed", "scope", scope, "id", scopeID, "stage", j.Stage, "err", err)
				hashFailed = true
				continue
			}
			if ok {
				j.StaleReason = &r
				changed = append(changed, j)
			}
			continue
		}
		r := reason(j)
		stale, err := checkStaleJob(ctx, st, hashers[j.Stage], j, r)
		if err != nil {
			if ctx.Err() != nil {
				return changed, hashFailed, ctx.Err()
			}
			var he *HashError
			if !errors.As(err, &he) {
				return changed, hashFailed, err
			}
			slog.Warn("invalidation: input hash failed", "scope", scope, "id", scopeID, "stage", j.Stage, "err", err)
			hashFailed = true
			continue
		}
		if stale {
			j.Status = model.StatusStale
			j.StaleReason = &r
			changed = append(changed, j)
		}
	}
	return changed, hashFailed, nil
}

// CheckStale runs the invalidation check for one lecture (no cause:
// stale_reason input_changed). CheckStale(...) is CheckStaleWith(..., StaleCause{}).
func CheckStale(ctx context.Context, st db.Store, hashers map[string]Hasher, lectureID string) ([]model.Job, error) {
	return CheckStaleWith(ctx, st, hashers, lectureID, StaleCause{})
}

// InvalidateCourse runs the invalidation check for every lecture, material
// and the course-level jobs of a course (after a glossary change, material
// upload/deletion, SPEC §4.6, §6.0.1), without a cause (stale_reason
// input_changed). InvalidateCourse(...) is InvalidateCourseWith(..., StaleCause{}).
func InvalidateCourse(ctx context.Context, st db.Store, hashers map[string]Hasher, courseID string) ([]model.Job, error) {
	return InvalidateCourseWith(ctx, st, hashers, courseID, StaleCause{})
}

// causeReason builds the reasonFunc of cause for a course. Only a glossary
// or janitor cause needs the course's current glossary_version (read once,
// after the caller's glossary change has been committed).
func causeReason(ctx context.Context, st db.Store, courseID string, cause StaleCause) (reasonFunc, error) {
	gv := 0
	if isGlossaryKind(cause.Kind) {
		c, err := st.GetCourse(ctx, courseID)
		if err != nil {
			return nil, err
		}
		gv = c.GlossaryVersion
	}
	return func(j model.Job) string { return StaleReason(cause, j, gv) }, nil
}

// invalidateCourse is the body of InvalidateCourseWith.
func invalidateCourse(ctx context.Context, st db.Store, hashers map[string]Hasher, courseID string, reason reasonFunc, rf *refresher) ([]model.Job, error) {
	var changed []model.Job
	lecs, err := st.ListLectures(ctx, courseID)
	if err != nil {
		return nil, err
	}
	for _, l := range lecs {
		c, err := checkStaleScope(ctx, st, hashers, model.ScopeLecture, l.ID, reason, rf)
		changed = append(changed, c...)
		if err != nil {
			return changed, err
		}
	}
	mats, err := st.ListMaterials(ctx, courseID)
	if err != nil {
		return changed, err
	}
	for _, m := range mats {
		c, err := checkStaleScope(ctx, st, hashers, model.ScopeMaterial, m.ID, reason, rf)
		changed = append(changed, c...)
		if err != nil {
			return changed, err
		}
	}
	c, err := checkStaleScope(ctx, st, hashers, model.ScopeCourse, courseID, reason, rf)
	return append(changed, c...), err
}
