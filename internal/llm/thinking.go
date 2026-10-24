// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"log/slog"
	"strings"
	"time"

	"lectureflow/internal/model"
)

// Per-role "深度思考" (.claude/thinking-brief.md section 2; T1). A role's
// level (model.RoleParams.Thinking: off, low, high, max) reaches the
// request through its target (snapshot or current setting). Off — the
// default, also every stray value — leaves every request body byte for
// byte as it was before the option existed (thinking_off_bodies.golden).
//
// DeepSeek (OpenAI-compatible, settings.IsDeepSeek):
//
//	off: "thinking": {"type": "disabled"} (appendix D2, unchanged)
//	on:  "thinking": {"type": "enabled"}, "reasoning_effort": "<level>",
//	     no temperature (ignored in thinking mode), max_tokens = the role's
//	     limit (DeepSeekDefaultMaxTokens when it sends none) + ThinkingBudget.
//	     Only message.content is read; reasoning_content is ignored.
//
// Anthropic (Messages API), max_tokens = the role's limit + ThinkingBudget,
// no temperature either way:
//
//	off: no thinking field (unchanged)
//	on, current Claude models (anthropicAdaptive):
//	     "thinking": {"type": "adaptive"}, "output_config": {"effort": "<level>"}
//	on, older Claude models and non-Claude model names (DeepSeek's
//	     Anthropic-compatible endpoint and the like):
//	     "thinking": {"type": "enabled", "budget_tokens": ThinkingBudget}
//	Thinking blocks of the answer are skipped (parseAnthropic).
//
// Other OpenAI-compatible providers do not support it (settings.SupportsThinking);
// a level stored for one anyway is sent as off and logged once.
//
// A truncated answer is retried once with max_tokens doubled, as without
// thinking (appendix D2). Each attempt's timeout grows by
// ThinkingTimeoutPerToken per budget token.

// Thinking budgets (tokens) of the levels: Anthropic's budget_tokens and,
// for every provider, the amount max_tokens is raised by.
const (
	ThinkingBudgetLow  = 2048
	ThinkingBudgetHigh = 8192
	ThinkingBudgetMax  = 24576
)

// ThinkingBudget returns the budget of a thinking level (0 for off and for
// any value that is not a level).
func ThinkingBudget(level string) int {
	switch level {
	case model.ThinkingLow:
		return ThinkingBudgetLow
	case model.ThinkingHigh:
		return ThinkingBudgetHigh
	case model.ThinkingMax:
		return ThinkingBudgetMax
	}
	return 0
}

// DeepSeekDefaultMaxTokens is DeepSeek's documented output limit in
// non-thinking mode when a request sends no max_tokens (8K; NOTES.md v2
// llm-builder): the "original limit" of the roles that send none, raised by
// the budget when thinking is on (DeepSeek's own default in thinking mode
// is far larger, but a request without max_tokens cannot be retried with a
// doubled limit when it is cut off).
const DeepSeekDefaultMaxTokens = 8192

// ThinkingTimeoutPerToken is how much longer one attempt may take per
// budget token when thinking is on (assumes at least 25 tokens/s): low
// adds about 82 s, high 5.5 min, max 16.4 min to DefaultTimeout. Without it
// a long reasoning answer would hit the 120 s timeout of SPEC §12 and be
// sent (and billed) again. Cancellation is unaffected.
const ThinkingTimeoutPerToken = 40 * time.Millisecond

// thinkingTimeout is the timeout of one attempt with the given budget.
func thinkingTimeout(base time.Duration, budget int) time.Duration {
	return base + time.Duration(budget)*ThinkingTimeoutPerToken
}

// anthropicLegacyModels are the Claude models that take thinking as
// {"type": "enabled", "budget_tokens": N}: the models before Claude Opus
// 4.6 / Sonnet 4.6. An entry matches the id itself and every id that
// continues it with "-", "@" or "." (dated snapshots, "-latest", "2.1",
// Vertex's "@date"). Every later Claude model takes {"type": "adaptive"} with
// output_config.effort; Opus 4.7 and later (Opus 5.5, Sonnet 5.5, Haiku
// 5.5, Fable, ...) reject budget_tokens with a 400, and on Opus/Sonnet 4.6
// it is deprecated (claude-api documentation, 2026-10; NOTES.md,
// 决定（深度思考 T1）).
var anthropicLegacyModels = []string{
	// Claude 1/2 and every Claude 3 model (3, 3.5, 3.7)
	"claude-instant", "claude-2", "claude-3",
	// Opus 4 and Sonnet 4 (aliases and dated ids)
	"claude-opus-4-0", "claude-opus-4-20250514", "claude-sonnet-4-0", "claude-sonnet-4-20250514",
	// Opus 4.1 and the 4.5 family
	"claude-opus-4-1", "claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5",
}

// anthropicAdaptive reports whether modelName takes adaptive thinking with
// an effort level: a Claude model id ("claude-…", any case) that is not
// one of anthropicLegacyModels. Non-Claude names (an Anthropic-compatible
// endpoint of another vendor, such as DeepSeek's) get the classic budget
// form, which those endpoints implement.
func anthropicAdaptive(modelName string) bool {
	id := strings.ToLower(strings.TrimSpace(modelName))
	if !strings.HasPrefix(id, "claude-") {
		return false
	}
	for _, p := range anthropicLegacyModels {
		if id == p || strings.HasPrefix(id, p) && strings.ContainsRune("-@.", rune(id[len(p)])) {
			return false
		}
	}
	return true
}

// anthropicThinking is the thinking field of a Messages API request.
type anthropicThinking struct {
	Type         string `json:"type"`                    // "adaptive" | "enabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // "enabled" only
}

// anthropicOutputConfig carries the effort of adaptive thinking.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

// anthropicThinkingFields returns the thinking fields of a request with
// thinking level (nil, nil when off).
func anthropicThinkingFields(modelName, level string) (*anthropicThinking, *anthropicOutputConfig) {
	budget := ThinkingBudget(level)
	if budget == 0 {
		return nil, nil
	}
	if anthropicAdaptive(modelName) {
		return &anthropicThinking{Type: "adaptive"}, &anthropicOutputConfig{Effort: level}
	}
	return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, nil
}

// supportsThinking is settings.SupportsThinking for a resolved target.
func supportsThinking(ptype string, deepseek bool) bool {
	return ptype == model.ProviderAnthropic || (ptype == model.ProviderOpenAI && deepseek)
}

// thinkingFor returns the level a request of role to the target sends:
// the params' level when it is valid and the provider supports thinking,
// otherwise off. A level that cannot be sent (should not occur: SetRole
// refuses it) is logged once per client, provider, role and value.
func (c *HTTPClient) thinkingFor(role, providerName, ptype string, deepseek bool, p model.RoleParams) string {
	raw := model.ThinkingOf(p)
	lvl := model.ThinkingLevel(p)
	why := ""
	switch {
	case raw != lvl:
		why = "not a thinking level"
	case lvl != model.ThinkingOff && !supportsThinking(ptype, deepseek):
		why = "the provider does not support thinking"
		lvl = model.ThinkingOff
	}
	if why != "" {
		if _, seen := c.thinkingWarned.LoadOrStore(providerName+"\x00"+role+"\x00"+raw, true); !seen {
			slog.Warn("llm: thinking sent as off", "role", role, "provider", providerName, "thinking", raw, "reason", why)
		}
	}
	return lvl
}
