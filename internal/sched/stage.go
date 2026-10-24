// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package sched runs stages under resource pools (SPEC §5).
//
// The Stage interface, ResourceKind, the job context helpers and the error
// classification below are FROZEN in Phase 0 (SPEC §16.2).
package sched

import (
	"context"
	"errors"
	"sync"

	"lectureflow/internal/model"
)

type ResourceKind string

const (
	ResourceCPU ResourceKind = "cpu"
	ResourceGPU ResourceKind = "gpu"
	ResourceAPI ResourceKind = "api"
)

// Stage is one kind of automatic task. scopeID is a lecture, material or
// course id depending on model.ScopeOfStage(Name()).
type Stage interface {
	Name() string
	Resource() ResourceKind
	// InputHash computes the SHA-256 of the stage's inputs (SPEC §4.4). It is
	// called when the stage starts (stored in job.input_hash) and by the
	// invalidation check. Stages without a defined hash return "".
	InputHash(ctx context.Context, scopeID string) (string, error)
	// Run does the work. progress takes values in [0,1]. ctx is cancelled on
	// user cancel; Run must then return promptly (within seconds).
	// Run must write its outputs atomically (tmp file + rename / one DB
	// transaction where SPEC requires it).
	Run(ctx context.Context, scopeID string, progress func(float64)) error
}

// Provider is implemented by API stages to tell the scheduler which LLM
// provider they will use (for api_slot / budget waiting).
type ProviderStage interface {
	Provider() string
}

// APIPool is the per-provider concurrency semaphore + token bucket of SPEC §5,
// shared by every request of every API stage. Implemented by sched; used by
// the llm client around each HTTP request.
type APIPool interface {
	// Acquire blocks until the provider has a free slot, a rate token, and
	// the daily budget (SPEC §17.7) is not exhausted. It returns ctx.Err() on cancel.
	Acquire(ctx context.Context, provider string) (release func(), err error)
}

// Events receives notifications for SSE (SPEC §8.2). Implemented by api.
// Calls must not block.
type Events interface {
	Job(j model.Job)         // status or progress changed (throttling is the receiver's job)
	Changed(kind, id string) // kind: lecture|course|sentences|patches|terms|system|devices (SPEC-v2 I8, id "")
}

// NopEvents discards events (tests).
type NopEvents struct{}

func (NopEvents) Job(model.Job)          {}
func (NopEvents) Changed(string, string) {}

type jobKey struct{}

// WithJob attaches the running job to ctx. The scheduler does this before Run.
func WithJob(ctx context.Context, j model.Job) context.Context {
	return context.WithValue(ctx, jobKey{}, j)
}

// JobFrom returns the job the stage is running for (stats, logs, file names).
func JobFrom(ctx context.Context) (model.Job, bool) {
	j, ok := ctx.Value(jobKey{}).(model.Job)
	return j, ok
}

// StatsSink receives the running job's statistics. The scheduler installs one
// per run (lead decision after Gate A round 2, NOTES.md): it is the ONLY
// writer of job.stats while a job runs and persists the accumulated values on
// every flush and before the final status, whatever the outcome. Stages must
// not call Store.SetJobStats for a running job; they use UpdateStats.
type StatsSink interface {
	// AddUsage adds the token usage of one LLM response (called by the llm client).
	AddUsage(promptTokens, completionTokens, cacheHitTokens int64)
	// Update mutates non-token fields (status, note, prompt_terms_used, ...).
	Update(fn func(*model.JobStats))
}

type statsKey struct{}

// WithStatsSink attaches sink to ctx (done by the scheduler before Run).
func WithStatsSink(ctx context.Context, sink StatsSink) context.Context {
	return context.WithValue(ctx, statsKey{}, sink)
}

// ReportUsage forwards one response's usage to the run's sink, if any.
func ReportUsage(ctx context.Context, promptTokens, completionTokens, cacheHitTokens int64) {
	if s, ok := ctx.Value(statsKey{}).(StatsSink); ok {
		s.AddUsage(promptTokens, completionTokens, cacheHitTokens)
	}
}

// UpdateStats mutates the running job's stats through the run's sink, if any.
func UpdateStats(ctx context.Context, fn func(*model.JobStats)) {
	if s, ok := ctx.Value(statsKey{}).(StatsSink); ok {
		s.Update(fn)
	}
}

// StatsRecorder is an in-memory StatsSink for tests of stages run directly.
type StatsRecorder struct {
	mu    sync.Mutex
	Stats model.JobStats
}

func (r *StatsRecorder) AddUsage(p, c, h int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Stats.PromptTokens += p
	r.Stats.CompletionTokens += c
	r.Stats.CacheHitTokens += h
	r.Stats.Requests++
}

func (r *StatsRecorder) Update(fn func(*model.JobStats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.Stats)
}

// Snapshot returns a copy of the recorded stats.
func (r *StatsRecorder) Snapshot() model.JobStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Stats
}

// permanentError marks a failure that must not be retried automatically:
// worker exit code 2 (SPEC §6.0), HTTP 400/401/403 (SPEC §12).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the scheduler skips automatic retries.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}
