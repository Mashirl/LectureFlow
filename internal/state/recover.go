// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Recover performs the startup recovery of SPEC §4.7. Call it before the
// scheduler starts:
//  1. every running current job → failed (error "interrupted by restart",
//     stored as that text: the content §4 defines; ParseJobError reads it
//     as the code CodeInterrupted);
//     if attempt < 2 it is requeued (failed → queued, attempt+1) in the same
//     transaction (FailJob; the attempts counted are model.CountedAttempts,
//     so requeues by the power policy do not count), so failed stays a
//     final state; an auxiliary
//     task (v2) is never retried automatically and stays failed;
//  2. queued jobs are left alone;
//  3. *.tmp files under the course directories (lecture, material and
//     syllabus dirs, including export/) are deleted.
func Recover(ctx context.Context, st db.Store, dataDir string) error {
	running, err := st.ListCurrentJobsByStatus(ctx, model.StatusRunning)
	if err != nil {
		return err
	}
	for _, j := range running {
		requeued, err := FailJob(ctx, st, j.ID, interruptedError, !model.IsAuxStage(j.Stage))
		if err != nil {
			return err
		}
		slog.Info("recovered interrupted job", "job", j.ID, "scope", j.Scope, "id", j.ScopeID, "stage", j.Stage, "requeued", requeued)
	}
	return removeTmp(filepath.Join(dataDir, "courses"))
}

// interruptedError is the job.error of §4.7, stored verbatim. The API reads
// it as {code: job.interrupted, detail: the text} (ParseJobError; lead
// ruling at the phase-2 gate: P14.3 does not change what §4 stores).
var interruptedError = model.ErrInterruptedByRestart

func removeTmp(root string) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
