// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// "修复断句" (textrepair-int; NOTES "决定（textrepair-int：worker-builder…）"):
// a lecture transcribed before the statistical text repair existed gets it
// without being transcribed again. The asr rerun with text_only keeps the
// sentences, reruns everything after asr (§4.6) and queues an asr job with
// model.ASRModeTextRepair, which runs worker/textrepair.py on the stored
// words: in the CPU pool (resourceOf), never on a transcription device.

// RerunTextRepair is the asr rerun with text_only of one lecture. The A4.3
// decision covers the stages after asr only (summary, repair, translate,
// export: they run again with their snapshots, or the current settings on
// model.ModelChoiceCurrent); the asr job keeps its snapshot, since nothing
// is transcribed. The preconditions are those of Rerun (done or stale asr,
// nothing after it running, not a draft). Holds the scan lock like Rerun.
func (s *Scheduler) RerunTextRepair(ctx context.Context, lectureID, choice string) error {
	if err := checkChoice(choice); err != nil {
		return err
	}
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if state.IsDraft(l) {
		return state.ErrDraft
	}
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, lectureID)
	if err != nil {
		return err
	}
	if err := rerunnable(jobs, model.StageASR); err != nil {
		return err
	}
	o := state.RerunOpts{TextRepair: true}
	if down := state.Downstream(model.StageASR); len(down) > 0 {
		if o.NewSnapshots, err = s.rerunSnapshots(lectureID, jobs, down[0], choice); err != nil {
			return err
		}
	}
	s.scanMu.Lock()
	err = state.RerunWith(ctx, s.st, s.dataDir, model.ScopeLecture, lectureID, model.StageASR, o)
	s.scanMu.Unlock()
	if err != nil {
		return err
	}
	s.ev.Changed("sentences", lectureID) // repaired text reset, translations cleared
	s.ev.Changed("patches", lectureID)
	s.emitScope(ctx, model.ScopeLecture, lectureID)
	s.Kick()
	return nil
}

// resourceOf is the pool a job runs in: its stage's, except a "修复断句" asr
// job (model.IsTextRepairJob), which runs worker/textrepair.py on the CPU
// and needs no transcription device.
func resourceOf(stg Stage, j model.Job) ResourceKind {
	if model.IsTextRepairJob(j) {
		return ResourceCPU
	}
	return stg.Resource()
}
