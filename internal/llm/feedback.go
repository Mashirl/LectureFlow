// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"time"

	"lectureflow/internal/sched"
)

// RateFeedback is implemented by an API pool that adapts a provider's
// concurrency to its answers (perf-llm; sched.Pool does). The HTTP client
// reports every attempt the pool granted: Throttled after HTTP 429, 503 or
// 529 (sent is when the request went out, so the throttles of one burst
// count once), Succeeded after an answer.
type RateFeedback interface {
	Throttled(provider string, sent time.Time)
	Succeeded(provider string)
}

type sentHookKey struct{}

// WithSentHook returns ctx whose HTTP attempts call fn right before the
// request is sent, after the API pool granted it (every attempt, retries
// included). The stages use it to time the prompt-cache warm-up of a run
// (perf-llm): the first request's prefix should reach the provider before
// the others are sent.
func WithSentHook(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, sentHookKey{}, fn)
}

func sentHookFrom(ctx context.Context) func() {
	fn, _ := ctx.Value(sentHookKey{}).(func())
	return fn
}

var _ RateFeedback = (*sched.Pool)(nil)
