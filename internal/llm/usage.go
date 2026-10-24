// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// CostRecorder is the v2 extension of UsageRecorder (SPEC-v2 P12.5): it also
// gets the model and the moment the request was sent, prices the request in
// the period that moment falls in (tariff.State, tariff.Cost), records tokens
// and cost in usage_daily, and returns the cost (priced false: no price, only
// tokens). When the client's recorder implements it, RecordRequest is called
// instead of RecordUsage, and the cost goes to the job's stats as well
// (sched.ReportUsageCost). sched.Pool implements it.
type CostRecorder interface {
	RecordRequest(ctx context.Context, providerID, model string, start time.Time, promptTokens, cacheHitTokens, completionTokens int64) (cost float64, priced bool)
}

// Totals is a snapshot of accumulated token usage (SPEC §12, §17.7).
type Totals struct {
	PromptTokens     int64 // includes cache-hit tokens (DeepSeek semantics)
	CompletionTokens int64
	CacheHitTokens   int64
	Requests         int64 // successful completions (and truncated Anthropic answers, which are billed)
}

// Usage accumulates the usage of every completion made with a context that
// carries it (WithUsage). Safe for concurrent use. Stages do NOT use it to
// write job.stats: the client reports every response to the scheduler's
// stats sink (sched.ReportUsage), which owns job.stats during a run.
type Usage struct {
	mu sync.Mutex
	t  Totals
}

// Add records one completion.
func (u *Usage) Add(r Result) {
	u.mu.Lock()
	u.t.PromptTokens += r.PromptTokens
	u.t.CompletionTokens += r.CompletionTokens
	u.t.CacheHitTokens += r.CacheHitTokens
	u.t.Requests++
	u.mu.Unlock()
}

// Totals returns the current sums.
func (u *Usage) Totals() Totals {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.t
}

type usageKey struct{}

// WithUsage makes every Complete call under ctx add its usage to u. A stage
// typically does:
//
//	u := &llm.Usage{}
//	ctx = llm.WithUsage(ctx, u)
//	... client.Complete(ctx, ...) ...
//	stats := u.Totals().AddTo(base, price) // base = job.stats at stage start
func WithUsage(ctx context.Context, u *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, u)
}

// UsageFrom returns the accumulator attached to ctx, or nil.
func UsageFrom(ctx context.Context) *Usage {
	u, _ := ctx.Value(usageKey{}).(*Usage)
	return u
}

// Cost estimates the cost with per-million-token prices (SPEC §17.7):
// cache-miss prompt tokens at Input, cache-hit tokens at CacheHit, completion
// tokens at Output.
func (t Totals) Cost(p config.Price) float64 {
	miss := t.PromptTokens - t.CacheHitTokens
	if miss < 0 {
		miss = 0
	}
	return (float64(miss)*p.Input + float64(t.CacheHitTokens)*p.CacheHit + float64(t.CompletionTokens)*p.Output) / 1e6
}

// AddTo returns st with these totals added (token counts, requests and,
// when price is non-nil, the estimated cost). Use the job.stats from the
// start of the run as st, so a retried job keeps the usage of earlier attempts.
func (t Totals) AddTo(st model.JobStats, price *config.Price) model.JobStats {
	st.PromptTokens += t.PromptTokens
	st.CompletionTokens += t.CompletionTokens
	st.CacheHitTokens += t.CacheHitTokens
	st.Requests += t.Requests
	if price != nil {
		st.EstimatedCost += t.Cost(*price)
	}
	return st
}

// PriceForRole returns the configured price of role's provider/model, or nil.
func PriceForRole(cfg *config.Config, role string) *config.Price {
	prov, mod := RoleProvider(cfg, role)
	if p, ok := cfg.PriceFor(prov, mod); ok {
		return &p
	}
	return nil
}
