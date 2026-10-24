// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"fmt"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// SPEC-v2 I8 (phase 3b freeze, .claude/v3b-brief.md). FROZEN signature;
// body: task C.

// BindASRDevice records the device an auto-mode asr job runs on (lead
// ruling 1): it rewrites the job's config_snapshot with
// model.ASRDeviceSnapshot(dev, language of the stored snapshot, auto true)
// — device_id, backend, model and precision of dev, language and auto
// kept. The scheduler calls it inside the transaction that moves the job
// queued → running, after TransitionJob and RecordPromptVersion, so a job
// is never running without its device in the snapshot. ErrConflict when
// the job is not a current asr job, is neither queued nor running, or its
// snapshot is not auto (a fixed snapshot is never rebound). It never
// changes the status and never touches the input hash.
//
// A dev without an id is ErrBadInput; a dev without a backend gets
// model.DeviceBackend(dev.ID). A missing job is the Store's error
// (db.ErrNotFound).
func BindASRDevice(ctx context.Context, st db.Store, jobID string, dev model.ASRDevice) error {
	if dev.ID == "" {
		return fmt.Errorf("%w: asr device without an id", ErrBadInput)
	}
	if dev.Backend == "" {
		dev.Backend = model.DeviceBackend(dev.ID)
	}
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if !j.IsCurrent || j.Stage != model.StageASR {
		return fmt.Errorf("%w: job %s is not a current asr job", ErrConflict, jobID)
	}
	if j.Status != model.StatusQueued && j.Status != model.StatusRunning {
		return fmt.Errorf("%w: asr job %s is %s, a device is bound only when it starts", ErrConflict, jobID, j.Status)
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok || !snap.Auto {
		return fmt.Errorf("%w: asr job %s has no auto-mode snapshot (fixed snapshots are never rebound)", ErrConflict, jobID)
	}
	raw := model.MarshalSnapshot(model.ASRDeviceSnapshot(dev, snap.Language, true))
	if raw == j.ConfigSnapshot {
		return nil
	}
	return st.SetJobSnapshot(ctx, jobID, raw)
}
