// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"log/slog"

	"lectureflow/internal/model"
)

// The scheduler owns job.stats while a job runs (StatsSink, stage.go). Each
// run keeps its stats in memory: AddUsage/Update only touch that copy and
// never block on I/O; the flusher persists it (SetJobStats) every flush
// interval, and finish does a last write before the final status
// transition, whatever the outcome.
//
// v1.1: the daily usage (usage_daily, budget, usage_today) is no longer
// derived from job.stats here. The llm client records every response with
// Pool.RecordUsage the moment it arrives (A10.3); the v1 kv usage_day:*
// ledger is not written any more.

// AddUsage implements StatsSink: tokens of one LLM response, Requests+1,
// and the estimated cost of that response when a price is configured.
func (r *run) AddUsage(promptTokens, completionTokens, cacheHitTokens int64) {
	d := model.JobStats{PromptTokens: promptTokens, CompletionTokens: completionTokens, CacheHitTokens: cacheHitTokens}
	cost, priced := 0.0, false
	if r.cfgCost != nil {
		cost, priced = r.cfgCost(d)
	}
	r.addUsage(promptTokens, completionTokens, cacheHitTokens, cost, priced)
}

// addUsage adds one response's tokens, Requests+1 and, when priced, its cost.
func (r *run) addUsage(promptTokens, completionTokens, cacheHitTokens int64, cost float64, priced bool) {
	r.smu.Lock()
	r.stats.PromptTokens += promptTokens
	r.stats.CompletionTokens += completionTokens
	r.stats.CacheHitTokens += cacheHitTokens
	r.stats.Requests++
	if priced {
		r.stats.EstimatedCost += cost
	}
	r.statsDirty = true
	r.smu.Unlock()
}

// ReportUsageCost is ReportUsage with the estimated cost of the response
// as the API pool priced it (v2 P12.5: by the model's price in the period the
// request started in). With priced false — or a sink that is not the
// scheduler's run — the sink prices the tokens itself, as ReportUsage does.
func ReportUsageCost(ctx context.Context, promptTokens, completionTokens, cacheHitTokens int64, cost float64, priced bool) {
	s, ok := ctx.Value(statsKey{}).(StatsSink)
	if !ok {
		return
	}
	if r, isRun := s.(*run); isRun && priced {
		r.addUsage(promptTokens, completionTokens, cacheHitTokens, cost, true)
		return
	}
	s.AddUsage(promptTokens, completionTokens, cacheHitTokens)
}

// Update implements StatsSink: mutates non-token fields of the in-memory
// stats. Token counters and the estimated cost are owned by AddUsage; changes
// fn makes to them are discarded.
func (r *run) Update(fn func(*model.JobStats)) {
	r.smu.Lock()
	defer r.smu.Unlock()
	tokens := r.stats
	fn(&r.stats)
	r.stats.PromptTokens, r.stats.CompletionTokens, r.stats.CacheHitTokens = tokens.PromptTokens, tokens.CompletionTokens, tokens.CacheHitTokens
	r.stats.Requests, r.stats.EstimatedCost = tokens.Requests, tokens.EstimatedCost
	r.statsDirty = true
}

func (r *run) statsSnapshot() (model.JobStats, bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	st, dirty := r.stats, r.statsDirty
	r.statsDirty = false
	return st, dirty
}

// persistStatsLocked writes the in-memory stats (after applying final, if
// non-nil). Returns the stats it wrote. Caller holds r.wmu.
func (s *Scheduler) persistStatsLocked(ctx context.Context, r *run, final func(*model.JobStats)) model.JobStats {
	st, dirty := r.statsSnapshot()
	if final != nil {
		final(&st)
		dirty = true
	}
	if dirty {
		if err := s.st.SetJobStats(ctx, r.job.ID, st); err != nil {
			slog.Warn("job stats", "job", r.job.ID, "err", err)
			r.smu.Lock()
			r.statsDirty = true // try again on the next flush
			r.smu.Unlock()
		}
	}
	return st
}
