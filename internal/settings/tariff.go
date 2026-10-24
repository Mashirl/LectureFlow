// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/tariff"
)

// v2 phase 2 (SPEC-v2 P12, appendix D3, P14.6, P14.7): provider tariffs and
// the cost of one request, the holiday data, and the UI language settings.
// Decisions: NOTES.md "v2 决定（llm-p2）".

// One-time startup steps of phase 2 (kv flags, "1" when done).
const (
	// KVTariffPreset: the DeepSeek tariff preset was filled into the
	// providers that existed before P12 (their tariff was never set).
	KVTariffPreset = "v2.tariff_preset"
	// KVUsageCostBackfill: usage_daily rows recorded before P12.5 got their
	// estimated_cost from the provider prices of that time.
	KVUsageCostBackfill = "v2.usage_cost_backfill"
)

// DeepSeekPresetJSON is json.Marshal(tariff.DeepSeekPreset()): the tariff a
// new DeepSeek provider gets (P12.2, D3).
func DeepSeekPresetJSON() string {
	b, _ := json.Marshal(tariff.DeepSeekPreset())
	return string(b)
}

// tariffError is a tariff validation error: the Chinese message is the
// Error() text (shown as is), errors.Is matches ErrInvalid and errors.As
// finds the *tariff.Error with its stable Code and Params (P14.3).
type tariffError struct {
	msg string
	te  *tariff.Error
}

func (e *tariffError) Error() string   { return e.msg }
func (e *tariffError) Unwrap() []error { return []error{ErrInvalid, e.te} }

// As lets errors.As find the coded form (*model.CodedError: the tariff
// code and params, the Chinese message as the wrapped error), like userError.
func (e *tariffError) As(target any) bool {
	if p, ok := target.(**model.CodedError); ok {
		*p = &model.CodedError{Info: model.ErrorInfo{Code: e.te.Code, Params: e.te.Params}, Err: errors.New(e.msg)}
		return true
	}
	return false
}

// tariffErr maps an error of tariff.Parse / tariff.ParseHolidays.
func tariffErr(err error) error {
	var te *tariff.Error
	if !errors.As(err, &te) {
		return userErr(ErrInvalid, CodeTariffInvalid, nil, "时段计价配置无效：%v", err)
	}
	p := func(k string) any { return te.Params[k] }
	var msg string
	switch te.Code {
	case tariff.CodeInvalidJSON:
		msg = "时段计价配置不是有效的 JSON"
	case tariff.CodeInvalidTimezone:
		msg = fmt.Sprintf("时段计价的时区「%v」无效，请填写 IANA 时区名，例如 Asia/Shanghai", p("timezone"))
	case tariff.CodeInvalidWindow:
		n := 0
		if i, ok := te.Params["index"].(int); ok {
			n = i + 1
		}
		msg = fmt.Sprintf("第 %d 个高峰时段无效：星期取 1–7（1 = 周一），时间格式为 HH:MM，开始须早于结束（跨午夜的时段请拆成两段）", n)
	case tariff.CodeInvalidHolidays:
		msg = "exclude_holidays 只能是 CN 或留空"
	case tariff.CodeInvalidMultiplier:
		msg = "空闲时段的价格倍率必须大于 0 且不超过 1"
	case tariff.CodeInvalidPrice:
		msg = fmt.Sprintf("模型「%v」的价格必须是不小于 0 的数", p("model"))
	case tariff.CodeInvalidHolidayData:
		msg = "节假日数据无效：每个年份（四位数字）对应该年的一组日期，格式为 YYYY-MM-DD"
	default:
		msg = "时段计价配置无效"
	}
	return &tariffError{msg: msg, te: te}
}

// applyTariff applies the tariff fields of a create/patch input (P12.2,
// P12.6). The stored JSON is the input, compacted (unknown fields are kept
// for newer versions).
func applyTariff(p *model.Provider, in ProviderInput) error {
	if in.ClearTariff {
		p.Tariff = nil
	}
	if in.Tariff != nil {
		raw := strings.TrimSpace(*in.Tariff)
		tf, err := tariff.Parse(raw)
		if err != nil {
			return tariffErr(err)
		}
		if tf == nil {
			p.Tariff = nil
		} else {
			var buf bytes.Buffer
			if err := json.Compact(&buf, []byte(raw)); err != nil {
				return tariffErr(err)
			}
			v := buf.String()
			p.Tariff = &v
		}
	}
	if in.OffpeakOnly != nil {
		p.OffpeakOnly = *in.OffpeakOnly
	}
	if !hasPeakWindows(p.Tariff) {
		if in.OffpeakOnly != nil && *in.OffpeakOnly {
			return userErr(ErrInvalid, CodeOffpeakNeedsPeak, nil, "只有设置了高峰时段的提供商才能开启“仅在空闲时段派发请求”")
		}
		p.OffpeakOnly = false // the switch only exists with a tariff
	}
	return nil
}

// parseProviderTariff parses provider.tariff; nil for none or invalid data.
func parseProviderTariff(raw *string) *tariff.Tariff {
	if raw == nil {
		return nil
	}
	tf, err := tariff.Parse(*raw)
	if err != nil {
		return nil
	}
	return tf
}

func hasPeakWindows(raw *string) bool {
	tf := parseProviderTariff(raw)
	return tf != nil && len(tf.PeakWindows) > 0
}

// parseTariffs parses the tariffs of the providers for the cache (Load).
func parseTariffs(provs []model.Provider) map[string]tariff.Tariff {
	out := make(map[string]tariff.Tariff, len(provs))
	for _, p := range provs {
		if p.Tariff == nil {
			continue
		}
		tf, err := tariff.Parse(*p.Tariff)
		if err != nil {
			slog.Warn("provider tariff is invalid; ignored", "provider", p.Name, "err", err)
			continue
		}
		if tf != nil {
			out[p.ID] = *tf
		}
	}
	return out
}

// cloneTariff deep-copies a tariff (callers may not change the cache).
func cloneTariff(t tariff.Tariff) tariff.Tariff {
	ws := make([]tariff.Window, len(t.PeakWindows))
	for i, w := range t.PeakWindows {
		w.Days = append([]int(nil), w.Days...)
		ws[i] = w
	}
	t.PeakWindows = ws
	if t.Models != nil {
		m := make(map[string]tariff.ModelPrice, len(t.Models))
		for k, v := range t.Models {
			m[k] = v
		}
		t.Models = m
	}
	return t
}

// ProviderTariff returns the parsed tariff of a provider (a copy); false
// when the provider has none (or it is invalid, or the provider is unknown).
func (s *Service) ProviderTariff(providerID string) (tariff.Tariff, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tariffs[providerID]
	if !ok {
		return tariff.Tariff{}, false
	}
	return cloneTariff(t), true
}

// FallbackPrice is the provider-level price (v1.1 A5.1 price fields, the
// PEAK price under a tariff, P12.2) in the form tariff.Cost takes; nil when
// the provider has no complete prices.
func FallbackPrice(p model.Provider) *tariff.ModelPrice {
	if !p.HasPrices() {
		return nil
	}
	mp := &tariff.ModelPrice{InputCacheHit: *p.PriceCacheHit, Input: *p.PriceInput, Output: *p.PriceOutput}
	if p.Currency != nil {
		mp.Currency = *p.Currency
	}
	return mp
}

// HasAnyPrice reports whether a cost can be estimated for some request of
// the provider: complete provider-level prices, or model prices in its
// tariff (D3). Without, only tokens are counted (§17.7). It is a property
// of the configuration, not of the requests made: with model prices only, a
// request for another model is still unpriced. Whether an amount is shown
// for a day therefore also depends on the day's unpriced requests
// (DisplayCost; lead ruling at the phase-2 gate); the daily budget applies
// when some provider HasAnyPrice.
func HasAnyPrice(p model.Provider) bool {
	if p.HasPrices() {
		return true
	}
	tf := parseProviderTariff(p.Tariff)
	return tf != nil && len(tf.Models) > 0
}

// ProviderCurrency is the currency of a provider's estimated costs: its own
// currency, else the currency of its tariff's model prices (the first one
// by model name that names a currency); nil when none is known.
func ProviderCurrency(p model.Provider) *string {
	if p.Currency != nil && strings.TrimSpace(*p.Currency) != "" {
		c := *p.Currency
		return &c
	}
	tf := parseProviderTariff(p.Tariff)
	if tf == nil {
		return nil
	}
	names := make([]string, 0, len(tf.Models))
	for n := range tf.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if c := strings.TrimSpace(tf.Models[n].Currency); c != "" {
			return &c
		}
	}
	return nil
}

// RequestCost estimates the cost of one request (P12.5, D3): priced in the
// period its start falls in (tariff.State), with the tariff's price of the
// model, else the provider-level price; ok is false without any price (only
// tokens count). currency falls back to the provider's currency.
func (s *Service) RequestCost(providerID, modelName string, start time.Time, promptTokens, cacheHitTokens, completionTokens int64) (cost float64, currency string, ok bool) {
	s.mu.RLock()
	p, found := s.providerLocked(providerID)
	tf, hasTariff := s.tariffs[providerID]
	hol := s.holidays
	s.mu.RUnlock()
	if !found {
		return 0, "", false
	}
	var cfg *tariff.Tariff
	peak := false
	if hasTariff {
		cfg = &tf
		if len(tf.PeakWindows) > 0 {
			peak, _ = tariff.State(start, tf, hol)
		}
	}
	cost, currency, ok = tariff.Cost(cfg, FallbackPrice(p), strings.TrimSpace(modelName), peak, promptTokens, cacheHitTokens, completionTokens)
	if ok && currency == "" && p.Currency != nil {
		currency = *p.Currency
	}
	return cost, currency, ok
}

// PeakState is P12.3 for a provider: whether now is in a peak window of its
// tariff and when that next changes (zero when it never does), and the
// P12.6 switch. ok is false for an unknown provider or one without a tariff
// with peak windows (never peak).
func (s *Service) PeakState(providerID string, now time.Time) (peak bool, nextChange time.Time, offpeakOnly bool, ok bool) {
	s.mu.RLock()
	p, found := s.providerLocked(providerID)
	tf, hasTariff := s.tariffs[providerID]
	hol := s.holidays
	s.mu.RUnlock()
	if !found || !hasTariff || len(tf.PeakWindows) == 0 {
		return false, time.Time{}, false, false
	}
	peak, nextChange = tariff.State(now, tf, hol)
	return peak, nextChange, p.OffpeakOnly, true
}

// ---------- holidays (P12.2: built-in file, viewable and editable) ----------

// HolidaysData is the holiday data in effect (GET /api/settings/holidays).
type HolidaysData struct {
	Years  map[string][]string // "2026": ["2026-01-01", ...]
	Edited bool                // the user's data replaces the built-in file
	Source string              // the "_source" note of the built-in file
}

// loadHolidays reads kv tariff.holidays_cn: the user's data when set and
// valid, else the embedded file.
func loadHolidays(ctx context.Context, st db.Store) (tariff.HolidaySet, bool, error) {
	v, ok, err := st.GetKV(ctx, model.KVHolidaysCN)
	if err != nil {
		return tariff.HolidaySet{}, false, err
	}
	if ok && strings.TrimSpace(v) != "" {
		h, err := tariff.ParseHolidays([]byte(v))
		if err == nil {
			return h, true, nil
		}
		slog.Warn("kv tariff.holidays_cn is invalid; using the built-in holidays", "err", err)
	}
	return tariff.HolidaysCN(), false, nil
}

// HolidaySet returns the holiday data in effect (cache).
func (s *Service) HolidaySet() tariff.HolidaySet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.holidays
}

// HolidayYears lists a holiday set as year → dates (both ascending).
func HolidayYears(h tariff.HolidaySet) map[string][]string {
	out := map[string][]string{}
	for _, y := range h.Years() {
		d := h.Dates(y)
		if d == nil {
			d = []string{}
		}
		out[fmt.Sprint(y)] = d
	}
	return out
}

// Holidays returns the holiday data in effect for the settings page.
func (s *Service) Holidays(ctx context.Context) (HolidaysData, error) {
	s.mu.RLock()
	h, edited := s.holidays, s.holidaysEdited
	s.mu.RUnlock()
	return HolidaysData{Years: HolidayYears(h), Edited: edited, Source: tariff.HolidaysCNSource()}, nil
}

// SetHolidays replaces the holiday data with the user's (kv
// tariff.holidays_cn, validated with tariff.ParseHolidays; errors carry the
// *tariff.Error). nil or empty years restores the built-in file. Each year's
// dates are stored sorted and de-duplicated. OnChange hooks run (the API pool
// re-checks off-peak waits).
func (s *Service) SetHolidays(ctx context.Context, years map[string][]string) error {
	val := ""
	if len(years) > 0 {
		norm := make(map[string][]string, len(years))
		for y, dates := range years {
			seen := map[string]bool{}
			list := []string{}
			for _, d := range dates {
				d = strings.TrimSpace(d)
				if !seen[d] {
					seen[d] = true
					list = append(list, d)
				}
			}
			sort.Strings(list)
			norm[strings.TrimSpace(y)] = list
		}
		b, err := json.Marshal(norm)
		if err != nil {
			return userErr(ErrInvalid, tariff.CodeInvalidHolidayData, nil, "节假日数据无效")
		}
		if _, err := tariff.ParseHolidays(b); err != nil {
			return tariffErr(err)
		}
		val = string(b)
	}
	s.wmu.Lock()
	err := s.st.SetKV(ctx, model.KVHolidaysCN, val)
	if err == nil {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

// ---------- UI language (P14.6, P14.7) ----------

// UILocales are the accepted ui.locale values: the P14.7 languages and the
// development pseudo-locale of P14.9 (web/i18n.js PSEUDO). "" = not chosen
// yet (the front end detects one from navigator.languages).
var UILocales = []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi", "en-XA"}

// ZhDisplays are the accepted ui.zh_display values (P14.6); "" = the default
// of the locale.
var ZhDisplays = []string{"simplified", "tw", "hk"}

// UISettings returns kv ui.locale and ui.zh_display ("" when not set).
func (s *Service) UISettings(ctx context.Context) (locale, zhDisplay string, err error) {
	read := func(key string) (string, error) {
		v, ok, err := s.st.GetKV(ctx, key)
		if err != nil || !ok {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
	if locale, err = read(model.KVUILocale); err != nil {
		return "", "", err
	}
	if zhDisplay, err = read(model.KVUIZhDisplay); err != nil {
		return "", "", err
	}
	return locale, zhDisplay, nil
}

// SetUISettings writes both values (validated; "" clears).
func (s *Service) SetUISettings(ctx context.Context, locale, zhDisplay string) error {
	locale, zhDisplay = strings.TrimSpace(locale), strings.TrimSpace(zhDisplay)
	if locale != "" && !oneOf(locale, UILocales) {
		return userErr(ErrInvalid, CodeLocaleUnsupported, map[string]any{"locale": locale}, "不支持的界面语言 %q", locale)
	}
	if zhDisplay != "" && !oneOf(zhDisplay, ZhDisplays) {
		return userErr(ErrInvalid, CodeZhDisplayInvalid, nil, "中文内容显示方式只能是 simplified、tw 或 hk")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.SetKV(ctx, model.KVUILocale, locale); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVUIZhDisplay, zhDisplay)
	})
}

func oneOf(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------- one-time startup steps (Open) ----------

// ApplyPhase2Defaults runs the one-time data steps of v2 phase 2 at
// startup, after migrations and Open (like ApplyRoleDefaults), each guarded
// by its kv flag:
//   - P12.2 / D3: the DeepSeek tariff preset for every DeepSeek provider
//     without a tariff (providers created before P12 never had one; those
//     created later get it in CreateProvider), kv KVTariffPreset;
//   - P12.5: estimated_cost of the usage_daily rows recorded before it (the
//     estimate shown until then: tokens × the provider prices), kv
//     KVUsageCostBackfill.
//
// It returns the names of the providers that got the preset and the number
// of usage rows priced. The cache is reloaded and OnChange hooks run when a
// provider changed. On an error the failed step runs again on the next start.
func (s *Service) ApplyPhase2Defaults(ctx context.Context) (presetProviders []string, pricedRows int, err error) {
	presetProviders, err = s.applyTariffPreset(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("填入 DeepSeek 的时段计价预设失败: %w", err)
	}
	if len(presetProviders) > 0 {
		s.wmu.Lock()
		err = s.Load(ctx)
		s.wmu.Unlock()
		if err != nil {
			return presetProviders, 0, err
		}
		s.changed()
		slog.Info("DeepSeek tariff preset filled in (SPEC-v2 P12.2, one time)", "providers", presetProviders)
	}
	pricedRows, err = s.backfillUsageCost(ctx)
	if err != nil {
		return presetProviders, 0, fmt.Errorf("补算已有用量的估算金额失败: %w", err)
	}
	if pricedRows > 0 {
		slog.Info("usage_daily estimated_cost backfilled (SPEC-v2 P12.5, one time)", "rows", pricedRows)
	}
	return presetProviders, pricedRows, nil
}

// applyTariffPreset fills tariff.DeepSeekPreset() into every DeepSeek
// provider without a tariff, once (kv KVTariffPreset): providers created
// before P12 never had one. Afterwards an empty tariff is the user's choice.
func (s *Service) applyTariffPreset(ctx context.Context) ([]string, error) {
	var names []string
	s.wmu.Lock()
	defer s.wmu.Unlock()
	err := s.st.Tx(ctx, func(tx db.Store) error {
		names = nil
		if v, ok, err := tx.GetKV(ctx, KVTariffPreset); err != nil || (ok && v != "") {
			return err
		}
		provs, err := tx.ListProviders(ctx)
		if err != nil {
			return err
		}
		preset := DeepSeekPresetJSON()
		for _, p := range provs {
			if p.Tariff != nil || !IsDeepSeek(p.BaseURL, p.BalanceAPI) {
				continue
			}
			v := preset
			p.Tariff = &v
			if err := tx.UpdateProvider(ctx, p); err != nil {
				return err
			}
			names = append(names, p.Name)
		}
		return tx.SetKV(ctx, KVTariffPreset, "1")
	})
	return names, err
}

// backfillUsageCost gives the usage_daily rows recorded before P12.5 (cost
// 0, tokens > 0) the estimate shown until then — tokens × the provider's
// prices — so today's spending and the budget do not restart at the
// upgrade. Once (kv KVUsageCostBackfill). Rows of providers without prices
// stay at 0.
func (s *Service) backfillUsageCost(ctx context.Context) (int, error) {
	n := 0
	s.wmu.Lock()
	defer s.wmu.Unlock()
	err := s.st.Tx(ctx, func(tx db.Store) error {
		n = 0
		if v, ok, err := tx.GetKV(ctx, KVUsageCostBackfill); err != nil || (ok && v != "") {
			return err
		}
		provs, err := tx.ListProviders(ctx)
		if err != nil {
			return err
		}
		rows, err := tx.ListUsageDaily(ctx, "")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.EstimatedCost != 0 || r.PromptTokens+r.CompletionTokens == 0 {
				continue
			}
			p, ok := findProvider(provs, r.ProviderID)
			if !ok {
				continue
			}
			c, ok := p.EstimateCost(r.PromptTokens, r.CacheHitTokens, r.CompletionTokens)
			if !ok || c <= 0 {
				continue
			}
			// AddUsageDaily adds: only the cost, no request or token.
			if err := tx.AddUsageDaily(ctx, model.UsageDaily{ProviderID: r.ProviderID, Day: r.Day, EstimatedCost: c}); err != nil {
				return err
			}
			n++
		}
		return tx.SetKV(ctx, KVUsageCostBackfill, "1")
	})
	return n, err
}
