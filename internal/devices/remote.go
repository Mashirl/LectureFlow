// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"errors"
	"fmt"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// isRemoteID: a device of the remote Colab backend (SPEC-v3 V14).
func isRemoteID(id string) bool { return model.DeviceBackend(id) == model.BackendColab }

// A colab:remote row List hides now is one of hiddenIDs (platform.go).

// SetRemote puts colab:remote into the device table (present: Colab is
// enabled and a credential is stored; the scheduler calls it through
// sched.Options.OnRemote) or takes it out. The first time it is present
// its row is inserted (enabled, priority model.DefaultDevicePriority — the
// lowest of the accelerators —, max_parallel 1, its workbench prompt
// dismissed: Colab has no self-test); afterwards its label, model and
// precision follow row (Colab's settings), the user's enabled switch and
// priority are kept. A row that is not present stays in the database but
// List hides it (Get still returns it). Calls the OnChange hooks.
func (r *Registry) SetRemote(ctx context.Context, present bool, row model.ASRDevice) error {
	if row.ID == "" {
		row.ID = model.DeviceColab
	}
	if !isRemoteID(row.ID) {
		return fmt.Errorf("%w: %s is not a remote device", ErrInvalid, row.ID)
	}
	row.Backend = model.BackendColab
	r.wmu.Lock()
	var err error
	if present {
		now := r.nowRFC3339()
		err = r.st.Tx(ctx, func(tx db.Store) error {
			d, err := tx.GetASRDevice(ctx, row.ID)
			switch {
			case errors.Is(err, db.ErrNotFound):
				nd := row
				nd.Enabled, nd.DetectedAt, nd.LastBenchmark = true, &now, nil
				if nd.MaxParallel < 1 {
					nd.MaxParallel = 1
				}
				if nd.Priority == 0 {
					nd.Priority = model.DefaultDevicePriority(row.ID)
				}
				if err := tx.InsertASRDevice(ctx, nd); err != nil {
					return err
				}
				return tx.SetKV(ctx, model.KVASRDevicePromptDismissedPrefix+row.ID, "1")
			case err != nil:
				return err
			}
			if d.Label == row.Label && d.Model == row.Model && d.Precision == row.Precision && d.Backend == row.Backend {
				return nil
			}
			d.Label, d.Model, d.Precision, d.Backend, d.DetectedAt = row.Label, row.Model, row.Precision, row.Backend, &now
			return tx.UpdateASRDevice(ctx, d)
		})
		if err == nil {
			err = r.reload(ctx)
		}
	}
	if err == nil {
		r.mu.Lock()
		r.remote = present
		r.mu.Unlock()
	}
	r.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("colab:remote in the device table: %w", err)
	}
	r.changed()
	return nil
}
