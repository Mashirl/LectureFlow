// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Defaults of a new provider (A5.1 DDL defaults).
const (
	DefaultMaxConcurrency = 4
	DefaultRPM            = 60
)

// Ranges of a provider's limits (perf-llm, NOTES.md). max_concurrency goes
// up to 512: DeepSeek's limit is per account (2500 concurrent requests for
// deepseek-flash) and a lecture of ~290 translate requests gains nothing
// beyond that. rpm 0 means no per-minute limit (DeepSeek has none).
const (
	MaxConcurrencyLimit = 512
	MaxRPM              = 10000
)

// DefaultASR is the ASR settings default of Load (SPEC §2, §17.2; SPEC-v2
// I8 and lead ruling 8 for the four fields of phase 3b: device_mode auto,
// multi_device true, battery_aware true, mem_reserve_mb 2048).
func DefaultASR() model.ASRSettings {
	return model.ASRSettings{Model: "large-v3", ComputeType: "auto", Device: "cuda", Language: "en",
		MaxParallel: 1, VRAMReserveMB: 1000, VRAMPerJobMB: 5000,
		DeviceMode: model.ASRDeviceModeAuto, MultiDevice: true, BatteryAware: true, MemReserveMB: DefaultMemReserveMB}
}

// DefaultMemReserveMB is the default of mem_reserve_mb (lead ruling 8) and
// MaxMemReserveMB its upper bound (CodeMemReserveInvalid: 0..65536).
const (
	DefaultMemReserveMB = 2048
	MaxMemReserveMB     = 65536
)

// DefaultGeneral is the general settings default of Load.
func DefaultGeneral() model.GeneralSettings {
	return model.GeneralSettings{KeepOriginalMedia: true, Notifications: true}
}

// fillASR replaces missing or out-of-range stored values with the defaults.
func fillASR(a model.ASRSettings) model.ASRSettings {
	d := DefaultASR()
	if strings.TrimSpace(a.Model) == "" {
		a.Model = d.Model
	}
	switch a.ComputeType {
	case "auto", "float16", "int8_float16":
	default:
		a.ComputeType = d.ComputeType
	}
	if a.Device != "cuda" && a.Device != "cpu" {
		a.Device = d.Device
	}
	if strings.TrimSpace(a.Language) == "" {
		a.Language = d.Language
	}
	if a.MaxParallel < 1 {
		a.MaxParallel = d.MaxParallel
	}
	if a.VRAMReserveMB < 0 {
		a.VRAMReserveMB = d.VRAMReserveMB
	}
	if a.VRAMPerJobMB <= 0 {
		a.VRAMPerJobMB = d.VRAMPerJobMB
	}
	// Phase 3b: a missing key keeps its default already (Load decodes over
	// DefaultASR); an empty device_mode is auto, an out-of-range reserve the
	// default. A device that no longer exists is kept (power management:
	// as a member of the manual list, below); the settings page shows the
	// choice.
	if a.DeviceMode = strings.TrimSpace(a.DeviceMode); a.DeviceMode == "" {
		a.DeviceMode = d.DeviceMode
	}
	if a.MemReserveMB < 0 || a.MemReserveMB > MaxMemReserveMB {
		a.MemReserveMB = d.MemReserveMB
	}
	// Power management (power-brief §3.2): device_mode is auto or manual
	// with a non-empty list without CPU devices; a 3b value naming one
	// device reads as manual [that id] (a CPU device: auto). Reading only
	// normalizes; a manual id whose row is gone is kept (the policy skips
	// devices that are not usable).
	return model.NormalizeASRDeviceMode(a)
}

// Config returns the startup configuration the service was opened with
// (never nil; config.Default() when none was given). Only the startup
// fields (data_dir, log_level, ...) are meaningful; the legacy [asr]/[llm]
// sections were imported once and are not read any more.
func (s *Service) Config() *config.Config { return s.config() }

func (s *Service) config() *config.Config { return s.cfg }

// TestModel picks the model for "测试连接" of a provider (A5.3): the model
// of the first role (settings-page order) that uses the provider, else the
// first cached probed model, else "".
func (s *Service) TestModel(ctx context.Context, providerID string) string {
	s.mu.RLock()
	for _, role := range model.Roles {
		r, ok := s.roles[role]
		if ok && r.ProviderID != nil && *r.ProviderID == providerID && r.Model != nil && strings.TrimSpace(*r.Model) != "" {
			m := strings.TrimSpace(*r.Model)
			s.mu.RUnlock()
			return m
		}
	}
	s.mu.RUnlock()
	if pm, ok, err := s.ProbedModels(ctx, providerID); err == nil && ok && len(pm.Models) > 0 {
		return pm.Models[0]
	}
	return ""
}

// changed calls the OnChange hooks (outside every lock).
func (s *Service) changed() {
	s.mu.RLock()
	hooks := append([]func(){}, s.hooks...)
	s.mu.RUnlock()
	for _, fn := range hooks {
		fn()
	}
}

func (s *Service) providerLocked(id string) (model.Provider, bool) {
	for _, p := range s.providers {
		if p.ID == id {
			return p, true
		}
	}
	return model.Provider{}, false
}

func cloneProvider(p model.Provider) model.Provider {
	cp := func(f *float64) *float64 {
		if f == nil {
			return nil
		}
		v := *f
		return &v
	}
	p.PriceInput, p.PriceCacheHit, p.PriceOutput = cp(p.PriceInput), cp(p.PriceCacheHit), cp(p.PriceOutput)
	if p.Currency != nil {
		c := *p.Currency
		p.Currency = &c
	}
	if p.Tariff != nil {
		t := *p.Tariff
		p.Tariff = &t
	}
	return p
}

func cloneRole(r model.RoleSetting) model.RoleSetting {
	if r.ProviderID != nil {
		v := *r.ProviderID
		r.ProviderID = &v
	}
	if r.Model != nil {
		v := *r.Model
		r.Model = &v
	}
	return r
}

// userError is an error meant for the user: it matches kind with
// errors.Is, its text is the Chinese message (kept as the detail), and
// errors.As finds its coded form *model.CodedError (v2 P14.3: code +
// params, the message as the wrapped error) for the API and job.error.
type userError struct {
	kind  error
	msg   string
	coded *model.CodedError
}

func (e *userError) Error() string { return e.msg }
func (e *userError) Unwrap() error { return e.kind }

// As lets errors.As(err, &ce) with ce *model.CodedError find the coded form.
func (e *userError) As(target any) bool {
	if p, ok := target.(**model.CodedError); ok {
		*p = e.coded
		return true
	}
	return false
}

// userErr builds a userError with its code and params (codes.go).
func userErr(kind error, code string, params map[string]any, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	return &userError{kind: kind, msg: msg,
		coded: &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params}, Err: errors.New(msg)}}
}

// parseParams decodes role_setting.params; invalid JSON counts as {}.
func parseParams(raw string) model.RoleParams {
	var p model.RoleParams
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// ResolveParams fills the defaults of a role's params as stored in a
// snapshot (NOTES.md v1.1 ruling 6): temperature always set (default
// model.DefaultTemperature), window set for translate only (default
// model.DefaultTranslateWindow) and dropped for every other role; thinking
// set only when it is a valid level other than off (model.ThinkingLevel),
// so a role without thinking resolves to exactly what it did before the
// option existed.
func ResolveParams(role string, p model.RoleParams) model.RoleParams {
	t := model.DefaultTemperature(role)
	if p.Temperature != nil {
		t = *p.Temperature
	}
	out := model.RoleParams{Temperature: &t}
	if role == model.RoleTranslate {
		w := model.DefaultTranslateWindow
		if p.Window != nil && *p.Window >= 0 {
			w = *p.Window
		}
		out.Window = &w
	}
	if lvl := model.ThinkingLevel(p); lvl != model.ThinkingOff {
		out.Thinking = &lvl
	}
	return out
}

// SupportsThinking reports whether the role setting "深度思考" can be
// turned on for provider p (.claude/thinking-brief.md): DeepSeek's
// OpenAI-compatible API (IsDeepSeek: thinking and reasoning_effort) and
// every Anthropic-type provider (thinking in the Messages API). Other
// OpenAI-compatible providers have no common switch, so their roles stay
// off (SetRole refuses anything else; internal/llm sends a stray level
// as off).
func SupportsThinking(p model.Provider) bool {
	switch p.Type {
	case model.ProviderAnthropic:
		return true
	case model.ProviderOpenAI:
		return IsDeepSeek(p.BaseURL, p.BalanceAPI)
	}
	return false
}

func snapshotProviderName(s model.LLMSnapshot) string {
	if s.ProviderName != "" {
		return s.ProviderName
	}
	return s.ProviderID
}

// IsLocal reports whether a base_url points at this machine (localhost,
// 127.0.0.0/8, ::1): such servers (Ollama, llama.cpp) may run without a key.
func IsLocal(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isDeepSeek(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.EqualFold(u.Hostname(), "api.deepseek.com")
}

func validRole(role string) bool {
	for _, r := range model.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// rolesUsing lists the roles (settings-page order) whose row names the provider.
func rolesUsing(rows []model.RoleSetting, providerID string) []string {
	used := map[string]bool{}
	for _, r := range rows {
		if r.ProviderID != nil && *r.ProviderID == providerID {
			used[r.Role] = true
		}
	}
	var out []string
	for _, role := range model.Roles {
		if used[role] {
			out = append(out, role)
		}
	}
	return out
}

func inUseErr(roles []string) error {
	labels := make([]string, len(roles))
	for i, r := range roles {
		labels[i] = "「" + RoleLabel(r) + "」"
	}
	return userErr(ErrProviderInUse, CodeProviderInUse, map[string]any{"roles": slices.Clone(roles)},
		"提供商正被%s使用，请先在角色分配中改用其他提供商", strings.Join(labels, "、"))
}

// providerWriteErr maps a failed provider insert/update to a user message.
func providerWriteErr(err error, name string) error {
	if errors.Is(err, db.ErrDuplicate) {
		return userErr(db.ErrDuplicate, CodeProviderNameTaken, map[string]any{"name": name}, "已有名为「%s」的提供商，请换一个名称", name)
	}
	return err
}

// applyInput applies a create/patch input to p and validates the result.
// Error texts never contain the key.
func applyInput(p *model.Provider, in ProviderInput) error {
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		p.Type = strings.TrimSpace(*in.Type)
	}
	if in.BaseURL != nil {
		p.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.APIKey != nil {
		if k := strings.TrimSpace(*in.APIKey); k != "" {
			p.APIKey = k
		}
	}
	if in.MaxConcurrency != nil {
		p.MaxConcurrency = *in.MaxConcurrency
	}
	if in.RPM != nil {
		p.RPM = *in.RPM
	}
	if in.BalanceAPI != nil {
		p.BalanceAPI = strings.TrimSpace(*in.BalanceAPI)
	}
	if in.ClearPrices {
		p.PriceInput, p.PriceCacheHit, p.PriceOutput = nil, nil, nil
	}
	for _, f := range []struct {
		in  *float64
		dst **float64
	}{{in.PriceInput, &p.PriceInput}, {in.PriceCacheHit, &p.PriceCacheHit}, {in.PriceOutput, &p.PriceOutput}} {
		if f.in != nil {
			v := *f.in
			*f.dst = &v
		}
	}
	if in.Currency != nil {
		if c := strings.TrimSpace(*in.Currency); c != "" {
			p.Currency = &c
		} else {
			p.Currency = nil
		}
	}
	if p.BalanceAPI == "" {
		p.BalanceAPI = model.BalanceNone
	}
	if err := applyTariff(p, in); err != nil {
		return err
	}
	return validateProvider(*p)
}

func validateProvider(p model.Provider) error {
	if p.Name == "" {
		return userErr(ErrInvalid, CodeProviderNameEmpty, nil, "请填写提供商名称")
	}
	if len([]rune(p.Name)) > 64 {
		return userErr(ErrInvalid, CodeProviderNameTooLong, map[string]any{"max": 64}, "提供商名称不能超过 64 个字符")
	}
	if p.Type != model.ProviderOpenAI && p.Type != model.ProviderAnthropic {
		return userErr(ErrInvalid, CodeProviderTypeInvalid, nil, "提供商类型只能是 OpenAI 兼容（openai）或 Anthropic（anthropic）")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return userErr(ErrInvalid, CodeBaseURLInvalid, nil, "base_url 必须是 http:// 或 https:// 开头的完整地址")
	}
	if p.MaxConcurrency < 1 || p.MaxConcurrency > MaxConcurrencyLimit {
		return userErr(ErrInvalid, CodeConcurrencyInvalid, map[string]any{"min": 1, "max": MaxConcurrencyLimit},
			"并发上限必须在 1 到 %d 之间", MaxConcurrencyLimit)
	}
	if p.RPM < 0 || p.RPM > MaxRPM {
		return userErr(ErrInvalid, CodeRPMInvalid, map[string]any{"min": 0, "max": MaxRPM},
			"每分钟请求数必须在 0 到 %d 之间（0 表示不限）", MaxRPM)
	}
	if p.BalanceAPI != model.BalanceNone && p.BalanceAPI != model.BalanceDeepSeek {
		return userErr(ErrInvalid, CodeBalanceAPIInvalid, nil, "余额接口只能是 none 或 deepseek")
	}
	for _, f := range []*float64{p.PriceInput, p.PriceCacheHit, p.PriceOutput} {
		if f != nil && (!(*f >= 0) || math.IsInf(*f, 0)) {
			return userErr(ErrInvalid, CodePriceInvalid, nil, "价格必须是不小于 0 的数")
		}
	}
	return nil
}

// importLegacy writes the legacy config.toml fields (ImportLegacy). It
// returns the names of imported providers that have no key.
func importLegacy(ctx context.Context, tx db.Store, cfg *config.Config) (noKey []string, err error) {
	names := make([]string, 0, len(cfg.LLM.Providers))
	for name := range cfg.LLM.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	now := db.Now()
	imported := map[string]bool{}
	for _, name := range names {
		lp := cfg.LLM.Providers[name]
		p := model.Provider{
			ID: name, Name: name, Type: model.ProviderOpenAI,
			BaseURL:        strings.TrimRight(strings.TrimSpace(lp.BaseURL), "/"),
			APIKey:         strings.TrimSpace(lp.Key()),
			MaxConcurrency: clamp(lp.MaxConcurrency, 1, MaxConcurrencyLimit, DefaultMaxConcurrency),
			RPM:            legacyRPM(lp.RequestsPerMinute),
			BalanceAPI:     model.BalanceNone,
			CreatedAt:      now,
		}
		if isDeepSeek(p.BaseURL) {
			p.BalanceAPI = model.BalanceDeepSeek
		}
		if pr, ok := LegacyPrice(cfg, name); ok {
			in, hit, out := pr.Input, pr.CacheHit, pr.Output
			p.PriceInput, p.PriceCacheHit, p.PriceOutput = &in, &hit, &out
		}
		if err := tx.InsertProvider(ctx, p); err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		imported[name] = true
		if p.APIKey == "" && !lp.Local && !IsLocal(p.BaseURL) {
			noKey = append(noKey, name)
		}
	}
	upsert := func(role string, r config.Role) error {
		if r.Provider == "" || !imported[r.Provider] || strings.TrimSpace(r.Model) == "" {
			return nil // the user assigns it on the settings page
		}
		var params model.RoleParams
		if role == model.RoleTranslate && r.Window > 0 {
			w := r.Window
			params.Window = &w
		}
		b, _ := json.Marshal(params)
		pid, m := r.Provider, strings.TrimSpace(r.Model)
		return tx.UpsertRoleSetting(ctx, model.RoleSetting{Role: role, ProviderID: &pid, Model: &m, Params: string(b)})
	}
	for _, role := range []string{model.RoleGlossary, model.RoleSummary, model.RoleRepair, model.RoleTranslate} {
		if err := upsert(role, cfg.LLM.Roles[role]); err != nil {
			return nil, err
		}
	}
	g := cfg.LLM.Roles[model.RoleGlossary]
	if err := upsert(model.RoleSyllabusFormat, config.Role{Provider: g.Provider, Model: g.Model}); err != nil {
		return nil, err
	}

	// The legacy [asr] fields over the defaults: the phase 3b fields
	// (device_mode, multi_device, battery_aware, mem_reserve_mb) have no
	// config.toml key and keep DefaultASR's values (their zero values would
	// switch multi_device and battery_aware off).
	a := DefaultASR()
	a.Model, a.ComputeType, a.Device, a.Language = cfg.ASR.Model, cfg.ASR.ComputeType, cfg.ASR.Device, cfg.ASR.Language
	a.MaxParallel, a.VRAMReserveMB, a.VRAMPerJobMB = cfg.ASR.MaxParallel, cfg.ASR.VRAMReserveMB, cfg.ASR.VRAMPerJobMB
	a = fillASR(a)
	b, _ := json.Marshal(a)
	if err := tx.SetKV(ctx, model.KVSettingsASR, string(b)); err != nil {
		return nil, err
	}
	if _, ok, err := tx.GetKV(ctx, model.KVSettingsGeneral); err != nil {
		return nil, err
	} else if !ok {
		budget := cfg.LLM.DailyBudget
		if !(budget >= 0) {
			budget = 0
		}
		b, _ := json.Marshal(model.GeneralSettings{KeepOriginalMedia: cfg.Storage.KeepMedia(), Notifications: true, DailyBudget: budget})
		if err := tx.SetKV(ctx, model.KVSettingsGeneral, string(b)); err != nil {
			return nil, err
		}
	}
	return noKey, nil
}

// LegacyPrice returns the [llm.prices] entry of the model the roles use
// with provider: usage_daily has no model column, so a provider carries one
// price. Roles are tried by token volume (translate, repair, summary,
// glossary); a provider no role uses takes its only priced model, if any.
func LegacyPrice(cfg *config.Config, provider string) (config.Price, bool) {
	for _, role := range []string{model.RoleTranslate, model.RoleRepair, model.RoleSummary, model.RoleGlossary} {
		r, ok := cfg.LLM.Roles[role]
		if !ok || r.Provider != provider {
			continue
		}
		if p, ok := cfg.PriceFor(provider, r.Model); ok {
			return p, true
		}
	}
	if m := cfg.LLM.Prices[provider]; len(m) == 1 {
		for _, p := range m {
			return p, true
		}
	}
	return config.Price{}, false
}

// legacyRPM is the rpm of a provider imported from config.toml: 0 (the
// config's DeepSeek default, perf-llm) = no per-minute limit, a negative
// value the default.
func legacyRPM(v int) int {
	if v < 0 {
		return DefaultRPM
	}
	return min(v, MaxRPM)
}

// SameOrigin reports whether two base_urls point at the same origin
// (scheme, host and port; case-insensitive, default ports filled in). An
// unparsable URL only matches itself.
func SameOrigin(a, b string) bool {
	origin := func(s string) string {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || u.Host == "" {
			return "raw:" + strings.TrimSpace(s)
		}
		scheme := strings.ToLower(u.Scheme)
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[scheme]
		}
		return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
	}
	return origin(a) == origin(b)
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}
