// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
)

// span is when an item ran.
type span struct{ start, end time.Time }

type spans struct {
	mu       sync.Mutex
	s        map[int]span
	inflight int
	peak     int
}

func (r *spans) run(i int, body func()) {
	r.mu.Lock()
	if r.s == nil {
		r.s = map[int]span{}
	}
	r.inflight++
	r.peak = max(r.peak, r.inflight)
	r.s[i] = span{start: time.Now()}
	r.mu.Unlock()
	body()
	r.mu.Lock()
	sp := r.s[i]
	sp.end = time.Now()
	r.s[i] = sp
	r.inflight--
	r.mu.Unlock()
}

// TestParallelWarmWaitsForTheFirstItem (perf-llm): no other item starts
// before item 0 finished; afterwards up to workers run at once.
func TestParallelWarmWaitsForTheFirstItem(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 12, 4, warmup{max: 10 * time.Second}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i == 0 {
				time.Sleep(40 * time.Millisecond)
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.s) != 12 {
		t.Fatalf("%d items ran", len(r.s))
	}
	for i := 1; i < 12; i++ {
		if r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d started before item 0 finished", i)
		}
	}
	if r.peak < 2 || r.peak > 4 {
		t.Errorf("peak %d, want 2..4", r.peak)
	}
}

// Fewer than warmMinItems items, or one worker: plain parallel.
func TestParallelWarmSmallRuns(t *testing.T) {
	var r spans
	both := make(chan struct{})
	var once sync.Once
	n := 0
	var mu sync.Mutex
	err := parallelWarm(context.Background(), 2, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			mu.Lock()
			n++
			if n == 2 {
				once.Do(func() { close(both) })
			}
			mu.Unlock()
			select { // both are in flight together, or this times out
			case <-both:
			case <-time.After(2 * time.Second):
			}
		})
		return false, nil
	})
	if err != nil || r.peak != 2 {
		t.Fatalf("two items: peak %d, err %v", r.peak, err)
	}
}

// The cap: a slow first item holds the others back at most w.max.
func TestParallelWarmCap(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 5, 5, warmup{max: 30 * time.Millisecond}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i == 0 {
				time.Sleep(400 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		if !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for the slow first item past the cap", i)
		}
		if d := r.s[i].start.Sub(r.s[0].start); d < 25*time.Millisecond {
			t.Errorf("item %d started %v after item 0, before the cap", i, d)
		}
	}
}

// afterSend: the others start that long after item 0's request was sent,
// without waiting for its answer (repair, whose chunks take seconds).
func TestParallelWarmAfterSend(t *testing.T) {
	fake := llmtest.New(t)
	fake.SetDelay(400 * time.Millisecond)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	fake.Configure(cfg)
	c := llm.New(cfg, nil, "", nil) // no pool: item 0 is sent as soon as it starts
	var r spans
	err := parallelWarm(context.Background(), 4, 4, warmup{afterSend: 50 * time.Millisecond, max: 10 * time.Second}, func(ctx context.Context, i int) (bool, error) {
		var err error
		r.run(i, func() {
			if i == 0 {
				_, err = c.Complete(ctx, config.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
			}
		})
		return err != nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 4; i++ {
		if !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for item 0's answer", i)
		}
		if d := r.s[i].start.Sub(r.s[0].start); d < 45*time.Millisecond {
			t.Errorf("item %d started %v after item 0, want the 50ms head start", i, d)
		}
	}
}

// A fatal error of item 0 stops the run: no other item runs.
func TestParallelWarmFirstItemFails(t *testing.T) {
	boom := errors.New("boom")
	var r spans
	err := parallelWarm(context.Background(), 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {})
		if i == 0 {
			return true, boom
		}
		return false, nil
	})
	if !errors.Is(err, boom) || len(r.s) != 1 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
	// cancelled while the others wait: they give up at once
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	err = parallelWarm(ctx, 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		if i == 0 {
			cancel()
			<-ctx.Done()
			return true, ctx.Err()
		}
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(start))
	}
}

// The waiting items are parked at the gate when item 0 fails: none of them
// runs. (If item 0 opened the gate on its way out, ahead of parallel's
// cancel, parked waiters would go through it.) The sleep only gives the
// waiters time to park; the assertion holds whatever the timing.
func TestParallelWarmFirstItemFailsWhileOthersWait(t *testing.T) {
	boom := errors.New("boom")
	for round := range 20 {
		var r spans
		err := parallelWarm(context.Background(), 40, 16, translateWarmup, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(5 * time.Millisecond)
				}
			})
			if i == 0 {
				return true, boom
			}
			return false, nil
		})
		if !errors.Is(err, boom) || len(r.s) != 1 {
			t.Fatalf("round %d: err %v, %d items ran", round, err, len(r.s))
		}
	}
}

// A non-fatal result of item 0 (stop == false, e.g. an unusable answer
// recorded as a failed sentence) still opens the gate: every item runs.
func TestParallelWarmFirstItemNonFatalErrorReleases(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {})
		if i == 0 {
			return false, errors.New("unusable")
		}
		return false, nil
	})
	if err != nil || len(r.s) != 10 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
}
