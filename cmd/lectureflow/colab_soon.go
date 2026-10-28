// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/fetch"
)

// downloadSoon tells the scheduler how soon a download of the browser
// extension becomes a lecture (sched.Options.RemoteSoon, colab-batch: the
// Colab runtime is kept for a lecture about to arrive): at once for a
// download that is merging or importing; for one downloading with a known
// size, the rest at the rate since the previous call; otherwise unknown
// (-1). The queue is set once it exists (it is built after the scheduler).
type downloadSoon struct {
	q   atomic.Pointer[fetch.Queue]
	now func() time.Time

	mu   sync.Mutex
	last map[int64]soonSample
}

type soonSample struct {
	at   time.Time
	done int64
}

func newDownloadSoon() *downloadSoon {
	return &downloadSoon{now: time.Now, last: map[int64]soonSample{}}
}

func (d *downloadSoon) setQueue(q *fetch.Queue) {
	if q != nil {
		d.q.Store(q)
	}
}

// eta is the soonest expected arrival (-1: none expected or unknown).
func (d *downloadSoon) eta(ctx context.Context) time.Duration {
	q := d.q.Load()
	if q == nil {
		return -1
	}
	tasks, err := q.List(ctx, "")
	if err != nil {
		return -1
	}
	return d.estimate(tasks)
}

func (d *downloadSoon) estimate(tasks []fetch.TaskInfo) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	best := time.Duration(-1)
	seen := map[int64]bool{}
	for _, t := range tasks {
		switch t.State {
		case bridge.StateMerging, bridge.StateImporting:
			best = 0
		case bridge.StateDownloading:
			seen[t.ID] = true
			prev, ok := d.last[t.ID]
			d.last[t.ID] = soonSample{at: now, done: t.BytesDone}
			el := now.Sub(prev.at).Seconds()
			if !ok || t.BytesTotal <= 0 || t.BytesDone <= prev.done || el <= 0 {
				continue
			}
			rate := float64(t.BytesDone-prev.done) / el
			eta := time.Duration(float64(max(0, t.BytesTotal-t.BytesDone)) / rate * float64(time.Second))
			if best < 0 || eta < best {
				best = eta
			}
		}
	}
	for id := range d.last {
		if !seen[id] {
			delete(d.last, id)
		}
	}
	return best
}
