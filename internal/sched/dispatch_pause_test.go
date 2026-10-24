// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"testing"
	"time"

	"lectureflow/internal/model"
)

// SPEC-v3 V1.3 (data directory copy): PauseDispatch stops new dispatches,
// leaves the running job alone, nests, and resumes when every caller has
// resumed.
func TestPauseDispatch(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, &fakeStage{name: model.StagePrep, res: ResourceCPU, run: g.run})
	a := e.lecture()
	e.start()
	e.waitStatus(a.ID, model.StagePrep, model.StatusRunning)
	waitRuns(t, g, 1)

	r1 := e.s.PauseDispatch()
	r2 := e.s.PauseDispatch()
	if !e.s.DispatchPaused() {
		t.Fatal("DispatchPaused = false after PauseDispatch")
	}
	b := e.lecture()
	e.s.Kick()
	time.Sleep(80 * time.Millisecond)
	if _, _, runs := g.stats(); runs != 1 {
		t.Fatalf("dispatched while paused: runs %d", runs)
	}
	if j := e.lj(b.ID, model.StagePrep); j.Status != model.StatusPending {
		t.Fatalf("paused scheduler touched a job: %s", j.Status)
	}

	// The running job is not affected.
	close(g.release)
	e.waitStatus(a.ID, model.StagePrep, model.StatusDone)

	// Nested: one resume (twice) is not enough.
	r1()
	r1()
	if !e.s.DispatchPaused() {
		t.Fatal("dispatch resumed while a second pause is held")
	}
	e.s.Kick()
	time.Sleep(80 * time.Millisecond)
	if _, _, runs := g.stats(); runs != 1 {
		t.Fatalf("dispatched while still paused: runs %d", runs)
	}

	r2()
	if e.s.DispatchPaused() {
		t.Fatal("still paused after every resume")
	}
	e.waitStatus(b.ID, model.StagePrep, model.StatusDone)
	waitRuns(t, g, 2)
}

func waitRuns(t *testing.T, g *gate, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, _, runs := g.stats(); runs == n {
			return
		}
		if time.Now().After(deadline) {
			_, _, runs := g.stats()
			t.Fatalf("runs = %d, want %d", runs, n)
		}
		time.Sleep(time.Millisecond)
	}
}
