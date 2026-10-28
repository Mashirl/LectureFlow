// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"sync"
	"time"

	"lectureflow/internal/llm"
)

// Prompt-cache warm-up (perf-llm, NOTES.md). The requests of a translate or
// repair run share one long system prompt (the §13 instructions, the
// glossary, the summary, for repair also the slides). DeepSeek caches a
// prefix only once it has processed a request with it; requests sent
// together with the first one all pay the full price for it. So the first
// request goes out alone and the others follow when the warm-up ends. Only
// the order of sending changes: requests, contexts, QA and the per-sentence
// writes are exactly as before (SPEC §7, §13 untouched).

// warmup says when the other items of a run may start.
type warmup struct {
	// afterSend > 0 ends the warm-up this long after the first item's
	// first request was sent (llm.WithSentHook), without waiting for its
	// answer; 0 waits for the first item to finish.
	afterSend time.Duration
	// max ends the warm-up at the latest this long after the first item
	// started (it may wait in the API pool, or be retried).
	max time.Duration
}

var (
	// translateWarmup: a translate request answers in about a second and its
	// answer means the prefix was processed; production data (8 / 300) showed
	// the requests sent after the first answer hitting the cache.
	translateWarmup = warmup{max: 15 * time.Second}
	// repairWarmup: a repair chunk takes 6–10 s to generate, but its prefix
	// is read in the first second; the other chunks follow 2 s after the
	// first was sent instead of waiting for its answer.
	repairWarmup = warmup{afterSend: 2 * time.Second, max: 15 * time.Second}
)

// warmMinItems: runs with fewer items do not warm up (one serial request to
// save at most one cache miss is not worth it).
const warmMinItems = 3

// warmupOff switches the warm-up off (tests: the behaviour before perf-llm,
// for the comparison in perf_llm_test.go).
var warmupOff = false

// parallelWarm is parallel with a warm-up: item 0 runs first; the other
// items wait until it finishes, or w says otherwise. At most workers items
// are in flight at any time, waiting ones included. A fatal error of item 0
// stops the run like in parallel: if it comes before the warm-up ended, no
// waiting item runs.
func parallelWarm(ctx context.Context, n, workers int, w warmup, fn func(ctx context.Context, i int) (stop bool, err error)) error {
	if n < warmMinItems || workers < 2 || warmupOff {
		return parallel(ctx, n, workers, fn)
	}
	gate := make(chan struct{})
	var (
		mu      sync.Mutex
		settled bool // the warm-up ended: the gate opened, or item 0 failed and it stays shut
		timers  []*time.Timer
	)
	// settle ends the warm-up once. release opens the gate; otherwise
	// (item 0 stopped the run) the gate stays shut for good, including
	// against a timer firing later, and the waiting items leave through ctx,
	// which parallel cancels as soon as item 0 has returned. Opening the
	// gate on a fatal error would let the waiting items race that cancel
	// and run after it (perf-llm, NOTES.md).
	settle := func(release bool) {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		settled = true
		if release {
			close(gate)
		}
	}
	open := func() { settle(true) }
	after := func(d time.Duration) {
		mu.Lock()
		timers = append(timers, time.AfterFunc(d, open))
		mu.Unlock()
	}
	defer func() {
		mu.Lock()
		for _, t := range timers {
			t.Stop()
		}
		mu.Unlock()
	}()
	return parallel(ctx, n, workers, func(ctx context.Context, i int) (bool, error) {
		if i == 0 {
			if w.max > 0 {
				after(w.max)
			}
			if w.afterSend > 0 {
				var sent sync.Once
				ctx = llm.WithSentHook(ctx, func() { sent.Do(func() { after(w.afterSend) }) })
			}
			stop, err := fn(ctx, 0)
			settle(!stop) // before parallel sees stop and cancels ctx
			return stop, err
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return true, ctx.Err()
		}
		// Both may be ready (select picks at random): a stopped or
		// cancelled run starts nothing new, as in parallel.
		if err := ctx.Err(); err != nil {
			return true, err
		}
		return fn(ctx, i)
	})
}
