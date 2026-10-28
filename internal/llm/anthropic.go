// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"encoding/json"
	"errors"
	"strings"

	"lectureflow/internal/model"
)

// Anthropic Messages API adapter (SPEC-v1.1 A6). Transport, retries and
// error classification are shared with the OpenAI-compatible path (http.go);
// only the request body, the auth headers and the response differ.

const (
	// AnthropicVersion is the anthropic-version header of A6.
	AnthropicVersion = "2023-06-01"
	// DefaultAnthropicMaxTokens is max_tokens of an Anthropic request for
	// every role without its own value (SPEC-v2 appendix D2: 4000). The
	// Messages API requires the field; thinking counts against it.
	DefaultAnthropicMaxTokens = 4000
	// JanitorAnthropicMaxTokens is max_tokens of the glossary_janitor role
	// (appendix D2): its answer lists actions for a whole glossary.
	JanitorAnthropicMaxTokens = 16000
	// stopMaxTokens is the stop_reason of an answer cut off at max_tokens;
	// appendix D2 counts it as a failure (see HTTPClient.Complete).
	stopMaxTokens = "max_tokens"
	// JSONInstruction is appended to the user message in JSON mode: the
	// Messages API has no response_format (A6). The answer still goes
	// through the tolerant ParseJSON of SPEC §12.
	JSONInstruction = "\n\nRespond with JSON only."
	// StatusOverloaded is Anthropic's 529 overloaded_error, retried like 5xx (A6).
	StatusOverloaded = 529
)

// AnthropicMaxTokensFor returns max_tokens of an Anthropic request of role
// (appendix D2): glossary_janitor 16000; chapters, organizer and every
// other role 4000. A truncated answer is retried once with the limit doubled.
func AnthropicMaxTokensFor(role string) int {
	switch role {
	case model.RoleGlossaryJanitor:
		return JanitorAnthropicMaxTokens
	case model.RoleChapters, model.RoleOrganizer:
		return 4000
	}
	return DefaultAnthropicMaxTokens
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string               `json:"model"`
	MaxTokens int                  `json:"max_tokens"`
	System    []anthropicTextBlock `json:"system,omitempty"`
	Messages  []anthropicMessage   `json:"messages"`
	// Thinking and OutputConfig are set only when the role's "深度思考" is
	// on (thinking.go, anthropicThinkingFields).
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

// anthropicBody builds the request of A6: the fixed prefix as the single
// system block, marked cache_control ephemeral (prompt caching; the prefix
// is byte-identical for one lecture and role, SPEC §12), the variable part
// as the user message.
//
// No temperature is sent: current Claude models reject sampling
// parameters other than the defaults (and API version 2026-10-21 removes
// them on every model), so a role's temperature applies to
// OpenAI-compatible providers only (NOTES.md, v1.1 settings-builder).
func anthropicBody(tg target, maxTokens int, system, user string, jsonMode bool) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}
	// Thinking off: no thinking parameter, also not for DeepSeek's
	// Anthropic-compatible endpoint: its documentation gives no reliable way
	// to switch thinking off in this format (appendix D2; NOTES.md, v2
	// llm-builder). Thinking on: thinking.go (maxTokens already includes
	// the budget).
	if jsonMode {
		user += JSONInstruction
	}
	req := anthropicRequest{
		Model:     tg.model,
		MaxTokens: maxTokens,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	}
	req.Thinking, req.OutputConfig = anthropicThinkingFields(tg.model, tg.thinking)
	if system != "" { // the API rejects empty text blocks
		req.System = []anthropicTextBlock{{Type: "text", Text: system, CacheControl: &anthropicCacheControl{Type: "ephemeral"}}}
	}
	return json.Marshal(req)
}

type anthropicResponse struct {
	Type    string `json:"type"`
	Content *[]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseAnthropic reads a 200 response of /v1/messages. The text is the
// concatenation of the text blocks (thinking blocks are skipped).
//
// Usage is mapped to the DeepSeek semantics used everywhere else (job.stats,
// usage_daily, cost estimates: prompt tokens INCLUDE cache hits):
// prompt = input_tokens + cache_read_input_tokens + cache_creation_input_tokens,
// cache_hit = cache_read_input_tokens, completion = output_tokens.
//
// stop_reason "max_tokens" is a failTruncated error that carries the
// response's usage (the tokens were spent and billed).
func parseAnthropic(body []byte, key string) (Result, *attemptErr) {
	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(body, key), err: err}
	}
	if ar.Content == nil {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(body, key), err: errors.New("no content in response")}
	}
	var sb strings.Builder
	for _, b := range *ar.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	res := Result{Text: sb.String()}
	if u := ar.Usage; u != nil {
		res.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		res.CacheHitTokens = u.CacheReadInputTokens
		res.CompletionTokens = u.OutputTokens
	}
	if ar.StopReason == stopMaxTokens {
		return Result{}, &attemptErr{kind: failTruncated, res: res, err: errors.New("stop_reason max_tokens")}
	}
	return res, nil
}
