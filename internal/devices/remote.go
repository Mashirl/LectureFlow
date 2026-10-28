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

// ---------- SSH inference servers (ssh-mvp) ----------

// SSH rows ("ssh:<slug>", backend = the engine the server runs) are in the
// table while the server is configured (internal/sshremote adds and removes
// them); they are never detected: Present follows SetSSHPresent (connected
// and set up). Like colab:remote they are outside the power-aware policy
// and multi_device (the scheduler skips model.IsRemoteDevice rows there).

// UpsertSSH inserts an SSH server's row (enabled, priority row.Priority or
// model.DefaultDevicePriority, max_parallel at least 1, its workbench
// prompt dismissed: the server is added on purpose) or updates its label,
// backend, model and precision; the user's enabled switch, priority and
// max_parallel are kept. Calls the OnChange hooks.
func (r *Registry) UpsertSSH(ctx context.Context, row model.ASRDevice) error {
	if !model.IsSSHDevice(row.ID) {
		return fmt.Errorf("%w: %s is not an SSH server's device", ErrInvalid, row.ID)
	}
	if row.Backend != model.BackendFasterWhisper && row.Backend != model.BackendMLX {
		return fmt.Errorf("%w: an SSH server runs faster-whisper or mlx, not %q", ErrInvalid, row.Backend)
	}
	r.wmu.Lock()
	now := r.nowRFC3339()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		d, err := tx.GetASRDevice(ctx, row.ID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			nd := row
			nd.Enabled, nd.DetectedAt, nd.LastBenchmark = true, &now, nil
			nd.MaxParallel = max(nd.MaxParallel, 1)
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
		if d.Label == row.Label && d.Backend == row.Backend && d.Model == row.Model && d.Precision == row.Precision {
			return nil
		}
		d.Label, d.Backend, d.Model, d.Precision = row.Label, row.Backend, row.Model, row.Precision
		return tx.UpdateASRDevice(ctx, d)
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("%s in the device table: %w", row.ID, err)
	}
	r.changed()
	return nil
}

// RemoveSSH deletes an SSH server's row (a missing row is not an error).
func (r *Registry) RemoveSSH(ctx context.Context, id string) error {
	if !model.IsSSHDevice(id) {
		return fmt.Errorf("%w: %s is not an SSH server's device", ErrInvalid, id)
	}
	del, ok := r.st.(interface {
		DeleteASRDevice(ctx context.Context, id string) error
	})
	if !ok {
		return fmt.Errorf("devices: the store cannot delete device rows")
	}
	r.wmu.Lock()
	err := del.DeleteASRDevice(ctx, id)
	if err == nil {
		err = r.st.SetKV(ctx, model.KVASRDevicePromptDismissedPrefix+id, "")
	}
	if err == nil {
		err = r.reload(ctx)
	}
	if err == nil {
		r.mu.Lock()
		delete(r.ssh, id) // a nil map (SSH off): no-op
		r.mu.Unlock()
	}
	r.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("removing %s from the device table: %w", id, err)
	}
	r.changed()
	return nil
}

// EnableSSH switches the SSH servers on (cmd, only with
// LECTUREFLOW_EXPERIMENTAL_SSH=1; security review 3, L6): before it, and
// without it, List and the snapshots leave every ssh:* row out (the rows
// stay in the database) and SetSSHPresent does nothing.
func (r *Registry) EnableSSH() {
	r.mu.Lock()
	if r.ssh == nil {
		r.ssh = map[string]bool{}
	}
	r.mu.Unlock()
	r.changed()
}

// remoteHiddenLocked reports whether List and the snapshots leave a remote
// row out now: colab:remote while Colab is not in the table (SetRemote),
// ssh:* rows while the SSH servers are off (EnableSSH not called).
func (r *Registry) remoteHiddenLocked(id string) bool {
	switch {
	case isRemoteID(id):
		return !r.remote
	case model.IsSSHDevice(id):
		return r.ssh == nil
	}
	return false
}

// SetSSHPresent records whether an SSH server can take lectures now
// (connected, its agent and environment ready). Calls the OnChange hooks
// when it changed. Nothing while the SSH servers are off.
func (r *Registry) SetSSHPresent(id string, present bool) {
	if !model.IsSSHDevice(id) {
		return
	}
	r.mu.Lock()
	if r.ssh == nil {
		r.mu.Unlock()
		return
	}
	changed := r.ssh[id] != present
	r.ssh[id] = present
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}
