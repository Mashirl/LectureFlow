// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// v3 graceful stop: Shutdown cancels the running job and waits for it; the
// job stays running in the database and the next start's recovery (SPEC
// §4.7) requeues it, after which a new scheduler completes it.
func TestShutdownThenStartupRecovery(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, &fakeStage{name: model.StagePrep, res: ResourceCPU, run: g.run})
	l := e.lecture()
	e.start()
	before := e.waitStatus(l.ID, model.StagePrep, model.StatusRunning)
	// The job turns running before the stage function is entered: wait for
	// the run itself, so the "no dispatch after Shutdown" count below is exact.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, _, runs := g.stats(); runs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stage did not start")
		}
		time.Sleep(time.Millisecond)
	}

	if !e.s.Shutdown(5 * time.Second) {
		t.Fatal("Shutdown did not finish in time")
	}
	if cur, _, _ := g.stats(); cur != 0 {
		t.Fatalf("stage still running after Shutdown: %d", cur)
	}
	if j := e.lj(l.ID, model.StagePrep); j.Status != model.StatusRunning {
		t.Fatalf("interrupted job must stay running for recovery, got %s", j.Status)
	}
	e.s.Kick() // no dispatch after Shutdown
	time.Sleep(30 * time.Millisecond)
	if _, _, runs := g.stats(); runs != 1 {
		t.Fatalf("a job was dispatched after Shutdown: runs %d", runs)
	}

	if err := state.Recover(e.ctx, e.st, e.dataDir); err != nil {
		t.Fatal(err)
	}
	j := e.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusQueued || j.Attempt != before.Attempt+1 {
		t.Fatalf("after recovery: status %s attempt %d (before %d)", j.Status, j.Attempt, before.Attempt)
	}

	e.s = e.newScheduler(instant(model.StagePrep, ResourceCPU))
	e.start()
	e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
}

// Shutdown gives up after its time limit when a run ignores cancellation.
func TestShutdownTimeLimit(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	stuck := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		started <- struct{}{}
		<-release // ignores ctx
		return nil
	}}
	e := newEnv(t, nil, stuck)
	e.lecture()
	e.start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not start")
	}
	t0 := time.Now()
	if e.s.Shutdown(100 * time.Millisecond) {
		t.Fatal("Shutdown must report the time limit")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("Shutdown waited %v", d)
	}
	close(release)
	e.s.Stop() // the background stop completes once the run returns
}
