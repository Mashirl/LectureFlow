// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows && realgpu

package resources

// Reads the real GPU Engine performance counters of this machine (lead,
// 3b Gate round 1): only with the realgpu build tag (make.ps1 realgpu),
// never in the default test run, which covers the pure parsing functions
// (devices_test.go: TestParseEngineInstance, TestLUIDKey, TestUtilByLUID,
// TestDeviceCardsUtilUnreadable).

import (
	"testing"
	"time"
)

// The real PDH reader on this machine: it opens, collects twice and every
// instance it returns has a LUID. The realgpu run is made on the machine
// with the Intel GPU / NPU, so a missing counter set is a failure here.
func TestPDHReaderReadsEngineCounters(t *testing.T) {
	r, err := newEngineReader()
	if err != nil {
		t.Fatalf("GPU Engine counters not available: %v", err)
	}
	defer r.Close()
	first, err := r.Read()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first != nil {
		t.Errorf("first read of a rate counter returned %d samples, want nil (priming)", len(first))
	}
	time.Sleep(300 * time.Millisecond)
	ss, err := r.Read()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if ss == nil {
		t.Errorf("second read returned nil, want a (possibly empty) slice")
	}
	for _, s := range ss {
		if _, _, ok := ParseEngineInstance(s.Instance); !ok {
			t.Errorf("instance without a LUID: %q", s.Instance)
		}
		if s.Value < 0 {
			t.Errorf("%s: negative value %v", s.Instance, s.Value)
		}
	}
	t.Logf("%d engine instances, %d adapters: %v", len(ss), len(UtilByLUID(ss)), UtilByLUID(ss))
}
