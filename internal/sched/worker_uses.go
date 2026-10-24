// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"sort"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// What a WorkerUse is.
const (
	UseASR        = "asr"         // a local asr run (running, or admitted and starting)
	UseDeviceTest = "device_test" // a device self-test (waiting for its slot or running)
	UseMaterial   = "material"    // PDF extraction (the python component's base environment)
)

// WorkerUse is local work that runs a worker of a backend's environment
// and, for transcription, a model (v3 Gate round 1 item 5: the component
// manager must not uninstall what it needs). Model and Precision are the
// configuration an asr run was admitted with; Precision may be "auto"
// (resolved by the worker). Model "" = any model of the backend (a device
// self-test tries several) or none (material).
type WorkerUse struct {
	What      string `json:"what"`
	ID        string `json:"id"` // job id (asr, material) or device id (device_test)
	Backend   string `json:"backend"`
	Model     string `json:"model,omitempty"`
	Precision string `json:"precision,omitempty"`
}

// WorkerUses lists the local work in progress that runs a Python worker:
// asr runs this scheduler admitted (registered before their queued →
// running transition, so a job the scan just picked is included), device
// self-tests that wait for their slot or run, and material extraction runs.
// Remote (Colab) runs use nothing local and are left out. Sorted by What,
// then ID. Safe for concurrent use; it never calls out while holding the
// scheduler's lock.
func (s *Scheduler) WorkerUses() []WorkerUse {
	s.mu.Lock()
	var out []WorkerUse
	for id, r := range s.running {
		switch r.job.Stage {
		case model.StageASR:
			if u, ok := asrUse(id, r); ok {
				out = append(out, u)
			}
		case model.StageMaterialExtract:
			out = append(out, WorkerUse{What: UseMaterial, ID: id, Backend: config.ComponentBase})
		}
	}
	for id, t := range s.tests {
		if t == nil || !t.active() || isRemote(t.dev.ID) || t.dev.Backend == model.BackendColab {
			continue
		}
		// A self-test tries several candidates (devices.Candidates) one
		// after the other: any model of the backend may be next.
		out = append(out, WorkerUse{What: UseDeviceTest, ID: id, Backend: t.dev.Backend})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].What != out[j].What {
			return out[i].What < out[j].What
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// asrUse is the backend, model and precision an asr run uses: the device
// row it was admitted on for an auto snapshot (bound in its start
// transaction), else its snapshot. false for a remote run.
func asrUse(id string, r *run) (WorkerUse, bool) {
	u := WorkerUse{What: UseASR, ID: id}
	snap, ok := model.ParseASRSnapshot(r.job.ConfigSnapshot)
	if ok {
		snap = model.NormalizeASRSnapshot(snap)
	}
	switch {
	case r.devRow.ID != "" && (r.bind || !ok || snap.Auto):
		u.Backend, u.Model, u.Precision = r.devRow.Backend, r.devRow.Model, r.devRow.Precision
		if u.Backend == "" {
			u.Backend = model.DeviceBackend(r.devRow.ID)
		}
		if isRemote(r.devRow.ID) {
			return u, false
		}
	case ok:
		if isRemote(snap.DeviceID) || snap.Backend == model.BackendColab {
			return u, false
		}
		u.Backend, u.Model, u.Precision = snap.Backend, snap.Model, snap.Precision
	default:
		u.Backend = model.BackendFasterWhisper // no snapshot: the v1 worker
	}
	if u.Backend == model.BackendColab {
		return u, false
	}
	return u, true
}
