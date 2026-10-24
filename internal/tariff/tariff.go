// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package tariff decides whether a provider is in its peak or off-peak
// pricing period and estimates what a request costs (SPEC-v2 P12, appendix D3).
//
// It is pure: no database, files, network, clock or randomness. The only data
// it reads is embedded in the binary: holidays_cn.json (go:embed) and the time
// zone database (time/tzdata, so Windows needs no system zoneinfo, P12.2).
// Callers pass the current time in.
package tariff

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // P12.2: zones load on Windows without a system database
)

// Window is one peak window: From (inclusive) to To (exclusive) on the tariff's
// wall clock, on the listed ISO weekdays. To may be "24:00" (until midnight).
// A window never wraps past midnight; an overnight period is two windows.
type Window struct {
	Days []int  `json:"days"` // ISO weekday, 1 = Monday
	From string `json:"from"` // "09:00"
	To   string `json:"to"`   // "12:00" (exclusive)
}

// ModelPrice is a price per million tokens at the PEAK rate (D3). The
// off-peak price is peak × Tariff.OffpeakMultiplier.
type ModelPrice struct {
	InputCacheHit float64 `json:"input_cache_hit"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Currency      string  `json:"currency"`
}

// Tariff is the provider.tariff JSON (P12.2 + D3).
//
// A tariff without peak windows is never in a peak period and has no
// time-of-day pricing: Cost charges the listed prices and OffpeakMultiplier
// may be omitted (NOTES.md, tariff-builder).
type Tariff struct {
	Timezone          string                `json:"timezone"`
	PeakWindows       []Window              `json:"peak_windows"`
	ExcludeHolidays   string                `json:"exclude_holidays,omitempty"` // "CN" or ""
	OffpeakMultiplier float64               `json:"offpeak_multiplier"`
	Models            map[string]ModelPrice `json:"models,omitempty"`
}

// ExcludeHolidaysCN is the only ExcludeHolidays value besides "": mainland
// China's public holidays (HolidaysCN) are off-peak all day.
const ExcludeHolidaysCN = "CN"

// Error codes of *Error (stable; the API answers with a code, P14.3).
const (
	CodeInvalidJSON        = "tariff.invalid_json"
	CodeInvalidTimezone    = "tariff.invalid_timezone"
	CodeInvalidWindow      = "tariff.invalid_window"
	CodeInvalidHolidays    = "tariff.invalid_exclude_holidays"
	CodeInvalidMultiplier  = "tariff.invalid_offpeak_multiplier"
	CodeInvalidPrice       = "tariff.invalid_model_price"
	CodeInvalidHolidayData = "tariff.invalid_holiday_data"
)

// Error is a validation error of a tariff or of holiday data. Code is one of
// the Code* constants and Params name the offending value, so a settings
// editor can show a translated message (P14.3).
type Error struct {
	Code   string
	Params map[string]any
	Msg    string // English, for logs and "show details"
}

func (e *Error) Error() string { return "tariff: " + e.Msg }

func invalid(code string, params map[string]any, format string, args ...any) *Error {
	return &Error{Code: code, Params: params, Msg: fmt.Sprintf(format, args...)}
}

// Parse decodes and validates a provider.tariff column. "" and "null" mean the
// provider has no tariff: (nil, nil). Errors are *Error.
func Parse(raw string) (*Tariff, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" {
		return nil, nil
	}
	var t Tariff
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, invalid(CodeInvalidJSON, nil, "invalid JSON: %v", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks the time zone, the windows, the holiday set, the multiplier
// and the model prices. Errors are *Error.
func (c Tariff) Validate() error {
	if _, err := loadZone(c.Timezone); err != nil {
		return invalid(CodeInvalidTimezone, map[string]any{"timezone": c.Timezone},
			"unknown time zone %q", c.Timezone)
	}
	for i, w := range c.PeakWindows {
		if _, reason := compileWindow(w); reason != "" {
			return invalid(CodeInvalidWindow, map[string]any{"index": i, "from": w.From, "to": w.To, "days": w.Days},
				"peak window %d: %s", i+1, reason)
		}
	}
	switch c.ExcludeHolidays {
	case "", ExcludeHolidaysCN:
	default:
		return invalid(CodeInvalidHolidays, map[string]any{"value": c.ExcludeHolidays},
			"exclude_holidays must be \"CN\" or empty, got %q", c.ExcludeHolidays)
	}
	m := c.OffpeakMultiplier
	if math.IsNaN(m) || m < 0 || m > 1 || (m == 0 && len(c.PeakWindows) > 0) {
		return invalid(CodeInvalidMultiplier, map[string]any{"value": m},
			"offpeak_multiplier must be greater than 0 and at most 1, got %v", m)
	}
	for name, p := range c.Models {
		if strings.TrimSpace(name) == "" {
			return invalid(CodeInvalidPrice, map[string]any{"model": name}, "empty model name")
		}
		for _, v := range []float64{p.InputCacheHit, p.Input, p.Output} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return invalid(CodeInvalidPrice, map[string]any{"model": name},
					"model %q: prices must be numbers not less than 0", name)
			}
		}
	}
	return nil
}

// DeepSeekPreset is the tariff filled in for a new DeepSeek provider: the
// P12.2 windows and the D3 peak prices per model. Each call returns fresh
// slices and maps.
func DeepSeekPreset() Tariff {
	weekdays := func() []int { return []int{1, 2, 3, 4, 5} }
	return Tariff{
		Timezone: "Asia/Shanghai",
		PeakWindows: []Window{
			{Days: weekdays(), From: "09:00", To: "12:00"},
			{Days: weekdays(), From: "14:00", To: "18:00"},
		},
		ExcludeHolidays:   ExcludeHolidaysCN,
		OffpeakMultiplier: 0.5,
		Models: map[string]ModelPrice{
			"deepseek-flash":  {InputCacheHit: 0.04, Input: 2.0, Output: 8.0, Currency: "CNY"},
			"deepseek-v4-pro": {InputCacheHit: 0.30, Input: 9.0, Output: 27.0, Currency: "CNY"},
		},
	}
}

// Cost estimates one request's cost (P12.5, D3). The price is cfg.Models[model]
// when present, else fallback (the provider-level prices, may be nil); with
// neither, ok is false and only tokens are counted. promptTokens includes
// cacheHitTokens (DeepSeek semantics, as model.Provider.EstimateCost): input
// applies to the non-cached part, input_cache_hit to the cached part, output to
// completion tokens. peak is the period the request started in (State); an
// off-peak request costs peak × OffpeakMultiplier when cfg has peak windows.
func Cost(cfg *Tariff, fallback *ModelPrice, model string, peak bool, promptTokens, cacheHitTokens, completionTokens int64) (cost float64, currency string, ok bool) {
	price := fallback
	if cfg != nil {
		if p, found := cfg.Models[model]; found {
			price = &p
		}
	}
	if price == nil {
		return 0, "", false
	}
	miss := promptTokens - cacheHitTokens
	if miss < 0 {
		miss = 0
	}
	cost = (float64(miss)*price.Input + float64(cacheHitTokens)*price.InputCacheHit +
		float64(completionTokens)*price.Output) / 1e6
	if !peak && cfg != nil && len(cfg.PeakWindows) > 0 && cfg.OffpeakMultiplier > 0 {
		cost *= cfg.OffpeakMultiplier
	}
	return cost, price.Currency, true
}

// minutesPerDay is the "24:00" end of a window.
const minutesPerDay = 24 * 60

// compileWindow checks a window and converts it to minutes; reason is "" when
// the window is valid.
func compileWindow(w Window) (s span, reason string) {
	if len(w.Days) == 0 {
		return s, "no days"
	}
	for _, d := range w.Days {
		if d < 1 || d > 7 {
			return s, fmt.Sprintf("day %d is not an ISO weekday (1 = Monday … 7 = Sunday)", d)
		}
		s.days[d] = true
	}
	var ok bool
	if s.from, ok = parseClock(w.From, false); !ok {
		return s, fmt.Sprintf("from %q is not a time HH:MM between 00:00 and 23:59", w.From)
	}
	if s.to, ok = parseClock(w.To, true); !ok {
		return s, fmt.Sprintf("to %q is not a time HH:MM between 00:00 and 24:00", w.To)
	}
	if s.from >= s.to {
		return s, fmt.Sprintf("from %s is not before to %s (split an overnight window in two)", w.From, w.To)
	}
	return s, ""
}

// parseClock parses "HH:MM" into minutes after midnight. "24:00" is accepted
// only for the end of a window.
func parseClock(s string, end bool) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	switch {
	case m > 59:
		return 0, false
	case h == 24 && m == 0 && end:
		return minutesPerDay, true
	case h > 23:
		return 0, false
	}
	return h*60 + m, true
}

// zones caches loaded locations by name (the embedded database is parsed on
// every LoadLocation; State runs every minute).
var zones sync.Map

// loadZone loads an IANA zone. "" and "Local" are rejected: both depend on the
// machine rather than on the tariff.
func loadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("no time zone")
	}
	if l, ok := zones.Load(name); ok {
		return l.(*time.Location), nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, l)
	return l, nil
}
