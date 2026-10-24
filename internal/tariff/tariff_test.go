// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

// The preset is P12.2's JSON with D3's "models", copied from SPEC-v2.md.
func TestDeepSeekPresetMatchesSpec(t *testing.T) {
	const spec = `{
  "timezone": "Asia/Shanghai",
  "peak_windows": [{"days": [1,2,3,4,5], "from": "09:00", "to": "12:00"},
                   {"days": [1,2,3,4,5], "from": "14:00", "to": "18:00"}],
  "exclude_holidays": "CN",
  "offpeak_multiplier": 0.5,
  "models": {
    "deepseek-flash":  {"input_cache_hit": 0.04, "input": 2.0, "output": 8.0, "currency": "CNY"},
    "deepseek-v4-pro": {"input_cache_hit": 0.30, "input": 9.0, "output": 27.0, "currency": "CNY"}
  }
}`
	var want Tariff
	if err := json.Unmarshal([]byte(spec), &want); err != nil {
		t.Fatal(err)
	}
	got := DeepSeekPreset()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeepSeekPreset() = %+v\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	// Fresh values each call: changing one preset does not change the next.
	got.PeakWindows[0].Days[0] = 7
	got.Models["deepseek-flash"] = ModelPrice{}
	if again := DeepSeekPreset(); !reflect.DeepEqual(again, want) {
		t.Fatal("DeepSeekPreset shares slices or maps between calls")
	}
}

func TestParse(t *testing.T) {
	for _, raw := range []string{"", "null", "  null\n", " \t"} {
		if got, err := Parse(raw); got != nil || err != nil {
			t.Errorf("Parse(%q) = %v, %v; want nil, nil", raw, got, err)
		}
	}
	// Round trip of the preset.
	b, err := json.Marshal(DeepSeekPreset())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, DeepSeekPreset()) {
		t.Fatalf("round trip = %+v", *got)
	}
	valid := []string{
		`{"timezone":"Europe/London","peak_windows":[{"days":[7],"from":"18:00","to":"24:00"}],"offpeak_multiplier":1}`,
		`{"timezone":"UTC","peak_windows":[{"days":[1,1,6],"from":"00:00","to":"00:01"}],"offpeak_multiplier":0.25}`,
		// No windows: no time-of-day pricing; the multiplier may be omitted.
		`{"timezone":"UTC","peak_windows":[],"models":{"claude-opus-5-5":{"input":15,"input_cache_hit":1.5,"output":75,"currency":"USD"}}}`,
		`{"timezone":"Asia/Shanghai","peak_windows":null}`,
		// Unknown fields are ignored (forward compatibility).
		`{"timezone":"UTC","peak_windows":[],"offpeak_multiplier":0.5,"future_field":true}`,
	}
	for _, raw := range valid {
		if _, err := Parse(raw); err != nil {
			t.Errorf("Parse(%s): %v", raw, err)
		}
	}
	window := func(w string) string {
		return `{"timezone":"Asia/Shanghai","offpeak_multiplier":0.5,"peak_windows":[` + w + `]}`
	}
	invalid := []struct {
		raw, code string
	}{
		{`{`, CodeInvalidJSON},
		{`[]`, CodeInvalidJSON},
		{`{"timezone":"Asia/Shanghai","peak_windows":"x"}`, CodeInvalidJSON},
		{`{"peak_windows":[]}`, CodeInvalidTimezone},
		{`{"timezone":"Local","peak_windows":[]}`, CodeInvalidTimezone},
		{`{"timezone":"Mars/Olympus_Mons","peak_windows":[]}`, CodeInvalidTimezone},
		{window(`{"days":[],"from":"09:00","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"from":"09:00","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"days":[0],"from":"09:00","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"days":[8],"from":"09:00","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"9:00","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"09:60","to":"12:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"24:00","to":"24:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"09:00","to":"24:01"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"09:00","to":"25:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"09:00","to":"09:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"22:00","to":"02:00"}`), CodeInvalidWindow},
		{window(`{"days":[1],"from":"09:00","to":"12：00"}`), CodeInvalidWindow},
		{`{"timezone":"UTC","peak_windows":[],"exclude_holidays":"US"}`, CodeInvalidHolidays},
		{`{"timezone":"UTC","peak_windows":[],"exclude_holidays":"cn"}`, CodeInvalidHolidays},
		{`{"timezone":"UTC","peak_windows":[{"days":[1],"from":"09:00","to":"12:00"}]}`, CodeInvalidMultiplier},
		{`{"timezone":"UTC","peak_windows":[],"offpeak_multiplier":-0.5}`, CodeInvalidMultiplier},
		{`{"timezone":"UTC","peak_windows":[],"offpeak_multiplier":1.5}`, CodeInvalidMultiplier},
		{`{"timezone":"UTC","peak_windows":[],"models":{"m":{"input":-1}}}`, CodeInvalidPrice},
		{`{"timezone":"UTC","peak_windows":[],"models":{" ":{"input":1}}}`, CodeInvalidPrice},
	}
	for _, c := range invalid {
		got, err := Parse(c.raw)
		var e *Error
		if got != nil || !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("Parse(%s) = %v, %v; want code %s", c.raw, got, err, c.code)
		}
	}
	// Validate also rejects NaN and infinities, which JSON cannot carry.
	for _, bad := range []Tariff{
		{Timezone: "UTC", OffpeakMultiplier: math.NaN()},
		{Timezone: "UTC", Models: map[string]ModelPrice{"m": {Output: math.Inf(1)}}},
		{Timezone: "UTC", Models: map[string]ModelPrice{"m": {InputCacheHit: math.NaN()}}},
	} {
		if bad.Validate() == nil {
			t.Errorf("Validate(%+v) = nil", bad)
		}
	}
}

func TestCost(t *testing.T) {
	preset := DeepSeekPreset()
	noWindows := Tariff{Timezone: "UTC", Models: map[string]ModelPrice{
		"deepseek-flash": {InputCacheHit: 0.04, Input: 2.0, Output: 8.0, Currency: "CNY"}}}
	fallback := &ModelPrice{InputCacheHit: 0.1, Input: 1.0, Output: 2.0, Currency: "USD"}
	// 1M prompt tokens of which 400k hit the cache, 100k completion tokens.
	const prompt, hit, completion = 1_000_000, 400_000, 100_000
	for _, c := range []struct {
		name     string
		cfg      *Tariff
		fallback *ModelPrice
		model    string
		peak     bool
		prompt   int64
		hit      int64
		want     float64
		currency string
		ok       bool
	}{
		// flash: 0.6M × 2.0 + 0.4M × 0.04 + 0.1M × 8.0 = 1.2 + 0.016 + 0.8
		{"flash peak", &preset, nil, "deepseek-flash", true, prompt, hit, 2.016, "CNY", true},
		{"flash off-peak", &preset, nil, "deepseek-flash", false, prompt, hit, 1.008, "CNY", true},
		// v4-pro: 0.6M × 9 + 0.4M × 0.30 + 0.1M × 27 = 5.4 + 0.12 + 2.7
		{"v4-pro peak", &preset, nil, "deepseek-v4-pro", true, prompt, hit, 8.22, "CNY", true},
		{"v4-pro off-peak", &preset, nil, "deepseek-v4-pro", false, prompt, hit, 4.11, "CNY", true},
		{"model price wins over fallback", &preset, fallback, "deepseek-flash", true, prompt, hit, 2.016, "CNY", true},
		// Fallback: 0.6M × 1 + 0.4M × 0.1 + 0.1M × 2 = 0.6 + 0.04 + 0.2
		{"fallback peak", &preset, fallback, "deepseek-chat", true, prompt, hit, 0.84, "USD", true},
		{"fallback off-peak", &preset, fallback, "deepseek-chat", false, prompt, hit, 0.42, "USD", true},
		{"no price", &preset, nil, "deepseek-chat", true, prompt, hit, 0, "", false},
		{"no tariff, fallback", nil, fallback, "deepseek-flash", false, prompt, hit, 0.84, "USD", true},
		{"no tariff, no price", nil, nil, "deepseek-flash", true, prompt, hit, 0, "", false},
		{"no windows: listed price", &noWindows, nil, "deepseek-flash", false, prompt, hit, 2.016, "CNY", true},
		{"no cache hits", &preset, nil, "deepseek-flash", true, prompt, 0, 2.8, "CNY", true},
		{"all cached", &preset, nil, "deepseek-flash", true, prompt, prompt, 0.84, "CNY", true},
		// More hits than prompt tokens: the miss part is clamped at 0.
		{"hits above prompt", &preset, nil, "deepseek-flash", true, 0, hit, 0.816, "CNY", true},
	} {
		cost, currency, ok := Cost(c.cfg, c.fallback, c.model, c.peak, c.prompt, c.hit, completion)
		if ok != c.ok || currency != c.currency || math.Abs(cost-c.want) > 1e-9 {
			t.Errorf("%s: Cost = %v %q %v; want %v %q %v", c.name, cost, currency, ok, c.want, c.currency, c.ok)
		}
	}
}
