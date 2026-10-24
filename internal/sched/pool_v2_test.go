// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// v2 P12.5 (per-request cost by the period the request started in) and
// P12.6 (dispatch only off-peak) in the API pool.

func beijing(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// fakeNow is a settable clock for PoolOptions.Now.
type fakeNow struct{ v atomic.Value }

func newFakeNow(t time.Time) *fakeNow { c := &fakeNow{}; c.v.Store(t); return c }
func (c *fakeNow) Now() time.Time     { return c.v.Load().(time.Time) }
func (c *fakeNow) Set(t time.Time)    { c.v.Store(t) }

// mkDeepSeek creates a DeepSeek provider (it gets tariff.DeepSeekPreset()),
// with provider-level prices when given.
func mkDeepSeek(t *testing.T, set *settings.Service, name string, prices ...float64) model.Provider {
	t.Helper()
	typ, url, key, cur := model.ProviderOpenAI, "https://api.deepseek.com", "sk-pool-test", "CNY"
	max, rpm := 4, 600
	in := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, MaxConcurrency: &max, RPM: &rpm}
	if len(prices) == 3 {
		in.PriceInput, in.PriceCacheHit, in.PriceOutput, in.Currency = &prices[0], &prices[1], &prices[2], &cur
	}
	p, err := set.CreateProvider(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tariff == nil {
		t.Fatal("DeepSeek provider without the preset tariff")
	}
	return p
}

func TestPoolPricesEachRequestByItsStart(t *testing.T) {
	st, set := poolSettings(t)
	ctx := context.Background()
	ds := mkDeepSeek(t, set, "DeepSeek", 4, 1, 16)
	modelsOnly := mkDeepSeek(t, set, "DeepSeek without own prices")
	clk := newFakeNow(beijing(t, "2026-10-21 13:00:00")) // Wednesday, recorded after the peak ended
	pool := NewPoolWithSettings(st, set, PoolOptions{Now: clk.Now, BudgetCacheTTL: -1})

	const prompt, hit, completion = 1_000_000, 400_000, 100_000
	flashPeak := (600_000*2.0 + 400_000*0.04 + 100_000*8.0) / 1e6
	fallbackPeak := (600_000*4.0 + 400_000*1.0 + 100_000*16.0) / 1e6
	total := 0.0
	for _, tc := range []struct {
		model, start string
		want         float64
	}{
		{"deepseek-flash", "2026-10-21 11:59:59", flashPeak},     // started in the peak: peak price
		{"deepseek-flash", "2026-10-21 12:00:00", flashPeak / 2}, // started off-peak
		{"deepseek-chat", "2026-10-21 10:00:00", fallbackPeak},   // no model price: the provider's
		{"deepseek-chat", "2026-10-24 10:00:00", fallbackPeak / 2},
	} {
		cost, priced := pool.RecordRequest(ctx, ds.ID, tc.model, beijing(t, tc.start), prompt, hit, completion)
		if !priced || math.Abs(cost-tc.want) > 1e-9 {
			t.Errorf("%s at %s: %v %v, want %v", tc.model, tc.start, cost, priced, tc.want)
		}
		total += tc.want
	}
	// No model price and no provider price: tokens only.
	if cost, priced := pool.RecordRequest(ctx, modelsOnly.ID, "deepseek-chat", beijing(t, "2026-10-21 10:00:00"), prompt, hit, completion); priced || cost != 0 {
		t.Fatalf("unpriced request %v %v", cost, priced)
	}
	if cost, priced := pool.RecordRequest(ctx, modelsOnly.ID, "deepseek-v4-pro", beijing(t, "2026-10-21 10:00:00"), prompt, hit, completion); !priced ||
		math.Abs(cost-(600_000*9.0+400_000*0.3+100_000*27.0)/1e6) > 1e-9 {
		t.Fatalf("v4-pro %v %v", cost, priced)
	}
	total += (600_000*9.0 + 400_000*0.3 + 100_000*27.0) / 1e6

	rows, err := st.ListUsageDaily(ctx, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	for _, r := range rows {
		if r.Day != dayKey(clk.Now()) {
			t.Errorf("day %s", r.Day)
		}
		if r.ProviderID == ds.ID && (r.Requests != 4 || math.Abs(r.EstimatedCost-(flashPeak*1.5+fallbackPeak*1.5)) > 1e-9) {
			t.Errorf("ds row %+v", r)
		}
	}
	// "今日用量" and the budget read the accumulated estimates.
	u, _ := pool.UsageToday(ctx)
	if u.EstimatedCost == nil || math.Abs(*u.EstimatedCost-total) > 1e-9 || u.PromptTokens != 6*prompt {
		t.Fatalf("usage today %+v", u)
	}
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: total + 0.01})
	if pool.BudgetExceeded(ctx) {
		t.Fatal("budget exceeded below the total")
	}
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: total - 0.01})
	if !pool.BudgetExceeded(ctx) {
		t.Fatal("budget not exceeded")
	}
	// RecordUsage (no model, started now = 13:00, off-peak): the provider price, halved.
	before := *u.EstimatedCost
	pool.RecordUsage(ctx, ds.ID, prompt, hit, completion)
	if u, _ = pool.UsageToday(ctx); math.Abs(*u.EstimatedCost-before-fallbackPeak/2) > 1e-9 {
		t.Fatalf("RecordUsage cost %v", *u.EstimatedCost-before)
	}
	if c, ok := pool.ProviderCost(ds.ID, prompt, hit, completion); !ok || math.Abs(c-fallbackPeak/2) > 1e-9 {
		t.Fatalf("ProviderCost %v %v", c, ok)
	}
	// A provider whose only prices are in its tariff counts as priced (D3).
	set.UpdateProvider(ctx, ds.ID, settings.ProviderInput{ClearPrices: true})
	if u, _ = pool.UsageToday(ctx); u.EstimatedCost == nil {
		t.Fatal("tariff model prices count as prices")
	}
}

// The job's stats get the pool's estimate (sched.ReportUsageCost); without
// one the run prices the tokens itself as before.
func TestReportUsageCost(t *testing.T) {
	r := &run{}
	r.cfgCost = func(d model.JobStats) (float64, bool) { return float64(d.PromptTokens) / 1000, true }
	ctx := WithStatsSink(context.Background(), r)
	ReportUsageCost(ctx, 1000, 100, 50, 0.25, true)
	if st := r.stats; st.PromptTokens != 1000 || st.CompletionTokens != 100 || st.CacheHitTokens != 50 || st.Requests != 1 || st.EstimatedCost != 0.25 {
		t.Fatalf("priced %+v", st)
	}
	ReportUsageCost(ctx, 2000, 0, 0, 0, false) // cfgCost: 2.0
	if st := r.stats; st.Requests != 2 || math.Abs(st.EstimatedCost-2.25) > 1e-12 || !r.statsDirty {
		t.Fatalf("fallback %+v", st)
	}
	rec := &StatsRecorder{}
	ReportUsageCost(WithStatsSink(context.Background(), rec), 10, 1, 0, 9, true)
	if rec.Stats.PromptTokens != 10 || rec.Stats.Requests != 1 {
		t.Fatalf("other sinks get AddUsage: %+v", rec.Stats)
	}
	ReportUsageCost(context.Background(), 1, 1, 1, 1, true) // no sink: nothing happens
}

func TestPoolOffpeakOnly(t *testing.T) {
	st, set := poolSettings(t)
	ctx := context.Background()
	ds := mkDeepSeek(t, set, "DeepSeek")
	other := mkProvider(t, set, "Other", 4, 600)
	clk := newFakeNow(beijing(t, "2026-10-21 11:30:00")) // Wednesday, peak until 12:00
	pool := NewPoolWithSettings(st, set, PoolOptions{Now: clk.Now, OffpeakPoll: 5 * time.Millisecond})

	// Switch off: no wait in the peak.
	if w, _ := pool.OffpeakWait(ds.ID, clk.Now()); w {
		t.Fatal("waiting without offpeak_only")
	}
	grant(t, "peak, switch off", pool, WithJob(ctx, model.Job{ID: "J0"}), ds.ID)

	on, off := true, false
	if _, err := set.UpdateProvider(ctx, ds.ID, settings.ProviderInput{OffpeakOnly: &on}); err != nil {
		t.Fatal(err)
	}
	noon := beijing(t, "2026-10-21 12:00:00")
	if w, until := pool.OffpeakWait(ds.ID, clk.Now()); !w || !until.Equal(noon) {
		t.Fatalf("OffpeakWait %v %v", w, until)
	}
	if w, _ := pool.OffpeakWait(other.ID, clk.Now()); w {
		t.Fatal("a provider without tariff waits")
	}
	if w, _ := pool.OffpeakWait(ds.ID, beijing(t, "2026-10-21 12:00:00")); w {
		t.Fatal("off-peak waits")
	}

	// A job's request waits until the off-peak period; other providers and
	// requests without a job (interactive calls) do not.
	rel, errc := acquireAsync(pool, WithJob(ctx, model.Job{ID: "J1"}), ds.ID)
	expectBlocked(t, "peak, offpeak_only", rel, errc)
	if until, ok := pool.OffpeakBlocked("J1"); !ok || !until.Equal(noon) {
		t.Fatalf("OffpeakBlocked %v %v", until, ok)
	}
	if _, ok := pool.OffpeakBlocked("J0"); ok {
		t.Fatal("J0 is not blocked")
	}
	grant(t, "other provider", pool, WithJob(ctx, model.Job{ID: "J2"}), other.ID)
	grant(t, "no job", pool, ctx, ds.ID)
	clk.Set(noon)
	expectGranted(t, "off-peak started", rel, errc)()
	if _, ok := pool.OffpeakBlocked("J1"); ok {
		t.Fatal("still reported blocked")
	}

	// Long polls: only a settings change wakes the waiter early.
	clk.Set(beijing(t, "2026-10-21 15:00:00"))
	slow := NewPoolWithSettings(st, set, PoolOptions{Now: clk.Now, OffpeakPoll: time.Hour})
	rel, errc = acquireAsync(slow, WithJob(ctx, model.Job{ID: "J3"}), ds.ID)
	expectBlocked(t, "afternoon peak", rel, errc)
	if _, err := set.UpdateProvider(ctx, ds.ID, settings.ProviderInput{OffpeakOnly: &off}); err != nil {
		t.Fatal(err)
	}
	expectGranted(t, "switch turned off", rel, errc)()

	// Holidays edited while waiting: today becomes a holiday.
	set.UpdateProvider(ctx, ds.ID, settings.ProviderInput{OffpeakOnly: &on})
	rel, errc = acquireAsync(slow, WithJob(ctx, model.Job{ID: "J4"}), ds.ID)
	expectBlocked(t, "peak again", rel, errc)
	if err := set.SetHolidays(ctx, map[string][]string{"2026": {"2026-10-21"}}); err != nil {
		t.Fatal(err)
	}
	expectGranted(t, "today is a holiday", rel, errc)()
	set.SetHolidays(ctx, nil)

	// Cancelling a waiting request.
	jctx, cancel := context.WithCancel(WithJob(ctx, model.Job{ID: "J5"}))
	rel, errc = acquireAsync(slow, jctx, ds.ID)
	expectBlocked(t, "before cancel", rel, errc)
	deadline := time.Now().Add(2 * time.Second)
	for _, ok := slow.OffpeakBlocked("J5"); !ok && time.Now().Before(deadline); _, ok = slow.OffpeakBlocked("J5") {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not end the wait")
	}
	if _, ok := slow.OffpeakBlocked("J5"); ok {
		t.Fatal("cancelled job still blocked")
	}
	// The slot was never taken.
	if in, _, _ := slow.ProviderStatus(ds.ID); in != 0 {
		t.Fatalf("inflight %d", in)
	}

	// The legacy pool has no tariffs.
	legacy := NewPool(config.Default(), nil, PoolOptions{})
	if w, _ := legacy.OffpeakWait("deepseek", clk.Now()); w {
		t.Fatal("legacy pool waits")
	}
}

// grant acquires and releases at once, failing when not granted.
func grant(t *testing.T, what string, p *Pool, ctx context.Context, provider string) {
	t.Helper()
	rel, errc := acquireAsync(p, ctx, provider)
	expectGranted(t, what, rel, errc)()
}

func TestOffpeakReason(t *testing.T) {
	if got := OffpeakReason(time.Time{}); got != "offpeak:--:--" {
		t.Fatalf("zero %q", got)
	}
	at := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)
	if got, want := OffpeakReason(at), "offpeak:"+at.In(time.Local).Format("15:04"); got != want {
		t.Fatalf("%q want %q", got, want)
	}
}

// D3 (lead ruling at the phase-2 gate): a request without any price (no
// model price, no provider price) is counted per provider and local day;
// today's amount is unknown (nil) when every request was unpriced and is
// shown again once some request is priced. The budget is not enforced on
// unpriced requests (they never block), but BudgetUnpriced reports them
// while a budget applies, for the "budget may be underestimated" note.
func TestPoolCountsUnpricedRequests(t *testing.T) {
	st, set := poolSettings(t)
	ctx := context.Background()
	modelsOnly := mkDeepSeek(t, set, "DeepSeek without own prices") // tariff model prices only
	clk := newFakeNow(beijing(t, "2026-10-21 13:00:00"))
	pool := NewPoolWithSettings(st, set, PoolOptions{Now: clk.Now, BudgetCacheTTL: -1})
	day := dayKey(clk.Now())
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 0.001})

	if u, _ := pool.UsageToday(ctx); u.EstimatedCost == nil || *u.EstimatedCost != 0 || u.Requests != 0 || u.Unpriced != 0 {
		t.Fatalf("empty day %+v", u)
	}
	for i := 0; i < 2; i++ {
		if _, priced := pool.RecordRequest(ctx, modelsOnly.ID, "deepseek-chat", clk.Now(), 1_000_000, 0, 1_000_000); priced {
			t.Fatal("deepseek-chat has no price")
		}
	}
	if n, err := settings.UnpricedRequests(ctx, st, modelsOnly.ID, day); err != nil || n != 2 {
		t.Fatalf("counter %d %v", n, err)
	}
	u, _ := pool.UsageToday(ctx)
	if u.Requests != 2 || u.Unpriced != 2 || u.EstimatedCost != nil || u.PromptTokens != 2_000_000 {
		t.Fatalf("all unpriced: %+v", u)
	}
	if pool.BudgetExceeded(ctx) {
		t.Fatal("unpriced requests must not block (budget 0.001)")
	}
	if n := pool.BudgetUnpriced(ctx); n != 2 {
		t.Fatalf("BudgetUnpriced = %d, want 2", n)
	}

	// A priced request: the amount is known again, with the unpriced count.
	if _, priced := pool.RecordRequest(ctx, modelsOnly.ID, "deepseek-flash", clk.Now(), 1000, 0, 1000); !priced {
		t.Fatal("deepseek-flash has a tariff price")
	}
	if n, _ := settings.UnpricedRequests(ctx, st, modelsOnly.ID, day); n != 2 {
		t.Fatalf("a priced request was counted: %d", n)
	}
	u, _ = pool.UsageToday(ctx)
	if u.Requests != 3 || u.Unpriced != 2 || u.EstimatedCost == nil || *u.EstimatedCost <= 0 {
		t.Fatalf("partly unpriced: %+v", u)
	}
	// No budget: nothing to warn about.
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 0})
	if n := pool.BudgetUnpriced(ctx); n != 0 {
		t.Fatalf("BudgetUnpriced without a budget = %d", n)
	}
	// The next local day starts from zero.
	clk.Set(clk.Now().Add(24 * time.Hour))
	set.SetGeneral(ctx, model.GeneralSettings{DailyBudget: 1})
	if u, _ := pool.UsageToday(ctx); u.Unpriced != 0 || u.Requests != 0 || pool.BudgetUnpriced(ctx) != 0 {
		t.Fatalf("next day %+v", u)
	}
}
