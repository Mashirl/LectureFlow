// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// grantOrder queues one Acquire per label (in the order given, each queued
// before the next) behind a held slot of a one-slot pool, releases the slot
// and returns the order in which the requests were granted.
func grantOrder(t *testing.T, pool *Pool, pp *providerPool, reqs []struct {
	ctx   context.Context
	label string
}) []string {
	t.Helper()
	holder, err := pool.Acquire(context.Background(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		order []string
		wg    sync.WaitGroup
	)
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := pool.Acquire(r.ctx, "deepseek")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, r.label)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			rel()
		}()
		waitWaiters(t, pp, i+1)
	}
	holder()
	wg.Wait()
	return order
}

type labelled = struct {
	ctx   context.Context
	label string
}

// TestAPIPoolUrgentBeforeBulk (perf-llm): a younger lecture's summary and
// repair requests are served before an older lecture's translation; within
// a class the FIFO of SPEC §5 holds; requests without a job stay last.
func TestAPIPoolUrgentBeforeBulk(t *testing.T) {
	pool, pp := priorityPool(t, 1, 0)
	oldTr := model.Job{ID: "tr-old", ScopeID: "L-old", Stage: model.StageTranslate, CreatedAt: "2026-01-01T00:00:00Z"}
	youngTr := model.Job{ID: "tr-young", ScopeID: "L-young", Stage: model.StageTranslate, CreatedAt: "2026-01-02T00:00:00Z"}
	youngSum := model.Job{ID: "sum-young", ScopeID: "L-young", Stage: model.StageSummary, CreatedAt: "2026-01-02T00:00:00Z"}
	youngRep := model.Job{ID: "rep-young", ScopeID: "L-young", Stage: model.StageRepair, CreatedAt: "2026-01-02T00:00:01Z"}
	pool.setPriority(oldTr, "2026-01-01T00:00:00Z")
	pool.setPriority(youngTr, "2026-01-05T00:00:00Z")
	pool.setPriority(youngSum, "2026-01-05T00:00:00Z")
	pool.setPriority(youngRep, "2026-01-05T00:00:00Z")
	bg := context.Background()
	order := grantOrder(t, pool, pp, []labelled{
		{bg, "none"},
		{WithJob(bg, youngTr), "young-tr"},
		{WithJob(bg, oldTr), "old-tr"},
		{WithJob(bg, oldTr), "old-tr"},
		{WithJob(bg, youngSum), "summary"},
		{WithJob(bg, youngRep), "repair"},
	})
	want := []string{"summary", "repair", "old-tr", "old-tr", "young-tr", "none"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("grant order = %v, want %v", order, want)
	}
}

// TestAPIPoolBulkNotStarved (perf-llm): while translation waits, at most
// UrgentBurst urgent requests are granted in a row.
func TestAPIPoolBulkNotStarved(t *testing.T) {
	pool, pp := priorityPool(t, 1, 0)
	tr := model.Job{ID: "tr", ScopeID: "L1", Stage: model.StageTranslate, CreatedAt: "2026-01-01T00:00:00Z"}
	rep := model.Job{ID: "rep", ScopeID: "L2", Stage: model.StageRepair, CreatedAt: "2026-01-02T00:00:00Z"}
	pool.setPriority(tr, "2026-01-01T00:00:00Z")
	pool.setPriority(rep, "2026-01-02T00:00:00Z")
	bg := context.Background()
	var reqs []labelled
	for range 3 {
		reqs = append(reqs, labelled{WithJob(bg, tr), "B"})
	}
	for range 8 {
		reqs = append(reqs, labelled{WithJob(bg, rep), "U"})
	}
	order := strings.Join(grantOrder(t, pool, pp, reqs), "")
	if want := "UUUBUUUBUUB"; order != want {
		t.Fatalf("grant order = %s, want %s (UrgentBurst %d)", order, want, UrgentBurst)
	}
}

// TestAPIPoolAdaptiveLimit (perf-llm): a throttle halves the effective
// limit from what was in flight; throttles of requests sent before the cut
// do not cut again; `limit` answers raise it by one; Full follows it; a
// changed max_concurrency resets it.
func TestAPIPoolAdaptiveLimit(t *testing.T) {
	pool, pp := priorityPool(t, 8, 0)
	var rels []func()
	for range 8 {
		rel, err := pool.Acquire(context.Background(), "deepseek")
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sent := time.Now()
	rels[7]() // the throttled request released its slot before reporting
	pool.Throttled("deepseek", sent)
	if l, m, _ := pool.Limit("deepseek"); l != 4 || m != 8 {
		t.Fatalf("after a throttle: limit %d / %d, want 4 / 8", l, m)
	}
	pool.Throttled("deepseek", sent) // same burst
	if l, _, _ := pool.Limit("deepseek"); l != 4 {
		t.Fatalf("a throttle of the same burst cut again: limit %d", l)
	}
	if !pool.Full("deepseek") {
		t.Fatal("7 in flight with limit 4 must be full")
	}
	for _, rel := range rels[:6] {
		rel()
	}
	// 1 in flight, limit 4: a new request goes through at once
	rel, err := pool.Acquire(context.Background(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	rels = []func(){rels[6], rel}

	// A later burst (sent after the cut; the clock may be coarse): cut from
	// what is in flight (2 + the released one).
	later := func() time.Time { return time.Now().Add(time.Hour) }
	pool.Throttled("deepseek", later())
	if l, _, _ := pool.Limit("deepseek"); l != 1 {
		t.Fatalf("second throttle: limit %d, want 1", l)
	}
	// Waiters stay queued until the limit grows back.
	got := make(chan func(), 1)
	go func() {
		r, err := pool.Acquire(context.Background(), "deepseek")
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	waitWaiters(t, pp, 1)
	for _, r := range rels {
		r()
	}
	r := <-got // limit 1, nothing in flight
	pool.Succeeded("deepseek")
	if l, _, _ := pool.Limit("deepseek"); l != 2 {
		t.Fatalf("1 answer at limit 1: limit %d, want 2", l)
	}
	for range 2 {
		pool.Succeeded("deepseek")
	}
	if l, _, _ := pool.Limit("deepseek"); l != 3 {
		t.Fatalf("2 answers at limit 2: limit %d, want 3", l)
	}
	for range 3 + 4 + 5 + 6 + 7 + 100 {
		pool.Succeeded("deepseek")
	}
	if l, _, _ := pool.Limit("deepseek"); l != 8 {
		t.Fatalf("limit %d after many answers, want max 8", l)
	}
	r()

	pool.Throttled("deepseek", later().Add(time.Minute))
	pp.mu.Lock()
	pp.setLimitsLocked(16, 0) // the user changed max_concurrency
	pp.mu.Unlock()
	if l, m, _ := pool.Limit("deepseek"); l != 16 || m != 16 {
		t.Fatalf("after a max_concurrency change: %d / %d", l, m)
	}
	pool.Throttled("deepseek", later().Add(2*time.Hour))
	pp.mu.Lock()
	pp.setLimitsLocked(16, 600) // same max (e.g. an rpm edit): the adaptive limit stays
	pp.mu.Unlock()
	if l, _, _ := pool.Limit("deepseek"); l != 1 {
		t.Fatalf("an unrelated settings change reset the adaptive limit: %d", l)
	}
	pool.Throttled("unknown", time.Now()) // no panic
	pool.Succeeded("unknown")
}

// TestAPIPoolNoRPMLimit: rpm 0 means no per-minute limit (the DeepSeek
// preset, perf-llm): many requests in a row are granted at once.
func TestAPIPoolNoRPMLimit(t *testing.T) {
	pool, _ := priorityPool(t, 64, 0)
	start := time.Now()
	for range 500 {
		rel, err := pool.Acquire(context.Background(), "deepseek")
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("500 requests without a per-minute limit took %v", d)
	}
	if st := pool.Status()["deepseek"]; st.RPM != 0 || st.Max != 64 {
		t.Fatalf("status %+v", st)
	}
}
