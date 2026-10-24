// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import "time"

// Shutdown is Stop with a time limit, for the graceful stop of the desktop
// application (SPEC-v3 V1.1, v3-brief §1): no new job is dispatched, every
// running job and self-test is cancelled (their workers are killed by the
// cancelled context) and their goroutines are waited for. It reports
// whether they all ended within timeout; when not, they go on ending in the
// background and the caller proceeds (the process is about to exit). The
// interrupted jobs stay running in the database: startup recovery (SPEC
// §4.7) handles them on the next start. The scheduling policy is unchanged.
func (s *Scheduler) Shutdown(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
