// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
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
//     task (v2) is never retried automatically and stays failed, except
//     the stream task (recoverRequeues);
//  2. queued jobs are left alone;
//  3. *.tmp files under the course directories (lecture, material and
//     syllabus dirs, including export/) are deleted. One that cannot be
//     deleted is logged and skipped (removeTmp): the start goes on;
//  4. (test-chaos 2, beyond §4.7) the directories of deleted lectures that
//     a held file or a kill left behind, marked by the delete with a
//     tombstone, are removed (SweepLectureDirs).
//
// Only the database steps can fail it; the file steps never stop a start.
func Recover(ctx context.Context, st db.Store, dataDir string) error {
	running, err := st.ListCurrentJobsByStatus(ctx, model.StatusRunning)
	if err != nil {
		return err
	}
	for _, j := range running {
		requeued, err := FailJob(ctx, st, j.ID, interruptedError, recoverRequeues(j.Stage))
		if err != nil {
			return err
		}
		slog.Info("recovered interrupted job", "job", j.ID, "scope", j.Scope, "id", j.ScopeID, "stage", j.Stage, "requeued", requeued)
	}
	removeTmp(filepath.Join(dataDir, "courses"))
	SweepLectureDirs(ctx, st, dataDir)
	return nil
}

// recoverRequeues reports whether an interrupted job of stage is requeued
// by §4.7: every pipeline stage, and of the auxiliary tasks only stream
// (lead ruling 2026-10-27 14:30): since stream-parallel the playback file
// is a regular product of the pipeline (prep queues the transcode), so a
// restart continues it instead of waiting for the player to be opened
// (sched.EnsureStream still requeues one whose retries are used up). The
// others (title, chapters, filename_rule) stay failed: no automatic retry.
// A stream task that failed by itself is not running at a restart and is
// left alone, like every failed job.
func recoverRequeues(stage string) bool {
	return !model.IsAuxStage(stage) || stage == model.StageStream
}

// interruptedError is the job.error of §4.7, stored verbatim. The API reads
// it as {code: job.interrupted, detail: the text} (ParseJobError; lead
// ruling at the phase-2 gate: P14.3 does not change what §4 stores).
var interruptedError = model.ErrInterruptedByRestart

// removeTmp deletes the *.tmp files under root (§4.7 step 3). A file that
// cannot be deleted is logged and skipped, and so is a directory that
// cannot be read: before test-chaos 1 such a failure stopped the start, and
// an ffmpeg left running by a LectureFlow that was killed keeps its .tmp
// open (without allowing deletion) until it finishes, for many minutes on a
// long recording. A skipped file is garbage that is never taken for a
// product: audio.wav, stream.mp4 and the export files are written through a
// name no other program holds (stages.tmpFor). The next start tries again.
func removeTmp(root string) {
	held := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("startup recovery: a directory could not be read; its temporary files stay", "path", path, "err", err)
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				held++
				slog.Warn("startup recovery: a temporary file could not be removed (probably still open in another program); skipped",
					"path", path, "err", err)
			}
		}
		return nil
	})
	if held > 0 {
		slog.Warn("startup recovery: temporary files left for the next start", "count", held)
	}
}
