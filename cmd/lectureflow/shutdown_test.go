// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// The download queue stops alongside the scheduler (M1 acceptance): the two
// waits overlap instead of adding up.
func TestAlongsideOverlaps(t *testing.T) {
	var ran atomic.Bool
	start := time.Now()
	wait := alongside(func() {
		time.Sleep(300 * time.Millisecond)
		ran.Store(true)
	})
	time.Sleep(300 * time.Millisecond) // the scheduler's part
	if !wait(time.Second) || !ran.Load() {
		t.Fatal("the background part did not end")
	}
	if d := time.Since(start); d > 550*time.Millisecond {
		t.Fatalf("the parts took %s: they did not run at the same time", d)
	}
	// a part that overruns is reported, not waited for
	wait = alongside(func() { time.Sleep(2 * time.Second) })
	start = time.Now()
	if wait(100 * time.Millisecond) {
		t.Fatal("an unfinished part reported done")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %s", d)
	}
}
