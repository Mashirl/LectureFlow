// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package llm is the OpenAI-compatible client (SPEC §12).
//
// The Client interface and Result struct are FROZEN in Phase 0 (SPEC §16.2).
package llm

import "context"

// Result of one chat completion (after transport retries).
type Result struct {
	Text             string
	PromptTokens     int64
	CompletionTokens int64
	CacheHitTokens   int64 // usage.prompt_cache_hit_tokens when present
}

// Client sends one request: system = fixed prefix, user = variable part.
// role is one of config.Role* and selects provider, model and temperature.
// jsonMode sets response_format={"type":"json_object"}.
//
// Errors: HTTP 400/401/403 are returned wrapped with sched.Permanent and the
// message contains "请检查 API key 或模型名". Context cancellation returns ctx.Err().
//
// The client acquires sched.APIPool around every HTTP attempt.
type Client interface {
	Complete(ctx context.Context, role, system, user string, jsonMode bool) (Result, error)
}
