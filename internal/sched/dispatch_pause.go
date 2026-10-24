// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import "sync"

// PauseDispatch stops the scheduler from dispatching anything new until
// the returned resume is called (SPEC-v3 V1.3: the data directory copy,
// api.RelocateHooks.PauseDispatch). While held, no scan runs: no queued
// job starts (GPU, CPU and API alike), no waiting device self-test starts,
// no pending job is queued and no deferred or debounced request creates
// jobs, so the database is not written by the scheduler. Jobs and
// self-tests already running are not affected. Pauses nest: dispatch
// resumes when every caller has resumed. Calling one resume more than once
// has no further effect. Not persisted (a restart clears it).
//
// It returns once a scan in progress (which may have read the state before
// the pause) has ended, so nothing is dispatched after it returns.
func (s *Scheduler) PauseDispatch() (resume func()) {
	s.mu.Lock()
	s.dispatchHolds++
	s.mu.Unlock()
	// Barrier: every later scan sees the hold.
	s.scanMu.Lock()
	s.scanMu.Unlock() //nolint:staticcheck // empty critical section on purpose
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.dispatchHolds--
			s.mu.Unlock()
			s.Kick()
		})
	}
}

// DispatchPaused reports whether a PauseDispatch is in effect.
func (s *Scheduler) DispatchPaused() bool { return s.dispatchHeld() }

func (s *Scheduler) dispatchHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatchHolds > 0
}
