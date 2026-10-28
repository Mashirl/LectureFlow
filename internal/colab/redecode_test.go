// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"testing"
)

// The switch "修复退化的转写窗口" (redecode, 2026-10-27): Job.NoRedecode
// puts "redecode": false into the remote job.json, so worker/asr.py on the
// runtime skips the second decode as the local worker does; with the switch
// on (the zero value) job.json has no such field (TestMemoryModeOffIsUnchanged:
// eight fields, byte for byte as before).
func TestRemoteJobCarriesTheRedecodeSwitchOff(t *testing.T) {
	e, w := newMemoryEnv(t, "borrow_run_release.json", MemoryModeOff)
	ctx := context.Background()
	job := e.job(t)
	job.NoRedecode = true
	if _, err := e.b.Transcribe(ctx, job, nil); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if v, ok := w.jobJSON["redecode"]; !ok || v != false || len(w.jobJSON) != 9 {
		t.Errorf("job.json %v", w.jobJSON)
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatalf("QueueDrained: %v", err)
	}
	e.rs.finish()
	e.checkClean(t)
}
