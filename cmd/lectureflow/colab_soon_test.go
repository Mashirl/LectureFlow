// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/fetch"
)

func TestDownloadSoon(t *testing.T) {
	now := time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC)
	d := newDownloadSoon()
	d.now = func() time.Time { return now }
	if got := d.eta(context.Background()); got != -1 {
		t.Fatalf("no queue: %v", got)
	}
	dl := func(done, total int64) []fetch.TaskInfo {
		return []fetch.TaskInfo{{ID: 1, State: bridge.StateDownloading, BytesDone: done, BytesTotal: total},
			{ID: 2, State: bridge.StateQueued}}
	}
	// The first sight of a download: no rate yet.
	if got := d.estimate(dl(10<<20, 110<<20)); got != -1 {
		t.Errorf("first sample: %v", got)
	}
	// 10 MiB in 10 s: 100 MiB left take 100 s.
	now = now.Add(10 * time.Second)
	if got := d.estimate(dl(20<<20, 110<<20)); got != 90*time.Second {
		t.Errorf("eta %v, want 90s", got)
	}
	// Unknown size: unknown.
	now = now.Add(time.Second)
	if got := d.estimate(dl(21<<20, -1)); got != -1 {
		t.Errorf("unknown size: %v", got)
	}
	// Merging or importing: the lecture comes now.
	if got := d.estimate([]fetch.TaskInfo{{ID: 3, State: bridge.StateImporting}}); got != 0 {
		t.Errorf("importing: %v", got)
	}
	if len(d.last) != 0 {
		t.Errorf("samples of finished downloads kept: %v", d.last)
	}
}
