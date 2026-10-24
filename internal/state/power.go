// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"fmt"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Power management (v2-step20 freeze, .claude/power-brief.md §3.4). FROZEN
// signatures. The state machine of §4 is unchanged: a power interrupt is the
// §4.7 pattern (running → failed → queued in one transaction, "failed"
// never committed on its own).

// PowerRequeue records a run the power policy interrupted (migrate or
// suspend): running → failed → queued on the same job in ONE transaction,
// attempt+1, progress 0, job.error cleared (it is not a failure: the
// waiting_for of the queued job explains it). There is no retry limit —
// the scheduler has already counted the interrupt in
// job.stats.power_interrupts (model.CountedAttempts). ErrConflict when the
// job is not the current running job; nothing is written then.
func PowerRequeue(ctx context.Context, st db.Store, jobID string) error {
	return st.Tx(ctx, func(tx db.Store) error {
		empty := ""
		if err := TransitionJob(ctx, tx, jobID, model.StatusRunning, model.StatusFailed, db.JobFinish{Error: &empty}); err != nil {
			return err
		}
		j, err := tx.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		return requeue(ctx, tx, j, true)
	})
}

// RecordASRParts stores the parts of a resumed asr run in the job's
// snapshot (model.ASRSnapshot.Parts): the asr stage calls it when its result
// has two parts or more, in the transaction that writes the sentences. Every
// other field of the snapshot is kept as stored. Fewer than two parts: no
// change (an uninterrupted run keeps its snapshot byte for byte).
// ErrConflict when the job is not a current asr job, is not running, or its
// snapshot cannot be parsed; a missing job is the Store's error.
func RecordASRParts(ctx context.Context, st db.Store, jobID string, parts []model.ASRPart) error {
	if len(parts) < 2 {
		return nil
	}
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if !j.IsCurrent || j.Stage != model.StageASR || j.Status != model.StatusRunning {
		return fmt.Errorf("%w: job %s is not a running current asr job", ErrConflict, jobID)
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok {
		return fmt.Errorf("%w: asr job %s has no valid snapshot", ErrConflict, jobID)
	}
	snap.Parts = append([]model.ASRPart(nil), parts...)
	return st.SetJobSnapshot(ctx, jobID, model.MarshalSnapshot(snap))
}
