// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Transport defaults of SPEC §12.
var (
	// DefaultBackoff are the waits before retry 1..4 (429, 5xx, network
	// errors, timeouts). Retry-After overrides a wait when present.
	DefaultBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	// DefaultTimeout is the timeout of one HTTP attempt.
	DefaultTimeout = 120 * time.Second
	// MaxRetryAfter caps a server-provided Retry-After.
	MaxRetryAfter = 5 * time.Minute
)

// Temperature returns the sampling temperature of a role (SPEC §12).
func Temperature(role string) float64 { return model.DefaultTemperature(role) }

// RoleProvider returns the provider name and model configured for role
// in the legacy config (syllabus_format falls back to the glossary role).
func RoleProvider(cfg *config.Config, role string) (provider, model string) {
	r, ok := cfg.LLM.Roles[role]
	if !ok && role == RoleSyllabusFormat {
		r = cfg.LLM.Roles[config.RoleGlossary]
	}
	return r.Provider, r.Model
}

// RoleSyllabusFormat is the v1.1 A8.3 role (alias of the model constant).
const RoleSyllabusFormat = model.RoleSyllabusFormat

// HTTPClient implements Client for OpenAI-compatible providers (SPEC §12)
// and Anthropic providers (v1.1 A6). Fields other than those set by the
// constructors may be changed before first use.
//
// Two modes:
//   - New (legacy): providers and roles come from config.toml ([llm]);
//     provider ids are the config keys. Every provider is OpenAI-compatible.
//   - NewWithSettings (v1.1): providers and roles come from the settings
//     service; provider.type selects the adapter.
//
// In both modes a target attached with WithTarget wins over the role's
// current setting (target.go); in legacy mode its ProviderID is looked up
// among the config keys.
type HTTPClient struct {
	cfg    *config.Config    // legacy mode; in settings mode the startup config (log level, data dir)
	set    *settings.Service // settings mode; nil in legacy mode
	pool   sched.APIPool
	usage  UsageRecorder
	http   *http.Client
	logDir string

	// Backoff are the waits before each retry; len(Backoff) is the maximum
	// number of retries (default DefaultBackoff: 4 retries, 5 attempts).
	Backoff []time.Duration
	// Timeout of one HTTP attempt (default DefaultTimeout).
	Timeout time.Duration
	// Sleep waits d or until ctx is done (tests inject a fast one).
	Sleep func(ctx context.Context, d time.Duration) error
	// AnthropicMaxTokens, when > 0, is max_tokens of every Anthropic
	// request; 0 (the default) uses the per-role values of
	// AnthropicMaxTokensFor (SPEC-v2 appendix D2).
	AnthropicMaxTokens int

	logMu sync.Mutex
	// thinkingWarned: the thinking levels already logged as sent off
	// (thinking.go, thinkingFor).
	thinkingWarned sync.Map
}

var _ Client = (*HTTPClient)(nil)

// New returns a legacy-mode client for cfg. pool is acquired around every
// HTTP attempt (nil = no limit, tests only); when pool also implements
// UsageRecorder (sched.Pool does) every response is recorded there too.
// logDir is where debug logs go ("" = {data_dir}/logs/llm). hc nil = a
// default client with a connection pool sized for parallel requests.
func New(cfg *config.Config, pool sched.APIPool, logDir string, hc *http.Client) *HTTPClient {
	c := baseClient(cfg, pool, logDir, hc)
	if ur, ok := pool.(UsageRecorder); ok {
		c.usage = ur
	}
	return c
}

// NewWithSettings returns a v1.1 client: providers, keys and role settings
// come from set (read from its cache on every request, so edits on the
// settings page apply to the next request). pool is acquired per provider
// id around every HTTP attempt (nil = no limit, tests only); usage receives
// every successful response for usage_daily (nil = not recorded). logDir ""
// = {data_dir}/logs/llm of the startup config.
func NewWithSettings(set *settings.Service, pool sched.APIPool, usage UsageRecorder, logDir string, hc *http.Client) *HTTPClient {
	c := baseClient(set.Config(), pool, logDir, hc)
	c.set = set
	c.usage = usage
	return c
}

func baseClient(cfg *config.Config, pool sched.APIPool, logDir string, hc *http.Client) *HTTPClient {
	if cfg == nil {
		cfg = config.Default()
	}
	if logDir == "" {
		logDir = filepath.Join(cfg.DataDir, "logs", "llm")
	}
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// Room for the connections of a provider's whole concurrency
		// (perf-llm: DeepSeek 64 by default, up to settings.MaxConcurrencyLimit),
		// so a burst does not close and reopen TLS connections.
		tr.MaxIdleConns = 0
		tr.MaxIdleConnsPerHost = 256
		hc = &http.Client{Transport: tr, CheckRedirect: checkRedirect}
	} else if hc.CheckRedirect == nil {
		cp := *hc // the caller's client is left as it is
		cp.CheckRedirect = checkRedirect
		hc = &cp
	}
	return &HTTPClient{
		cfg:     cfg,
		pool:    pool,
		http:    hc,
		logDir:  logDir,
		Backoff: DefaultBackoff,
		Timeout: DefaultTimeout,
		Sleep:   sleepCtx,
	}
}

// checkRedirect is the redirect policy of the LLM clients (sec2-app): a
// redirect is followed only to the same host (scheme://host:port of the
// previous request; http → https on the same host name is allowed), so the
// API key never goes to another host — net/http keeps custom headers such
// as x-api-key on every redirect and Authorization on redirects within the
// same domain. Any other redirect is not followed: the 3xx response comes
// back as the answer (a failed request that names the status).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	if !sameOrigin(via[len(via)-1].URL, req.URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

// sameOrigin reports whether a redirect from prev to next stays on the same
// origin; an upgrade from http to https on the same host name counts.
func sameOrigin(prev, next *url.URL) bool {
	if !strings.EqualFold(prev.Hostname(), next.Hostname()) {
		return false
	}
	ps, ns := strings.ToLower(prev.Scheme), strings.ToLower(next.Scheme)
	switch {
	case ps == ns:
		return portOf(prev) == portOf(next)
	case ps == "http" && ns == "https":
		return true
	}
	return false
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// ErrTruncated: an answer was cut off at its output limit (Anthropic
// stop_reason "max_tokens", OpenAI-compatible finish_reason "length") and
// could not be completed: still truncated after the one retry with
// max_tokens doubled, or truncated at a limit the request did not set (so
// it cannot be raised). Complete returns it wrapped with sched.Permanent and
// a Chinese message ("输出被截断"); its usage is recorded as usual.
var ErrTruncated = errors.New("output truncated at max_tokens")

// TruncationRetries is how often a truncated answer is retried, each time
// with max_tokens doubled (appendix D2: once).
const TruncationRetries = 1

// DeepSeekJanitorMaxTokens is max_tokens of glossary_janitor requests to
// DeepSeek. Without it DeepSeek stops at its default of 8K output tokens in
// non-thinking mode; the documented upper limit is 384K (NOTES.md, v2
// llm-builder). Other roles and other OpenAI-compatible providers send no
// max_tokens.
const DeepSeekJanitorMaxTokens = 16000

// openAIMaxTokens returns max_tokens of an OpenAI-compatible request (0 =
// the field is not sent).
func openAIMaxTokens(role string, deepseek bool) int {
	if deepseek && role == model.RoleGlossaryJanitor {
		return DeepSeekJanitorMaxTokens
	}
	return 0
}

// thinkingParam is DeepSeek's thinking switch of the OpenAI-compatible
// API: {"thinking": {"type": "enabled" | "disabled"}} (thinking is on by
// default). Appendix D2: a role runs with thinking off unless the user
// turns its "深度思考" on (thinking.go); settings.IsDeepSeek decides which
// providers are DeepSeek.
type thinkingParam struct {
	Type string `json:"type"`
}

// thinkingDisabled returns the thinking field of a request to the provider:
// disabled for DeepSeek, absent (nil) for every other provider.
func thinkingDisabled(deepseek bool) *thinkingParam {
	if !deepseek {
		return nil
	}
	return &thinkingParam{Type: "disabled"}
}

// deepSeekThinking returns the thinking switch and reasoning_effort of an
// OpenAI-compatible request with thinking level (thinking.go): enabled with
// the level when thinking is on (DeepSeek only), else thinkingDisabled.
func deepSeekThinking(deepseek bool, level string) (*thinkingParam, string) {
	if deepseek && ThinkingBudget(level) > 0 {
		return &thinkingParam{Type: "enabled"}, level
	}
	return thinkingDisabled(deepseek), ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature is always sent, except with DeepSeek thinking on (ignored
	// in thinking mode); the field order is unchanged, so a request without
	// thinking is byte for byte what it was.
	Temperature    *float64          `json:"temperature,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Stream         bool              `json:"stream"`
	Thinking       *thinkingParam    `json:"thinking,omitempty"`   // DeepSeek only (appendix D2, thinking.go)
	MaxTokens      int               `json:"max_tokens,omitempty"` // 0 = not sent (openAIMaxTokens)
	// ReasoningEffort is DeepSeek's thinking level (thinking.go); "" = not sent.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int64  `json:"prompt_tokens"`
		CompletionTokens     int64  `json:"completion_tokens"`
		PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// failure kinds of one attempt
type failKind int

const (
	failNetwork   failKind = iota // transport error (retry)
	failTimeout                   // attempt timeout (retry)
	failStatus                    // HTTP status (retry for 429 / 5xx, incl. Anthropic 529)
	failEnvelope                  // 200 with an unreadable body (retry)
	failPool                      // sched.APIPool refused (returned as is)
	failTruncated                 // stop_reason max_tokens / finish_reason length (see Complete)
)

type attemptErr struct {
	kind       failKind
	status     int
	body       string
	retryAfter time.Duration // > 0 when the server sent Retry-After
	hasRA      bool
	err        error
	res        Result // failTruncated: the usage of the truncated answer
}

// throttled reports a throttling answer (perf-llm): 429 Too Many Requests,
// 503 Service Unavailable (DeepSeek's "server overloaded") and Anthropic's
// 529 Overloaded. The API pool lowers the provider's concurrency on it.
func (e *attemptErr) throttled() bool {
	return e.kind == failStatus && (e.status == http.StatusTooManyRequests || e.status == http.StatusServiceUnavailable || e.status == StatusOverloaded)
}

func (e *attemptErr) retryable() bool {
	if e.kind == failStatus {
		return e.status == http.StatusTooManyRequests || e.status >= 500
	}
	return true
}

// target is a resolved request target: the provider (with its key) and
// the model, temperature and thinking level to use.
type target struct {
	providerID   string // API pool and usage_daily key
	providerName string // for messages
	ptype        string // model.ProviderOpenAI | model.ProviderAnthropic
	baseURL      string
	key          string
	model        string
	temperature  float64
	deepseek     bool   // DeepSeek's API: thinking is switched off unless the role turns it on (appendix D2)
	thinking     string // model.Thinking*: the level sent (off when the provider does not support it)
}

// resolve picks the target of a request (target.go: WithTarget first,
// then the role's current setting).
func (c *HTTPClient) resolve(ctx context.Context, role string) (target, error) {
	if c.set != nil {
		return c.resolveSettings(ctx, role)
	}
	return c.resolveLegacy(ctx, role)
}

func temperatureOf(role string, p model.RoleParams) float64 {
	if p.Temperature != nil {
		return *p.Temperature
	}
	return model.DefaultTemperature(role)
}

func deletedProvider(t model.LLMSnapshot) error {
	name := t.ProviderName
	if name == "" {
		name = t.ProviderID
	}
	return sched.Permanent(model.Errorf(CodeSnapshotProviderDeleted, map[string]any{"provider": name},
		fmt.Errorf("提供商「%s」已删除，请重跑并选择使用当前设置", name)))
}

func (c *HTTPClient) resolveSettings(ctx context.Context, role string) (target, error) {
	t, ok := TargetFrom(ctx)
	if !ok {
		var err error
		if t, err = c.set.RoleTarget(role); err != nil {
			return target{}, sched.Permanent(err)
		}
	}
	p, ok := c.set.Provider(t.ProviderID)
	if !ok {
		return target{}, deletedProvider(t)
	}
	if strings.TrimSpace(t.Model) == "" {
		return target{}, sched.Permanent(model.Errorf(CodeNoModel, map[string]any{"role": role},
			fmt.Errorf("LLM 角色 %s 没有指定模型，请在设置中选择模型", role)))
	}
	if p.APIKey == "" && !settings.IsLocal(p.BaseURL) {
		return target{}, sched.Permanent(model.Errorf(CodeNoAPIKey, map[string]any{"provider": p.Name},
			fmt.Errorf("提供商「%s」没有设置 API key，请在设置页填写后点击重试", p.Name)))
	}
	deepseek := settings.IsDeepSeek(p.BaseURL, p.BalanceAPI)
	return target{
		providerID: p.ID, providerName: p.Name, ptype: p.Type, baseURL: p.BaseURL, key: p.APIKey,
		model: t.Model, temperature: temperatureOf(role, t.Params),
		deepseek: deepseek, thinking: c.thinkingFor(role, p.Name, p.Type, deepseek, t.Params),
	}, nil
}

func (c *HTTPClient) resolveLegacy(ctx context.Context, role string) (target, error) {
	name, mod := "", ""
	temp := Temperature(role)
	var params model.RoleParams // config.toml roles have no thinking
	if t, ok := TargetFrom(ctx); ok {
		if _, found := c.cfg.LLM.Providers[t.ProviderID]; !found {
			return target{}, deletedProvider(t)
		}
		name, mod, temp, params = t.ProviderID, t.Model, temperatureOf(role, t.Params), t.Params
	} else {
		r, ok := c.cfg.LLM.Roles[role]
		if !ok && role == RoleSyllabusFormat {
			r, ok = c.cfg.LLM.Roles[config.RoleGlossary]
		}
		if !ok {
			return target{}, sched.Permanent(model.Errorf(CodeUnknownRole, map[string]any{"role": role}, fmt.Errorf("未知的 LLM 角色 %q", role)))
		}
		name, mod = r.Provider, r.Model
	}
	p, ok := c.cfg.LLM.Providers[name]
	if !ok {
		return target{}, sched.Permanent(model.Errorf(CodeProviderNotConfigured, map[string]any{"role": role, "provider": name},
			fmt.Errorf("LLM 角色 %s 使用了未配置的 provider %q，请检查 API key 或模型名", role, name)))
	}
	key := p.Key()
	if key == "" && !p.Local {
		where := "config.toml 的 api_key"
		if p.APIKeyEnv != "" {
			where = "环境变量 " + p.APIKeyEnv
		}
		return target{}, sched.Permanent(model.Errorf(CodeNoAPIKey, map[string]any{"provider": name},
			fmt.Errorf("LLM provider %s 没有 API key（%s 为空），请检查 API key 或模型名", name, where)))
	}
	deepseek := settings.IsDeepSeek(p.BaseURL, "")
	return target{
		providerID: name, providerName: name, ptype: model.ProviderOpenAI, baseURL: p.BaseURL, key: key,
		model: mod, temperature: temp, deepseek: deepseek,
		thinking: c.thinkingFor(role, name, model.ProviderOpenAI, deepseek, params),
	}, nil
}

// Complete implements Client. See the Client doc for the error contract.
func (c *HTTPClient) Complete(ctx context.Context, role, system, user string, jsonMode bool) (Result, error) {
	tg, err := c.resolve(ctx, role)
	if err != nil {
		return Result{}, err
	}
	// maxTokens is the max_tokens of the request; 0 = not sent (most
	// OpenAI-compatible requests: the server's default limit applies).
	maxTokens := openAIMaxTokens(role, tg.deepseek)
	if tg.ptype == model.ProviderAnthropic {
		maxTokens = c.AnthropicMaxTokens
		if maxTokens <= 0 {
			maxTokens = AnthropicMaxTokensFor(role)
		}
	}
	// Thinking (thinking.go): the limit grows by the level's budget; a
	// DeepSeek role that sends no limit starts from DeepSeek's default.
	budget := ThinkingBudget(tg.thinking)
	if budget > 0 {
		if maxTokens <= 0 {
			maxTokens = DeepSeekDefaultMaxTokens
		}
		maxTokens += budget
	}
	build := func() ([]byte, error) {
		if tg.ptype == model.ProviderAnthropic {
			return anthropicBody(tg, maxTokens, system, user, jsonMode)
		}
		thinking, effort := deepSeekThinking(tg.deepseek, tg.thinking)
		req := chatRequest{
			Model: tg.model,
			Messages: []chatMessage{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
			Thinking:        thinking,
			MaxTokens:       maxTokens,
			ReasoningEffort: effort,
		}
		if effort == "" { // DeepSeek ignores temperature in thinking mode
			temp := tg.temperature
			req.Temperature = &temp
		}
		if jsonMode {
			req.ResponseFormat = map[string]string{"type": "json_object"}
		}
		return json.Marshal(req)
	}
	endpoint := strings.TrimRight(tg.baseURL, "/") + "/chat/completions"
	if tg.ptype == model.ProviderAnthropic {
		endpoint = strings.TrimRight(tg.baseURL, "/") + "/v1/messages"
	}
	body, err := build()
	if err != nil {
		return Result{}, sched.Permanent(err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if budget > 0 {
		timeout = thinkingTimeout(timeout, budget)
	}
	call := callInfo{role: role, provider: tg.providerID, providerName: tg.providerName, api: tg.ptype,
		model: tg.model, url: endpoint, key: tg.key, timeout: timeout}

	// retries counts the §12 transport retries (they use Backoff);
	// truncations the appendix D2 retries of a truncated answer.
	retries, truncations := 0, 0
	fb, _ := c.pool.(RateFeedback)
	for attempt := 0; ; attempt++ {
		res, start, aerr := c.attempt(ctx, call, body, attempt)
		if fb != nil && !start.IsZero() {
			switch {
			case aerr == nil || aerr.kind == failTruncated:
				fb.Succeeded(call.provider)
			case aerr.throttled():
				fb.Throttled(call.provider, start)
			}
		}
		if aerr == nil {
			c.reportUsage(ctx, tg, res, start)
			return res, nil
		}
		if aerr.kind == failTruncated {
			// The truncated answer was billed: it counts like any other.
			c.reportUsage(ctx, tg, aerr.res, start)
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if aerr.kind == failPool {
			return Result{}, aerr.err
		}
		if aerr.kind == failTruncated {
			// Appendix D2: a failure, retried with max_tokens doubled. The
			// server is fine, so there is no backoff wait. Retrying the job
			// with the same limits would most likely be cut off again (and
			// billed again), so the final error is permanent. A request
			// without max_tokens hit the server's default limit, which this
			// client cannot raise: permanent at once, no transport retry.
			if maxTokens <= 0 || truncations >= TruncationRetries {
				return Result{}, sched.Permanent(truncatedError(call, maxTokens, truncations, attempt))
			}
			truncations++
			maxTokens *= 2
			if body, err = build(); err != nil {
				return Result{}, sched.Permanent(err)
			}
			continue
		}
		if !aerr.retryable() {
			return Result{}, sched.Permanent(finalError(call, aerr, attempt))
		}
		if retries >= len(c.Backoff) {
			return Result{}, finalError(call, aerr, attempt)
		}
		wait := c.Backoff[retries]
		if aerr.hasRA {
			wait = aerr.retryAfter
		}
		retries++
		if err := c.Sleep(ctx, wait); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, err
		}
	}
}

// reportUsage reports the usage of one response, sent at start, to
// usage_daily (priced by the period start falls in, v2 P12.5, when the
// recorder is a CostRecorder), the job's stats sink (with that cost) and the
// WithUsage totals.
func (c *HTTPClient) reportUsage(ctx context.Context, tg target, res Result, start time.Time) {
	cost, priced := 0.0, false
	if c.usage != nil {
		// Spent tokens count even when the job is cancelled right now.
		uctx := context.WithoutCancel(ctx)
		if cr, ok := c.usage.(CostRecorder); ok {
			cost, priced = cr.RecordRequest(uctx, tg.providerID, tg.model, start, res.PromptTokens, res.CacheHitTokens, res.CompletionTokens)
		} else {
			c.usage.RecordUsage(uctx, tg.providerID, res.PromptTokens, res.CacheHitTokens, res.CompletionTokens)
		}
	}
	// The scheduler's stats sink owns job.stats (NOTES.md, Gate A round 2).
	// Without a priced estimate it prices the tokens itself (sched.ReportUsageCost).
	sched.ReportUsageCost(ctx, res.PromptTokens, res.CompletionTokens, res.CacheHitTokens, cost, priced)
	if u := UsageFrom(ctx); u != nil {
		u.Add(res)
	}
}

// truncatedError is the job error of an answer still truncated after the
// D2 retries; maxTokens is the limit of the last attempt (0 = the request
// set none and the server's default output limit was reached). Coded
// CodeTruncated (v2 P14.3); errors.Is(err, ErrTruncated) holds.
func truncatedError(call callInfo, maxTokens, truncations, retries int) error {
	limits := fmt.Sprintf("max_tokens %d 仍不够", maxTokens)
	switch {
	case maxTokens <= 0:
		limits = "回答达到了服务端默认的输出长度上限"
	case truncations > 0:
		limits = fmt.Sprintf("max_tokens 已从 %d 翻倍到 %d 仍不够", maxTokens>>truncations, maxTokens)
	}
	tried := ""
	if retries > 0 {
		tried = fmt.Sprintf("，已重试 %d 次", retries)
	}
	params := map[string]any{"role": call.role, "retries": retries, "provider": call.providerName, "model": call.model,
		"max_tokens": max(maxTokens, 0)}
	return model.Errorf(CodeTruncated, params, fmt.Errorf("LLM 输出被截断（%s，%s%s；provider %s，model %s），可以点击重试，或在设置中为该角色换用其他模型：%w",
		call.role, limits, tried, call.providerName, call.model, ErrTruncated))
}

type callInfo struct {
	role, provider, providerName, api, model, url, key string
	timeout                                            time.Duration
}

// setAuth sets the authentication headers of the provider type.
func setAuth(h http.Header, api, key string) {
	if api == model.ProviderAnthropic {
		if key != "" {
			h.Set("x-api-key", key)
		}
		h.Set("anthropic-version", AnthropicVersion)
		return
	}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

// attempt performs one HTTP request holding an API pool slot. start is when
// the request was sent (after the pool granted it): the moment that decides
// its peak or off-peak price (v2 P12.5).
func (c *HTTPClient) attempt(ctx context.Context, call callInfo, body []byte, n int) (_ Result, start time.Time, _ *attemptErr) {
	if c.pool != nil {
		release, err := c.pool.Acquire(ctx, call.provider)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, start, &attemptErr{kind: failNetwork, err: ctx.Err()}
			}
			return Result{}, start, &attemptErr{kind: failPool, err: fmt.Errorf("api pool: %w", err)}
		}
		defer release()
	}
	if fn := sentHookFrom(ctx); fn != nil {
		fn()
	}
	actx, cancel := context.WithTimeout(ctx, call.timeout)
	defer cancel()

	start = time.Now()
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, call.url, bytes.NewReader(body))
	if err != nil {
		// Malformed base_url: not retryable (status 0 → permanent, generic message).
		return Result{}, start, &attemptErr{kind: failStatus, status: 0, body: err.Error(), err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	setAuth(hreq.Header, call.api, call.key)

	resp, err := c.http.Do(hreq)
	var respBody []byte
	status := 0
	if err == nil {
		status = resp.StatusCode
		respBody, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
	}
	c.debugLog(ctx, call, hreq.Header, body, n, status, respBody, err, time.Since(start))

	if err != nil {
		if ctx.Err() != nil {
			return Result{}, start, &attemptErr{kind: failNetwork, err: ctx.Err()}
		}
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return Result{}, start, &attemptErr{kind: failTimeout, err: err}
		}
		return Result{}, start, &attemptErr{kind: failNetwork, err: err}
	}
	if status != http.StatusOK {
		ae := &attemptErr{kind: failStatus, status: status, body: redactSnippet(respBody, call.key)}
		ae.retryAfter, ae.hasRA = parseRetryAfter(resp.Header.Get("Retry-After"))
		return Result{}, start, ae
	}
	if call.api == model.ProviderAnthropic {
		res, aerr := parseAnthropic(respBody, call.key)
		return res, start, aerr
	}
	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return Result{}, start, &attemptErr{kind: failEnvelope, body: redactSnippet(respBody, call.key), err: err}
	}
	if len(cr.Choices) == 0 {
		return Result{}, start, &attemptErr{kind: failEnvelope, body: redactSnippet(respBody, call.key), err: errors.New("no choices in response")}
	}
	res := Result{Text: cr.Choices[0].Message.Content}
	if u := cr.Usage; u != nil {
		res.PromptTokens = u.PromptTokens
		res.CompletionTokens = u.CompletionTokens
		switch {
		case u.PromptCacheHitTokens != nil:
			res.CacheHitTokens = *u.PromptCacheHitTokens
		case u.PromptTokensDetails != nil:
			res.CacheHitTokens = u.PromptTokensDetails.CachedTokens
		}
	}
	if cr.Choices[0].FinishReason == finishLength {
		// Cut off at max_tokens (or the context length): the same failure
		// as Anthropic's stop_reason max_tokens (appendix D2).
		return Result{}, start, &attemptErr{kind: failTruncated, res: res, err: errors.New("finish_reason length")}
	}
	return res, start, nil
}

// finishLength is the OpenAI-compatible finish_reason of an answer cut off
// at max_tokens or at the context length.
const finishLength = "length"

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parseRetryAfter reads delay-seconds or an HTTP date, capped at MaxRetryAfter.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			secs = 0
		}
		d = time.Duration(secs * float64(time.Second))
	} else if t, err := http.ParseTime(v); err == nil {
		d = max(time.Until(t), 0)
	} else {
		return 0, false
	}
	return min(d, MaxRetryAfter), true
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	const n = 300
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// redactSnippet is the excerpt of a server's body for an error or a log:
// the key is redacted BEFORE the body is cut to its excerpt, so a key that
// straddles the cut cannot leak in part (sec2-app L1).
func redactSnippet(b []byte, key string) string {
	return snippet([]byte(redact(string(b), key)))
}

// redact replaces a key echoed by a server (some error bodies quote the
// key they rejected) with its hint, so it never reaches job.error or the UI.
func redact(s, key string) string {
	if key == "" || !strings.Contains(s, key) {
		return s
	}
	return strings.ReplaceAll(s, key, model.KeyHint(key))
}

// finalError turns the last attempt failure into the user-facing job error
// (SPEC §12, §17.6). retries is the number of retries already made. The
// error is coded (v2 P14.3, codes.go); its Chinese text is the detail.
func finalError(call callInfo, e *attemptErr, retries int) error {
	tried := ""
	if retries > 0 {
		tried = fmt.Sprintf("，已重试 %d 次", retries)
	}
	params := func(kv ...any) map[string]any {
		p := map[string]any{"role": call.role, "retries": retries}
		for i := 0; i+1 < len(kv); i += 2 {
			p[kv[i].(string)] = kv[i+1]
		}
		return p
	}
	switch e.kind {
	case failNetwork:
		return model.Errorf(CodeNetwork, params(),
			fmt.Errorf("LLM 请求失败（%s%s）：网络不可用，请检查网络连接后点击重试：%w", call.role, tried, e.err))
	case failTimeout:
		return model.Errorf(CodeTimeout, params("seconds", call.timeout.Seconds()),
			fmt.Errorf("LLM 请求超时（%s，单次请求超过 %s%s）：网络不可用或服务无响应，请稍后点击重试：%w",
				call.role, call.timeout, tried, e.err))
	case failEnvelope:
		return model.Errorf(CodeBadResponse, params(),
			fmt.Errorf("LLM 响应无法解析（%s%s）：%v：%s", call.role, tried, e.err, e.body))
	}
	switch {
	case e.status == http.StatusBadRequest || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden:
		return model.Errorf(CodeRejected, params("status", e.status, "provider", call.providerName, "model", call.model),
			fmt.Errorf("LLM 请求被拒绝（%s，HTTP %d），请检查 API key 或模型名（provider %s，model %s）：%s",
				call.role, e.status, call.providerName, call.model, e.body))
	case e.status == http.StatusPaymentRequired:
		return model.Errorf(CodeNoBalance, params("provider", call.providerName),
			fmt.Errorf("LLM 账户余额不足（%s，HTTP 402，provider %s），请充值后点击重试：%s", call.role, call.providerName, e.body))
	case e.status == http.StatusTooManyRequests:
		return model.Errorf(CodeRateLimited, params(),
			fmt.Errorf("LLM 请求被限流（%s，HTTP 429%s），请稍后点击重试：%s", call.role, tried, e.body))
	case e.status == StatusOverloaded:
		return model.Errorf(CodeOverloaded, params(),
			fmt.Errorf("LLM 服务过载（%s，HTTP 529%s），请稍后点击重试：%s", call.role, tried, e.body))
	case e.status >= 500:
		return model.Errorf(CodeServerError, params("status", e.status),
			fmt.Errorf("LLM 服务端错误（%s，HTTP %d%s），请稍后点击重试：%s", call.role, e.status, tried, e.body))
	}
	return model.Errorf(CodeRequestFailed, params("status", e.status, "provider", call.providerName, "model", call.model),
		fmt.Errorf("LLM 请求失败（%s，HTTP %d），请检查 base_url、API key 或模型名（provider %s，model %s）：%s",
			call.role, e.status, call.providerName, call.model, e.body))
}
