// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// v2 P12.5: every request is priced by the model and the moment it was sent
// (llm.CostRecorder); the cost also reaches the job's stats.

type requestCall struct {
	provider, model         string
	start                   time.Time
	prompt, hit, completion int64
}

// costRecorder is a UsageRecorder that is also a CostRecorder (like sched.Pool).
type costRecorder struct {
	fakeRecorder
	mu    sync.Mutex
	reqs  []requestCall
	cost  float64
	price bool
}

func (r *costRecorder) RecordRequest(ctx context.Context, providerID, model string, start time.Time, prompt, hit, completion int64) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, requestCall{providerID, model, start, prompt, hit, completion})
	return r.cost, r.price
}

func TestCostRecorderGetsModelAndStart(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			time.Sleep(20 * time.Millisecond) // the answer takes a while: start is when it was sent
		}
		w.Write([]byte(okBody(`{"ok":true}`)))
	})
	set := newSettings(t)
	p := addProvider(t, set, "Proxy", model.ProviderOpenAI, srv.URL, testKey)
	setRole(t, set, model.RoleSummary, p.ID, "m-1", model.RoleParams{})
	rec := &costRecorder{cost: 0.5, price: true}
	c := NewWithSettings(set, nil, rec, t.TempDir(), nil)
	c.Backoff = nil

	stats := &sched.StatsRecorder{}
	u := &Usage{}
	ctx := WithUsage(sched.WithStatsSink(context.Background(), stats), u)
	before := time.Now()
	if _, err := c.Complete(ctx, model.RoleSummary, "sys", "user", true); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(rec.reqs) != 1 || len(rec.all()) != 0 {
		t.Fatalf("RecordRequest %d calls, RecordUsage %d calls", len(rec.reqs), len(rec.all()))
	}
	r := rec.reqs[0]
	if r.provider != p.ID || r.model != "m-1" || r.prompt != 100 || r.hit != 64 || r.completion != 20 {
		t.Fatalf("call %+v", r)
	}
	if r.start.Before(before) || r.start.After(after) || after.Sub(r.start) < 20*time.Millisecond {
		t.Fatalf("start %v not when the request was sent (%v .. %v)", r.start, before, after)
	}
	// Other sinks get the tokens as before; the WithUsage totals too.
	if stats.Stats.PromptTokens != 100 || stats.Stats.Requests != 1 || u.Totals().Requests != 1 {
		t.Fatalf("stats %+v usage %+v", stats.Stats, u.Totals())
	}
}

// End to end with the real pool: the tariff's price of the model, recorded
// in usage_daily.estimated_cost.
func TestRequestCostRecordedInUsageDaily(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { w.Write([]byte(okBody(`{"ok":true}`))) })
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	st.SetKV(ctx, model.KVSettingsImported, "1")
	set, err := settings.Open(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Config().DataDir = t.TempDir()
	// Prices per model, no time-of-day pricing: the cost does not depend on
	// when the test runs.
	tf := `{"timezone":"UTC","models":{"m-1":{"input":2,"input_cache_hit":0.5,"output":8,"currency":"CNY"}}}`
	name, typ, url, key := "Proxy", model.ProviderOpenAI, srv.URL, testKey
	p, err := set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, Tariff: &tf})
	if err != nil {
		t.Fatal(err)
	}
	setRole(t, set, model.RoleSummary, p.ID, "m-1", model.RoleParams{})
	setRole(t, set, model.RoleRepair, p.ID, "m-2", model.RoleParams{})
	pool := sched.NewPoolWithSettings(st, set, sched.PoolOptions{})
	c := NewWithSettings(set, pool, pool, t.TempDir(), nil)
	for _, role := range []string{model.RoleSummary, model.RoleRepair} {
		if _, err := c.Complete(ctx, role, "sys", "user", true); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListUsageDaily(ctx, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	// m-1: (100-64)×2 + 64×0.5 + 20×8 per million; m-2 has no price.
	want := (36*2.0 + 64*0.5 + 20*8.0) / 1e6
	if r := rows[0]; r.Requests != 2 || r.PromptTokens != 200 || math.Abs(r.EstimatedCost-want) > 1e-15 {
		t.Fatalf("row %+v, want cost %v", r, want)
	}
}
