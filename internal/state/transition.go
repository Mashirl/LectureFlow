// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package state is the job state machine of SPEC §4: the single transition
// function, allowed actions, rerun/retry/cancel, invalidation, dependency
// checks and startup recovery. Pure logic plus transactions; it depends only
// on db and model (never on sched).
package state

import (
	"context"
	"errors"
	"fmt"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

var (
	// ErrIllegalTransition: the (from, to) pair is not in the SPEC §4.5 table,
	// or the compare-and-set matched no row.
	ErrIllegalTransition = errors.New("illegal state transition")
	// ErrConflict: the job is not in the expected state (it changed
	// concurrently, is not current, or the operation does not apply to its
	// status). The API maps it to 409.
	ErrConflict = errors.New("conflict")
	// ErrBusy: a stage that the operation would replace is running.
	// errors.Is(ErrBusy, ErrConflict) is true.
	ErrBusy = fmt.Errorf("a stage is running, try again later: %w", ErrConflict)
	// ErrMediaMissing: prep cannot be rerun because the original media file
	// is gone (storage.keep_original_media = false, SPEC §17.5).
	ErrMediaMissing = errors.New("original media file was deleted; prep cannot be rerun")
)

// legal is the hard-coded transition table of SPEC §4.5.
// "done|stale → (new job) pending" is not a transition: see Rerun.
var legal = map[[2]string]bool{
	{model.StatusPending, model.StatusQueued}:    true, // scheduler: deps satisfied
	{model.StatusQueued, model.StatusRunning}:    true, // scheduler: resources available
	{model.StatusQueued, model.StatusCancelled}:  true, // user
	{model.StatusRunning, model.StatusDone}:      true,
	{model.StatusRunning, model.StatusFailed}:    true,
	{model.StatusRunning, model.StatusCancelled}: true,
	{model.StatusFailed, model.StatusQueued}:     true, // auto retry or user retry
	{model.StatusCancelled, model.StatusQueued}:  true, // user retry
	{model.StatusDone, model.StatusStale}:        true, // invalidation
}

// Legal reports whether from → to is in the SPEC §4.5 table.
func Legal(from, to string) bool { return legal[[2]string{from, to}] }

// casError is returned when the compare-and-set matched no row. It is both
// an ErrIllegalTransition (SPEC §4.5: 0 rows = illegal) and an ErrConflict.
type casError struct{ msg string }

func (e *casError) Error() string { return e.msg }
func (e *casError) Is(target error) bool {
	return target == ErrIllegalTransition || target == ErrConflict
}

// Transition moves the current job of (scope, scopeID, stage) from → to
// (SPEC §4.5 transition()). See TransitionJob for the semantics.
func Transition(ctx context.Context, st db.Store, scope, scopeID, stage, from, to string, f db.JobFinish) error {
	if !Legal(from, to) {
		return fmt.Errorf("%w: %s %s→%s", ErrIllegalTransition, stage, from, to)
	}
	return st.Tx(ctx, func(tx db.Store) error {
		j, err := tx.GetCurrentJob(ctx, scope, scopeID, stage)
		if errors.Is(err, db.ErrNotFound) {
			return &casError{fmt.Sprintf("no current %s job for %s %s", stage, scope, scopeID)}
		}
		if err != nil {
			return err
		}
		return TransitionJob(ctx, tx, j.ID, from, to, f)
	})
}

// TransitionJob is the only caller of Store.CASJobStatus: UPDATE job SET
// status = to WHERE id = jobID AND status = from AND is_current = 1.
//
// Illegal pairs return ErrIllegalTransition without touching the database;
// a CAS that matches no row returns an error that is both
// ErrIllegalTransition and ErrConflict.
//
// Timestamps are filled in when f leaves them nil: → running sets
// started_at = now and clears finished_at; → done|failed|cancelled sets
// finished_at = now; → queued clears started_at and finished_at.
func TransitionJob(ctx context.Context, st db.Store, jobID, from, to string, f db.JobFinish) error {
	if !Legal(from, to) {
		return fmt.Errorf("%w: job %s %s→%s", ErrIllegalTransition, jobID, from, to)
	}
	now := db.Now()
	empty := ""
	switch to {
	case model.StatusRunning:
		if f.StartedAt == nil {
			f.StartedAt = &now
		}
		if f.FinishedAt == nil {
			f.FinishedAt = &empty
		}
	case model.StatusDone, model.StatusFailed, model.StatusCancelled:
		if f.FinishedAt == nil {
			f.FinishedAt = &now
		}
	case model.StatusQueued:
		if f.StartedAt == nil {
			f.StartedAt = &empty
		}
		if f.FinishedAt == nil {
			f.FinishedAt = &empty
		}
	}
	ok, err := st.CASJobStatus(ctx, jobID, from, to, f)
	if err != nil {
		return err
	}
	if !ok {
		return &casError{fmt.Sprintf("job %s: %s→%s matched no current row", jobID, from, to)}
	}
	return nil
}
