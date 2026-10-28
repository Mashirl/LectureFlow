// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Local devices whose components are missing (fresh-cuda; NOTES
// "决定（fresh-cuda：core-builder，2026-10-27）"; devices.WorkerState).
//
//   - planDevices hands the power-aware policy (devices.Place) every
//     enabled, present local device whose worker would not start the job
//     now as not present: the backend's environment or the run's model is
//     missing. The policy then places the job on the other devices as if
//     that device were not there (a laptop with an uninstalled CUDA uses
//     its Intel GPU on AC, a desktop the next device in the list). The
//     policy input of everything else (the resource bar's view, the
//     self-tests, the power changes) is unchanged.
//   - When nothing is left because of such devices, the job's waiting_for
//     names the first one the policy would have used:
//     devices.WaitComponentMissing / WaitModelMissing (localUnavailable:
//     Colab's overflow takes the job as on a computer without a local
//     device) or devices.WaitComponentInstalling (a task installs it: the
//     device will take jobs soon, Colab's overflow treats it like a busy
//     device). It starts there by itself once the component is installed
//     (every scan checks again).
//   - Without a component manager, in test mode, and once the components
//     are installed, nothing is masked: the admission is the earlier one.

import (
	"path/filepath"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// maskUnready marks the enabled, present local devices of in whose worker
// would not start j's run now as not present and returns their
// devices.WorkerState by id (nil when none). The run's model on a device is
// the device row's, or the fixed snapshot's on its own device (as
// devicePlan.target). Never call it with s.mu held.
func (s *Scheduler) maskUnready(sc *scanState, in *devices.PolicyInput, p devicePlan) map[string]string {
	if s.cfg == nil || s.cfg.Components == nil || s.fakeASR() {
		return nil
	}
	var masked map[string]string
	for i := range in.Devices {
		d := &in.Devices[i]
		if !d.Enabled || !d.Present {
			continue
		}
		row, ok := sc.row(d.ID)
		if !ok {
			continue
		}
		mdl, precision := row.Model, row.Precision
		if p.fixed && p.snap.DeviceID == d.ID {
			mdl, precision = p.snap.Model, p.snap.Precision
		}
		st := s.workerState(sc, d.Backend, mdl, precision)
		if st == devices.WorkerReady {
			continue
		}
		d.Present = false
		if masked == nil {
			masked = map[string]string{}
		}
		masked[d.ID] = st
	}
	return masked
}

// workerState is devices.WorkerState, once per scan for each backend,
// model and precision.
func (s *Scheduler) workerState(sc *scanState, backend, mdl, precision string) string {
	key := backend + "|" + mdl + "|" + precision
	if v, ok := sc.workers[key]; ok {
		return v
	}
	dataDir := s.dataDir
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	v := devices.WorkerState(s.cfg, dataDir, backend, mdl, precision)
	if sc.workers == nil {
		sc.workers = map[string]string{}
	}
	sc.workers[key] = v
	return v
}

// componentWait is the waiting_for of a job that the policy could not place
// once the devices of masked were left out (masked from maskUnready, in the
// masked input): the first device it would have used with them, by its
// missing component; "" when it would not have used any of them either.
func componentWait(in devices.PolicyInput, pj devices.PolicyJob, masked map[string]string) string {
	in.Devices = append([]devices.PolicyDevice(nil), in.Devices...)
	for i := range in.Devices {
		if _, ok := masked[in.Devices[i].ID]; ok {
			in.Devices[i].Present = true
		}
	}
	for _, id := range devices.Place(in, pj).Devices {
		if st, ok := masked[id]; ok {
			return devices.WorkerWait(st, id)
		}
	}
	return ""
}

// noLocalDevice: a policy wait that says no local device can take the job
// (as opposed to a paused or busy one).
func noLocalDevice(reason string) bool {
	switch reason {
	case model.WaitNoASRDevice, model.WaitPowerNoDevice, model.WaitPowerNoSaverDevice:
		return true
	}
	return false
}
