// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package procutil

import (
	"os"
	"testing"
)

// childEnv makes the test binary act as the child process (runChild).
const childEnv = "PROCUTIL_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(runChild(mode))
	}
	os.Exit(m.Run())
}

func TestMemoryExceeded(t *testing.T) {
	l := Limits{MemoryBytes: 1000}
	for _, c := range []struct {
		u      Usage
		l      Limits
		failed bool
		want   bool
	}{
		{Usage{MemoryLimitHit: true}, l, false, true},        // reported, even if the child swallowed it
		{Usage{PeakMemory: 950}, l, true, true},              // failed close to the cap
		{Usage{PeakMemory: 950}, l, false, false},            // succeeded close to the cap
		{Usage{PeakMemory: 500}, l, true, false},             // failed for another reason
		{Usage{MemoryLimitHit: true}, Limits{}, true, false}, // no cap
		{Usage{PeakMemory: 900}, l, true, true},              // 90%
		{Usage{PeakMemory: 899}, l, true, false},
	} {
		if got := c.u.MemoryExceeded(c.l, c.failed); got != c.want {
			t.Errorf("%+v %+v failed=%v: %v, want %v", c.u, c.l, c.failed, got, c.want)
		}
	}
}
