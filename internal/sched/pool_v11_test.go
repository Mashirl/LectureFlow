// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// v1.1 API pool: providers from the settings service (A5.4 hot updates),
// usage_daily (A10.3) and the budget from it (SPEC §17.7).

func poolSettings(t *testing.T) (db.Store, *settings.Service) {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(context.Background(), model.KVSettingsImported, "1") // no legacy import
	set, err := settings.Open(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st, set
}

func mkProvider(t *testing.T, set *settings.Service, name string, max, rpm int, prices ...float64) model.Provider {
	t.Helper()
	typ, url, key := model.ProviderOpenAI, "https://api.example.com", "sk-pool-test"
	in := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, MaxConcurrency: &max, RPM: &rpm}
	if len(prices) == 3 {
		in.PriceInput, in.PriceCacheHit, in.PriceOutput = &prices[0], &prices[1], &prices[2]
	}
	p, err := set.CreateProvider(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func patchLimits(t *testing.T, set *settings.Service, id string, max, rpm int) {
	t.Helper()
	in := settings.ProviderInput{}
	if max > 0 {
		in.MaxConcurrency = &max
	}
	if rpm > 0 {
		in.RPM = &rpm
	}
	if _, err := set.UpdateProvider(context.Background(), id, in); err != nil {
		t.Fatal(err)
	}
}

// acquireAsync starts an Acquire and reports its result on the channel.
func acquireAsync(p *Pool, ctx context.Context, provider string) (chan func(), chan error) {
	rel, errc := make(chan func(), 1), make(chan error, 1)
	go func() {
		r, err := p.Acquire(ctx, provider)
		if err != nil {
			errc <- err
			return
		}
		rel <- r
	}()
	return rel, errc
}

func expectBlocked(t *testing.T, what string, rel chan func(), errc chan error) {
	t.Helper()
	select {
	case <-rel:
		t.Fatalf("%s: granted, want blocked", what)
	case err := <-errc:
		t.Fatalf("%s: error %v, want blocked", what, err)
	case <-time.After(60 * time.Millisecond):
	}
}

func expectGranted(t *testing.T, what string, rel chan func(), errc chan error) func() {
	t.Helper()
	select {
	case r := <-rel:
		return r
	case err := <-errc:
		t.Fatalf("%s: %v", what, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: not granted", what)
	}
	return nil
}

func TestPoolWithSettingsHotUpdate(t *testing.T) {
	_, set := poolSettings(t)
	a := mkProvider(t, set, "ProvA", 2, 10000)
	pool := NewPoolWithSettings(nil, set, PoolOptions{})
	ctx := context.Background()

	if st := pool.Status(); len(st) != 1 || st[a.ID] != (APIStat{Name: "ProvA", Inflight: 0, Max: 2, RPM: 10000}) {
		t.Fatalf("status = %+v", st)
	}
	r1, _ := pool.Acquire(ctx, a.ID)
	r2, _ := pool.Acquire(ctx, a.ID)
	if !pool.Full(a.ID) {
		t.Fatal("full")
	}
	if in, max, ok := pool.ProviderStatus(a.ID); !ok || in != 2 || max != 2 {
		t.Fatalf("ProviderStatus = %d/%d %v", in, max, ok)
	}
	rel3, err3 := acquireAsync(pool, ctx, a.ID)
	expectBlocked(t, "third at max 2", rel3, err3)

	// Raising max_concurrency lets the waiter through at once (A5.4).
	patchLimits(t, set, a.ID, 3, 0)
	r3 := expectGranted(t, "third after max 3", rel3, err3)
	if in, max, _ := pool.ProviderStatus(a.ID); in != 3 || max != 3 {
		t.Fatalf("after raise: %d/%d", in, max)
	}

	// Lowering it never interrupts requests in flight; new ones wait until
	// the provider is below the new limit.
	patchLimits(t, set, a.ID, 1, 0)
	rel4, err4 := acquireAsync(pool, ctx, a.ID)
	expectBlocked(t, "fourth at max 1 with 3 in flight", rel4, err4)
	r1()
	r2()
	expectBlocked(t, "fourth with 1 in flight", rel4, err4)
	r3()
	r4 := expectGranted(t, "fourth", rel4, err4)
	r4()

	// requests_per_minute: 6 = one token per 10 s, burst 1.
	patchLimits(t, set, a.ID, 10, 6)
	if st := pool.Status()[a.ID]; st.RPM != 6 || st.Max != 10 {
		t.Fatalf("rpm status %+v", st)
	}
	r, err := pool.Acquire(ctx, a.ID) // the one token in the bucket
	if err != nil {
		t.Fatal(err)
	}
	r()
	rel5, err5 := acquireAsync(pool, ctx, a.ID)
	expectBlocked(t, "no token left at 6 rpm", rel5, err5)
	// A faster bucket applies to the waiter at once: at the old rate its
	// token would come about 10 s after the first one, far beyond
	// expectGranted's 2 s. (At 60 rpm the test had to time the grant
	// against 500 ms, settings write included: too tight on a loaded
	// machine, 708 ms on 2026-10-28.)
	patchLimits(t, set, a.ID, 0, 10000)
	expectGranted(t, "after rpm raise", rel5, err5)()

	// A provider created later appears; renames show in Status.
	b := mkProvider(t, set, "ProvB", 1, 10000)
	rb, err := pool.Acquire(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := "ProvA renamed"
	set.UpdateProvider(ctx, a.ID, settings.ProviderInput{Name: &name})
	if st := pool.Status(); len(st) != 2 || st[a.ID].Name != name || st[b.ID].Inflight != 1 {
		t.Fatalf("status = %+v", st)
	}
	if got := pool.Providers(); len(got) != 2 {
		t.Fatalf("providers = %v", got)
	}

	// Deleting a provider fails its waiting and new requests immediately;
	// the request in flight finishes normally.
	relW, errW := acquireAsync(pool, ctx, b.ID)
	expectBlocked(t, "waiter on B", relW, errW)
	if _, err := set.DeleteProvider(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errW:
		var ce *model.CodedError
		if !IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != CodeProviderDeleted || ce.Info.Params["provider"] != "ProvB" {
			t.Fatalf("waiter error %v", err)
		}
	case <-relW:
		t.Fatal("waiter granted on a deleted provider")
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after delete")
	}
	if _, err := pool.Acquire(ctx, b.ID); err == nil || !IsPermanent(err) {
		t.Fatalf("new request on deleted provider: %v", err)
	}
	if _, _, ok := pool.ProviderStatus(b.ID); ok || pool.Full(b.ID) {
		t.Fatal("deleted provider still listed")
	}
	rb() // releasing the in-flight slot of a deleted provider is harmless
	if _, ok := pool.Status()[b.ID]; ok {
		t.Fatal("deleted provider in Status")
	}
}

func TestPoolCancelWhileWaitingKeepsSlots(t *testing.T) {
	_, set := poolSettings(t)
	a := mkProvider(t, set, "A", 1, 10000)
	pool := NewPoolWithSettings(nil, set, PoolOptions{})
	r, _ := pool.Acquire(context.Background(), a.ID)
	ctx, cancel := context.WithCancel(context.Background())
	rel, errc := acquireAsync(pool, ctx, a.ID)
	expectBlocked(t, "waiter", rel, errc)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	r()
	if in, _, _ := pool.ProviderStatus(a.ID); in != 0 {
		t.Fatalf("leaked slot: %d", in)
	}
}

func TestUsageDailyAndBudgetWithSettings(t *testing.T) {
	st, set := poolSettings(t)
	ctx := context.Background()
	priced := mkProvider(t, set, "Priced", 4, 600, 1, 0.1, 2)
	free := mkProvider(t, set, "Unpriced", 4, 600)
	set.SetGeneral(ctx, model.GeneralSettings{KeepOriginalMedia: true, Notifications: true, DailyBudget: 1.0})
	var clock atomic.Value
	clock.Store(time.Now())
	now := func() time.Time { return clock.Load().(time.Time) }
	pool := NewPoolWithSettings(st, set, PoolOptions{Now: now, BudgetPoll: 5 * time.Millisecond})

	u, err := pool.UsageToday(ctx)
	if err != nil || u.EstimatedCost == nil || *u.EstimatedCost != 0 || pool.BudgetExceeded(ctx) {
		t.Fatalf("empty day: %+v %v", u, err)
	}
	// Tokens of a provider without prices count, but cost nothing.
	pool.RecordUsage(ctx, free.ID, 5_000_000, 0, 5_000_000)
	if pool.BudgetExceeded(ctx) {
		t.Fatal("unpriced usage exceeded the budget")
	}
	// 0.6M uncached × 1 + 0.4M cached × 0.1 + 0.2M output × 2 = 1.04 ≥ 1.0.
	pool.RecordUsage(ctx, priced.ID, 600_000, 400_000, 100_000)
	pool.RecordUsage(ctx, priced.ID, 400_000, 0, 100_000)
	u, _ = pool.UsageToday(ctx)
	if u.PromptTokens != 6_000_000 || u.CacheHitTokens != 400_000 || u.CompletionTokens != 5_200_000 ||
		u.EstimatedCost == nil || math.Abs(*u.EstimatedCost-1.04) > 1e-9 {
		t.Fatalf("usage = %+v cost %v", u, *u.EstimatedCost)
	}
	if !pool.BudgetExceeded(ctx) {
		t.Fatal("budget should be exceeded (the cache is refreshed by RecordUsage)")
	}
	rows, _ := st.ListUsageDaily(ctx, "")
	if len(rows) != 2 {
		t.Fatalf("usage_daily rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Day != dayKey(now()) {
			t.Fatalf("day %q", r.Day)
		}
		switch r.ProviderID {
		case priced.ID:
			if r.Requests != 2 || r.PromptTokens != 1_000_000 || r.CacheHitTokens != 400_000 || r.CompletionTokens != 200_000 {
				t.Fatalf("priced row %+v", r)
			}
		case free.ID:
			if r.Requests != 1 {
				t.Fatalf("free row %+v", r)
			}
		default:
			t.Fatalf("row %+v", r)
		}
	}
	if c, ok := pool.ProviderCost(priced.ID, 1_000_000, 400_000, 200_000); !ok || math.Abs(c-1.04) > 1e-9 {
		t.Fatalf("ProviderCost = %v %v", c, ok)
	}
	if _, ok := pool.ProviderCost(free.ID, 1, 0, 1); ok {
		t.Fatal("ProviderCost without prices")
	}

	// Acquire holds while over budget and reports it for the job.
	jctx, cancel := context.WithCancel(WithJob(ctx, model.Job{ID: "J1"}))
	rel, errc := acquireAsync(pool, jctx, free.ID)
	deadline := time.Now().Add(2 * time.Second)
	for !pool.BudgetBlocked("J1") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !pool.BudgetBlocked("J1") {
		t.Fatal("not budget blocked")
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}

	// The budget comes from the general settings and applies at once.
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 2})
	if pool.BudgetExceeded(ctx) {
		t.Fatal("budget raised to 2: not exceeded")
	}
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 0})
	if pool.BudgetExceeded(ctx) {
		t.Fatal("budget 0 = no budget")
	}
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 1})
	if !pool.BudgetExceeded(ctx) {
		t.Fatal("budget 1 again: exceeded")
	}

	// A blocked request resumes on the next local day.
	rel, errc = acquireAsync(pool, ctx, free.ID)
	expectBlocked(t, "over budget", rel, errc)
	clock.Store(time.Now().Add(48 * time.Hour))
	expectGranted(t, "next day", rel, errc)()
	if u, _ := pool.UsageToday(ctx); u.PromptTokens != 0 {
		t.Fatalf("next day usage = %+v", u)
	}

	// Without any price the cost is unknown (nil) and there is no budget.
	clock.Store(time.Now())
	set.UpdateProvider(ctx, priced.ID, settings.ProviderInput{ClearPrices: true})
	if u, _ := pool.UsageToday(ctx); u.EstimatedCost != nil || pool.BudgetExceeded(ctx) {
		t.Fatalf("no prices: %+v", u)
	}

	// No store: nothing recorded, nothing panics.
	NewPoolWithSettings(nil, set, PoolOptions{}).RecordUsage(ctx, priced.ID, 1, 1, 1)
}

func TestLegacyPoolUsesConfigPrices(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.LLM.Prices = map[string]map[string]config.Price{"deepseek": {"deepseek-chat": {Input: 2, CacheHit: 0.5, Output: 8}}}
	pool := NewPool(cfg, st, PoolOptions{})
	if c, ok := pool.ProviderCost("deepseek", 3_000_000, 2_000_000, 500_000); !ok || c != 2+1+4 {
		t.Fatalf("legacy ProviderCost = %v %v", c, ok)
	}
	pool.RecordUsage(context.Background(), "deepseek", 3_000_000, 2_000_000, 500_000)
	u, _ := pool.UsageToday(context.Background())
	if u.EstimatedCost == nil || *u.EstimatedCost != 7 || u.PromptTokens != 3_000_000 {
		t.Fatalf("legacy usage %+v", u)
	}
	if st := pool.Status()["deepseek"]; st.Name != "deepseek" || st.Max != 64 || st.RPM != 0 { // perf-llm: the DeepSeek default
		t.Fatalf("legacy status %+v", st)
	}
	// The v1 kv ledger is not written any more.
	if _, ok, _ := st.GetKV(context.Background(), "usage_day:"+dayKey(time.Now())); ok {
		t.Fatal("kv usage_day ledger written")
	}
}
