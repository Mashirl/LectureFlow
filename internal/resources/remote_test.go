// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// SPEC-v3 V14.5 (v14-colab-integrate): the remote area of the resource bar.
func TestSnapshotRemote(t *testing.T) {
	f := newDevFixture(t)
	at := "2026-10-24T08:00:00Z"
	f.rows = append(f.rows, model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Label: "Google Colab · T4",
		Enabled: true, Model: "large-v3", Precision: "float16", MaxParallel: 1, Priority: 5, DetectedAt: &at})
	f.stats = append(f.stats, sched.DeviceStatus{ID: model.DeviceColab, SlotsUsed: 1, SlotsMax: 1,
		Running: []sched.RunningASR{{JobID: "job-2", LectureID: f.lec.ID, Progress: 0.3}}})
	on := false
	d := f.deps(newClock())
	d.Remote = func(ctx context.Context) (colab.Status, bool) {
		return colab.Status{Service: "colab", Configured: true, Accelerator: "T4", State: colab.StateNone,
			WindowTotalSec: 4 * 3600}, on
	}
	s := New(d)
	s.newEngines = (&fakeEngines{}).open

	// Off: no remote area, and colab:remote never becomes a local device card.
	snap := s.build(context.Background())
	b, _ := json.Marshal(snap)
	if snap.Remote != nil || strings.Contains(string(b), `"remote"`) {
		t.Fatalf("remote while off: %s", b)
	}
	for _, c := range snap.Devices {
		if c.ID == model.DeviceColab {
			t.Fatalf("colab:remote as a device card: %+v", c)
		}
	}
	// On, no session: the collapsed line ("远端 GPU：未使用").
	on = true
	snap = s.build(context.Background())
	if snap.Remote == nil || snap.Remote.State != colab.StateNone || snap.Remote.Accelerator != "T4" {
		t.Fatalf("remote = %+v", snap.Remote)
	}
	b, _ = json.Marshal(snap)
	if !strings.Contains(string(b), `"remote":{"service":"colab"`) {
		t.Fatalf("JSON %s", b)
	}
}
