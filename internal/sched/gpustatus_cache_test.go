// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// perf-fix: GPUStatus reuses its nvidia-smi reading for GPUStatusTTL and
// concurrent callers share one run; pause flag and slots stay live.

type gpuTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gpuTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *gpuTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingGPU is a GPUQueryFunc that counts its runs; gate (when set)
// blocks each run until closed.
type countingGPU struct {
	mu    sync.Mutex
	mem   GPUMem
	err   error
	gate  chan struct{}
	calls atomic.Int32
}

func (g *countingGPU) query(ctx context.Context) (GPUMem, error) {
	g.calls.Add(1)
	g.mu.Lock()
	gate, mem, err := g.gate, g.mem, g.err
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return GPUMem{}, ctx.Err()
		}
	}
	return mem, err
}

func (g *countingGPU) set(mem GPUMem, err error) {
	g.mu.Lock()
	g.mem, g.err = mem, err
	g.mu.Unlock()
}

func newCachedGPUEnv(t *testing.T) (*env, *countingGPU, *gpuTestClock) {
	t.Helper()
	g := &countingGPU{mem: GPUMem{FreeMB: 9000, TotalMB: 12000, UsedMB: 3000}}
	clk := &gpuTestClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e := newEnvOpts(t, nil, func(o *Options) {
		o.GPUQuery = g.query
		o.Now = clk.Now
	})
	return e, g, clk
}

func TestGPUStatusReusesReadingWithinTTL(t *testing.T) {
	e, g, clk := newCachedGPUEnv(t)
	if st := e.s.GPUStatus(e.ctx); !st.OK || st.FreeMB != 9000 || st.TotalMB != 12000 {
		t.Fatalf("first status %+v", st)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("calls %d, want 1", n)
	}
	// Within the TTL: the same reading, no nvidia-smi run.
	g.set(GPUMem{FreeMB: 5000, TotalMB: 12000, UsedMB: 7000}, nil)
	clk.Advance(GPUStatusTTL - time.Millisecond)
	if st := e.s.GPUStatus(e.ctx); st.FreeMB != 9000 {
		t.Fatalf("cached status %+v", st)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("calls %d within the TTL, want 1", n)
	}
	// The pause flag is live, not part of the cached reading.
	if err := e.s.SetGPUPaused(true, false); err != nil {
		t.Fatal(err)
	}
	if st := e.s.GPUStatus(e.ctx); !st.Paused || g.calls.Load() != 1 {
		t.Fatalf("paused status %+v, calls %d", st, g.calls.Load())
	}
	// After the TTL: read again.
	clk.Advance(time.Millisecond)
	if st := e.s.GPUStatus(e.ctx); st.FreeMB != 5000 || st.UsedMB != 7000 {
		t.Fatalf("status after the TTL %+v", st)
	}
	if n := g.calls.Load(); n != 2 {
		t.Fatalf("calls %d after the TTL, want 2", n)
	}
	// A clock that went back does not keep a reading forever.
	clk.Advance(-time.Hour)
	e.s.GPUStatus(e.ctx)
	if n := g.calls.Load(); n != 3 {
		t.Fatalf("calls %d after the clock went back, want 3", n)
	}
}

func TestGPUStatusCachesFailures(t *testing.T) {
	e, g, clk := newCachedGPUEnv(t)
	g.set(GPUMem{}, errors.New("nvidia-smi: not found"))
	if st := e.s.GPUStatus(e.ctx); st.OK || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
	e.s.GPUStatus(e.ctx)
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("calls %d, want 1 (a failure is reused too)", n)
	}
	g.set(GPUMem{FreeMB: 1000, TotalMB: 8000, UsedMB: 7000}, nil)
	clk.Advance(GPUStatusTTL)
	if st := e.s.GPUStatus(e.ctx); !st.OK || st.Error != "" || st.TotalMB != 8000 {
		t.Fatalf("status after recovery %+v", st)
	}
}

func TestGPUStatusConcurrentCallersShareOneRun(t *testing.T) {
	e, g, _ := newCachedGPUEnv(t)
	gate := make(chan struct{})
	g.mu.Lock()
	g.gate = gate
	g.mu.Unlock()
	const n = 8
	var wg sync.WaitGroup
	res := make([]GPUStatus, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i] = e.s.GPUStatus(e.ctx)
		}(i)
	}
	// Let every caller reach the cache before the one run finishes.
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if c := g.calls.Load(); c != 1 {
		t.Fatalf("%d nvidia-smi runs for %d concurrent callers, want 1", c, n)
	}
	for i, st := range res {
		if !st.OK || st.FreeMB != 9000 {
			t.Fatalf("caller %d: %+v", i, st)
		}
	}
}

func TestGPUStatusCancelledCallerRecordsNothing(t *testing.T) {
	e, g, _ := newCachedGPUEnv(t)
	gate := make(chan struct{})
	g.mu.Lock()
	g.gate = gate
	g.mu.Unlock()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan GPUStatus)
	go func() { done <- e.s.GPUStatus(ctx) }()
	for g.calls.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	close(gate)
	// The next caller reads again (nothing was cached) and gets no
	// "context canceled" error.
	if st := e.s.GPUStatus(e.ctx); !st.OK || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if c := g.calls.Load(); c != 2 {
		t.Fatalf("calls %d, want 2", c)
	}
}

func TestGPUTotalMBUsesTheLastReading(t *testing.T) {
	e, g, clk := newCachedGPUEnv(t)
	if total := e.s.GPUTotalMB(e.ctx); total != 12000 {
		t.Fatalf("total %d", total)
	}
	if c := g.calls.Load(); c != 1 {
		t.Fatalf("calls %d, want 1 (nothing known yet)", c)
	}
	clk.Advance(time.Hour) // the total does not change: no new run however old
	if total := e.s.GPUTotalMB(e.ctx); total != 12000 || g.calls.Load() != 1 {
		t.Fatalf("total %d, calls %d", total, g.calls.Load())
	}
}

func TestGPUTotalMBUnknownAsksAgain(t *testing.T) {
	e, g, clk := newCachedGPUEnv(t)
	g.set(GPUMem{}, errors.New("nvidia-smi: not found"))
	if total := e.s.GPUTotalMB(e.ctx); total != 0 {
		t.Fatalf("total %d", total)
	}
	g.set(GPUMem{FreeMB: 100, TotalMB: 4096, UsedMB: 3996}, nil)
	clk.Advance(GPUStatusTTL)
	if total := e.s.GPUTotalMB(e.ctx); total != 4096 {
		t.Fatalf("total %d after the GPU became readable", total)
	}
}
