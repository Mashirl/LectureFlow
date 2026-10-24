// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/tariff"
)

// v2 phase 2 (SPEC-v2 P12.2, P12.5, P12.6, D3, P14.6, P14.7): provider
// tariffs, request costs, holidays and the UI language settings.

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return l
}()

// bj is a Beijing wall-clock time "2006-01-02 15:04".
func bj(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCreateProviderFillsDeepSeekPreset(t *testing.T) {
	s, _ := newService(t)
	preset := tariff.DeepSeekPreset()
	for _, tc := range []struct {
		name, typ, base, balance string
		want                     bool
	}{
		{"DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", "", true},
		{"DeepSeek v1 path", model.ProviderOpenAI, "https://api.deepseek.com/v1", "", true},
		{"DeepSeek Anthropic", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", "", true},
		{"Proxy with DeepSeek balance", model.ProviderOpenAI, "http://127.0.0.1:9999", model.BalanceDeepSeek, true},
		{"OpenAI", model.ProviderOpenAI, "https://api.openai.com/v1", "", false},
		{"Claude", model.ProviderAnthropic, "https://api.anthropic.com", "", false},
		{"Not DeepSeek", model.ProviderOpenAI, "https://notdeepseek.com", "", false},
	} {
		in := ProviderInput{Name: sp(tc.name), Type: sp(tc.typ), BaseURL: sp(tc.base), APIKey: sp(fakeKey)}
		if tc.balance != "" {
			in.BalanceAPI = sp(tc.balance)
		}
		p, err := s.CreateProvider(bg(), in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		tf, ok := s.ProviderTariff(p.ID)
		if !tc.want {
			if p.Tariff != nil || ok {
				t.Errorf("%s: unexpected tariff %v", tc.name, *p.Tariff)
			}
			continue
		}
		if p.Tariff == nil || *p.Tariff != DeepSeekPresetJSON() || !ok || !reflect.DeepEqual(tf, preset) {
			t.Errorf("%s: tariff %v / %+v", tc.name, p.Tariff, tf)
		}
		if p.OffpeakOnly {
			t.Errorf("%s: offpeak_only on by default", tc.name)
		}
	}
	// An explicit tariff or "none" in the input wins over the preset.
	custom := `{"timezone":"Asia/Shanghai","models":{"deepseek-flash":{"input":1,"output":2,"input_cache_hit":0.1,"currency":"CNY"}}}`
	p, err := s.CreateProvider(bg(), ProviderInput{Name: sp("DS custom"), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com"),
		APIKey: sp(fakeKey), Tariff: sp(custom)})
	if err != nil || p.Tariff == nil || *p.Tariff != custom {
		t.Fatalf("custom tariff %v %v", p.Tariff, err)
	}
	p, err = s.CreateProvider(bg(), ProviderInput{Name: sp("DS none"), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com"),
		APIKey: sp(fakeKey), ClearTariff: true})
	if err != nil || p.Tariff != nil {
		t.Fatalf("cleared tariff %v %v", p.Tariff, err)
	}
	p, err = s.CreateProvider(bg(), ProviderInput{Name: sp("DS null"), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com"),
		APIKey: sp(fakeKey), Tariff: sp("null")})
	if err != nil || p.Tariff != nil {
		t.Fatalf("null tariff %v %v", p.Tariff, err)
	}
	// Returned providers are copies, the tariff text included.
	c, _ := s.Provider(p.ID)
	if c.Tariff != nil {
		t.Fatal("cache")
	}
	ds, _ := s.CreateProvider(bg(), ProviderInput{Name: sp("DS copy"), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com"), APIKey: sp(fakeKey)})
	c, _ = s.Provider(ds.ID)
	*c.Tariff = "mutated"
	if c2, _ := s.Provider(ds.ID); *c2.Tariff != DeepSeekPresetJSON() {
		t.Fatal("Provider shares the tariff string")
	}
	tf, _ := s.ProviderTariff(ds.ID)
	tf.PeakWindows[0].Days[0] = 7
	tf.Models["deepseek-flash"] = tariff.ModelPrice{}
	if tf2, _ := s.ProviderTariff(ds.ID); !reflect.DeepEqual(tf2, preset) {
		t.Fatal("ProviderTariff shares the cached tariff")
	}
}

func TestUpdateProviderTariffAndOffpeakOnly(t *testing.T) {
	s, st := newService(t)
	o := create(t, s, "Other", model.ProviderOpenAI, "https://llm.example.org/v1", fakeKey)
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })

	// Without a tariff the switch cannot be turned on.
	if _, err := s.UpdateProvider(bg(), o.ID, ProviderInput{OffpeakOnly: bp(true)}); !errors.Is(err, ErrInvalid) || codeOf(err) != CodeOffpeakNeedsPeak {
		t.Fatalf("offpeak without tariff: %v", err)
	}
	// Models only (D3: prices without time-of-day pricing): still no switch.
	modelsOnly := `{"timezone": "UTC", "models": {"m": {"input": 1, "output": 2, "input_cache_hit": 0.5, "currency": "USD"}}}`
	if _, err := s.UpdateProvider(bg(), o.ID, ProviderInput{Tariff: sp(modelsOnly), OffpeakOnly: bp(true)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("offpeak without peak windows: %v", err)
	}
	if hooks.Load() != 0 {
		t.Fatalf("failed writes called hooks %d times", hooks.Load())
	}

	// A valid tariff is stored compacted, unknown fields kept.
	raw := `{
	  "timezone": "Europe/London",
	  "peak_windows": [{"days": [1,2,3,4,5], "from": "08:00", "to": "20:00"}],
	  "offpeak_multiplier": 0.25,
	  "future_field": {"x": 1}
	}`
	u, err := s.UpdateProvider(bg(), o.ID, ProviderInput{Tariff: sp(raw), OffpeakOnly: bp(true)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"timezone":"Europe/London","peak_windows":[{"days":[1,2,3,4,5],"from":"08:00","to":"20:00"}],"offpeak_multiplier":0.25,"future_field":{"x":1}}`
	if u.Tariff == nil || *u.Tariff != want || !u.OffpeakOnly {
		t.Fatalf("stored %v offpeak %v", u.Tariff, u.OffpeakOnly)
	}
	if got, _ := st.GetProvider(bg(), o.ID); got.Tariff == nil || *got.Tariff != want || !got.OffpeakOnly {
		t.Fatalf("db %+v", got)
	}
	if tf, ok := s.ProviderTariff(o.ID); !ok || tf.Timezone != "Europe/London" || tf.OffpeakMultiplier != 0.25 {
		t.Fatalf("cache %+v %v", tf, ok)
	}
	// Other patches leave tariff and switch alone.
	if u, err = s.UpdateProvider(bg(), o.ID, ProviderInput{RPM: ip(120)}); err != nil || u.Tariff == nil || !u.OffpeakOnly {
		t.Fatalf("unrelated patch %+v %v", u, err)
	}

	// Invalid tariffs: ErrInvalid, the *tariff.Error with its code and params,
	// a Chinese message, nothing changed.
	for _, tc := range []struct {
		raw, code string
		params    map[string]any
	}{
		{`{"timezone": `, tariff.CodeInvalidJSON, nil},
		{`{"timezone": "Mars/Olympus"}`, tariff.CodeInvalidTimezone, map[string]any{"timezone": "Mars/Olympus"}},
		{`{"timezone": "UTC", "peak_windows": [{"days": [1], "from": "09:00", "to": "10:00"}, {"days": [8], "from": "09:00", "to": "10:00"}], "offpeak_multiplier": 0.5}`,
			tariff.CodeInvalidWindow, map[string]any{"index": 1}},
		{`{"timezone": "UTC", "exclude_holidays": "US"}`, tariff.CodeInvalidHolidays, nil},
		{`{"timezone": "UTC", "peak_windows": [{"days": [1], "from": "09:00", "to": "10:00"}]}`, tariff.CodeInvalidMultiplier, nil},
		{`{"timezone": "UTC", "models": {"m": {"input": -1}}}`, tariff.CodeInvalidPrice, map[string]any{"model": "m"}},
	} {
		_, err := s.UpdateProvider(bg(), o.ID, ProviderInput{Tariff: sp(tc.raw)})
		var te *tariff.Error
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &te) || te.Code != tc.code {
			t.Errorf("%s: %v (%#v)", tc.raw, err, te)
			continue
		}
		// v2 P14.3: the coded form carries the tariff code and params.
		if !hasCode(err, tc.code, te.Params) {
			t.Errorf("%s: coded form %q", tc.raw, codeOf(err))
		}
		for k, v := range tc.params {
			if te.Params[k] != v {
				t.Errorf("%s: param %s = %v, want %v", tc.code, k, te.Params[k], v)
			}
		}
		if msg := err.Error(); msg == "" || strings.HasPrefix(msg, "tariff:") || !containsHan(msg) {
			t.Errorf("%s: message %q is not a Chinese sentence", tc.code, msg)
		}
		if strings.Contains(err.Error(), fakeKey) {
			t.Error("key in error")
		}
	}
	if c, _ := s.Provider(o.ID); c.Tariff == nil || *c.Tariff != want || c.RPM != 120 {
		t.Fatalf("failed patches changed the provider: %+v", c)
	}
	if hooks.Load() != 2 {
		t.Fatalf("hooks %d, want 2", hooks.Load())
	}

	// Turning the switch off, then clearing the tariff (also switches off).
	if u, err = s.UpdateProvider(bg(), o.ID, ProviderInput{OffpeakOnly: bp(false)}); err != nil || u.OffpeakOnly || u.Tariff == nil {
		t.Fatalf("switch off %+v %v", u, err)
	}
	if _, err = s.UpdateProvider(bg(), o.ID, ProviderInput{OffpeakOnly: bp(true)}); err != nil {
		t.Fatal(err)
	}
	if u, err = s.UpdateProvider(bg(), o.ID, ProviderInput{ClearTariff: true}); err != nil || u.Tariff != nil || u.OffpeakOnly {
		t.Fatalf("clear %+v %v", u, err)
	}
	if _, ok := s.ProviderTariff(o.ID); ok {
		t.Fatal("cache still has the tariff")
	}
	// "null" clears as well.
	if _, err = s.UpdateProvider(bg(), o.ID, ProviderInput{Tariff: sp(modelsOnly)}); err != nil {
		t.Fatal(err)
	}
	if u, err = s.UpdateProvider(bg(), o.ID, ProviderInput{Tariff: sp(" null ")}); err != nil || u.Tariff != nil {
		t.Fatalf("null %+v %v", u, err)
	}
}

func bp(b bool) *bool { return &b }

func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// Providers created before P12 have no tariff; the first start fills the
// DeepSeek preset once, so a tariff the user removes later stays removed.
func TestTariffPresetFilledOnceAtStartup(t *testing.T) {
	st := openStore(t)
	st.SetKV(bg(), model.KVSettingsImported, "1")
	old := []model.Provider{
		{ID: "ds", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com", APIKey: fakeKey, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceDeepSeek},
		{ID: "proxy", Name: "Proxy", Type: model.ProviderOpenAI, BaseURL: "http://127.0.0.1:8000", MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceDeepSeek},
		{ID: "oa", Name: "OpenAI", Type: model.ProviderOpenAI, BaseURL: "https://api.openai.com/v1", APIKey: fakeKey, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone},
		{ID: "cl", Name: "Claude", Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com", APIKey: fakeKey, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone},
	}
	for _, p := range old {
		if err := st.InsertProvider(bg(), p); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Open alone changes nothing (the step runs at startup, in main).
	if got, _ := s.Provider("ds"); got.Tariff != nil {
		t.Fatal("Open filled the preset")
	}
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })
	names, rows, err := s.ApplyPhase2Defaults(bg())
	if err != nil || !reflect.DeepEqual(names, []string{"DeepSeek", "Proxy"}) || rows != 0 || hooks.Load() != 1 {
		t.Fatalf("filled %v rows %d err %v hooks %d", names, rows, err, hooks.Load())
	}
	for _, p := range old {
		got, _ := s.Provider(p.ID)
		want := p.ID == "ds" || p.ID == "proxy"
		if want != (got.Tariff != nil) || (want && *got.Tariff != DeepSeekPresetJSON()) {
			t.Errorf("%s: tariff %v", p.ID, got.Tariff)
		}
		if _, ok := s.ProviderTariff(p.ID); ok != want {
			t.Errorf("%s: cache %v", p.ID, ok)
		}
	}
	if v, ok, _ := st.GetKV(bg(), KVTariffPreset); !ok || v != "1" {
		t.Fatalf("flag %q %v", v, ok)
	}
	// The user removes the tariff; the next start does not bring it back.
	if _, err := s.UpdateProvider(bg(), "ds", ProviderInput{ClearTariff: true}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names, _, err := s2.ApplyPhase2Defaults(bg()); err != nil || len(names) != 0 {
		t.Fatalf("second start filled %v %v", names, err)
	}
	if got, _ := s2.Provider("ds"); got.Tariff != nil {
		t.Fatalf("preset filled again: %v", *got.Tariff)
	}
}

// A first start that imports config.toml gives the imported DeepSeek
// provider the preset too.
func TestImportedDeepSeekProviderGetsPreset(t *testing.T) {
	st := openStore(t)
	s, err := Open(bg(), st, legacyConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyPhase2Defaults(bg()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Provider("deepseek"); got.Tariff == nil || *got.Tariff != DeepSeekPresetJSON() {
		t.Fatalf("deepseek tariff %v", got.Tariff)
	}
	if got, _ := s.Provider("ollama"); got.Tariff != nil {
		t.Fatalf("ollama tariff %v", *got.Tariff)
	}
}

// usage_daily rows recorded before P12.5 get the estimate shown until then
// (tokens × provider prices), once.
func TestUsageCostBackfillOnce(t *testing.T) {
	st := openStore(t)
	st.SetKV(bg(), model.KVSettingsImported, "1")
	st.SetKV(bg(), KVTariffPreset, "1")
	priced := model.Provider{ID: "priced", Name: "Priced", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org", APIKey: fakeKey,
		MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone, PriceInput: fp(2), PriceCacheHit: fp(0.5), PriceOutput: fp(8)}
	free := model.Provider{ID: "free", Name: "Free", Type: model.ProviderOpenAI, BaseURL: "http://127.0.0.1:1", MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone}
	for _, p := range []model.Provider{priced, free} {
		if err := st.InsertProvider(bg(), p); err != nil {
			t.Fatal(err)
		}
	}
	rows := []model.UsageDaily{
		{ProviderID: "priced", Day: "2026-10-20", Requests: 3, PromptTokens: 1_000_000, CacheHitTokens: 400_000, CompletionTokens: 100_000},
		{ProviderID: "priced", Day: "2026-10-21", Requests: 1, PromptTokens: 500_000, CompletionTokens: 0, EstimatedCost: 0.25}, // already priced
		{ProviderID: "free", Day: "2026-10-21", Requests: 1, PromptTokens: 500_000},
		{ProviderID: "gone", Day: "2026-10-21", Requests: 1, PromptTokens: 500_000},
	}
	for _, r := range rows {
		if err := st.AddUsageDaily(bg(), r); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		got, err := st.ListUsageDaily(bg(), "")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]float64{"priced/2026-10-20": (600_000*2 + 400_000*0.5 + 100_000*8) / 1e6, "priced/2026-10-21": 0.25,
			"free/2026-10-21": 0, "gone/2026-10-21": 0}
		if len(got) != len(want) {
			t.Fatalf("rows %+v", got)
		}
		for _, r := range got {
			if w := want[r.ProviderID+"/"+r.Day]; !near(r.EstimatedCost, w) {
				t.Errorf("%s/%s: cost %v want %v", r.ProviderID, r.Day, r.EstimatedCost, w)
			}
			if r.ProviderID == "priced" && r.Day == "2026-10-20" && (r.Requests != 3 || r.PromptTokens != 1_000_000) {
				t.Errorf("tokens changed: %+v", r)
			}
		}
	}
	for i := 0; i < 2; i++ { // the second start adds nothing
		s, err := Open(bg(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, rows, err := s.ApplyPhase2Defaults(bg()); err != nil || rows != map[int]int{0: 1, 1: 0}[i] {
			t.Fatalf("start %d: rows %d %v", i, rows, err)
		}
		check()
	}
}

func TestRequestCostByPeriodAndModel(t *testing.T) {
	s, _ := newService(t)
	ds, err := s.CreateProvider(bg(), ProviderInput{Name: sp("DeepSeek"), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com"),
		APIKey: sp(fakeKey), PriceInput: fp(4), PriceCacheHit: fp(1), PriceOutput: fp(16), Currency: sp("CNY")})
	if err != nil {
		t.Fatal(err)
	}
	plain := create(t, s, "No prices", model.ProviderOpenAI, "https://llm.example.org", fakeKey)
	const prompt, hit, completion = 1_000_000, 400_000, 100_000
	flashPeak := (600_000*2.0 + 400_000*0.04 + 100_000*8.0) / 1e6
	fallbackPeak := (600_000*4.0 + 400_000*1.0 + 100_000*16.0) / 1e6
	for _, tc := range []struct {
		name, model, at string
		want            float64
	}{
		{"flash, Wednesday peak", "deepseek-flash", "2026-10-21 10:00", flashPeak},
		{"flash, Wednesday lunch", "deepseek-flash", "2026-10-21 13:00", flashPeak / 2},
		{"flash, 18:00 is off-peak", "deepseek-flash", "2026-10-21 18:00", flashPeak / 2},
		{"flash, Saturday", "deepseek-flash", "2026-10-24 10:00", flashPeak / 2},
		{"flash, National Day holiday (Monday)", "deepseek-flash", "2026-10-05 10:00", flashPeak / 2},
		{"pro, peak", "deepseek-v4-pro", "2026-10-21 15:00", (600_000*9.0 + 400_000*0.3 + 100_000*27.0) / 1e6},
		{"unknown model falls back to provider prices, peak", "deepseek-chat", "2026-10-21 10:00", fallbackPeak},
		{"unknown model falls back to provider prices, off-peak", "deepseek-chat", "2026-10-21 20:00", fallbackPeak / 2},
	} {
		// The start time decides, in whatever zone it is given.
		start := bj(t, tc.at).In(time.FixedZone("UTC-7", -7*3600))
		cost, cur, ok := s.RequestCost(ds.ID, tc.model, start, prompt, hit, completion)
		if !ok || !near(cost, tc.want) || cur != "CNY" {
			t.Errorf("%s: %v %q %v, want %v", tc.name, cost, cur, ok, tc.want)
		}
	}
	if _, _, ok := s.RequestCost(plain.ID, "m", bj(t, "2026-10-21 10:00"), prompt, hit, completion); ok {
		t.Error("provider without prices must not be priced")
	}
	if _, _, ok := s.RequestCost("missing", "m", bj(t, "2026-10-21 10:00"), prompt, hit, completion); ok {
		t.Error("unknown provider priced")
	}
	// Provider-level prices without a tariff: the price as is, any time.
	if _, err := s.UpdateProvider(bg(), plain.ID, ProviderInput{PriceInput: fp(1), PriceCacheHit: fp(1), PriceOutput: fp(1), Currency: sp("USD")}); err != nil {
		t.Fatal(err)
	}
	if cost, cur, ok := s.RequestCost(plain.ID, "m", bj(t, "2026-10-24 03:00"), prompt, hit, completion); !ok || !near(cost, 1.1) || cur != "USD" {
		t.Errorf("plain: %v %q %v", cost, cur, ok)
	}
	// User-edited holidays apply: Wednesday 21 October becomes a holiday.
	if err := s.SetHolidays(bg(), map[string][]string{"2026": {"2026-10-21"}}); err != nil {
		t.Fatal(err)
	}
	if cost, _, _ := s.RequestCost(ds.ID, "deepseek-flash", bj(t, "2026-10-21 10:00"), prompt, hit, completion); !near(cost, flashPeak/2) {
		t.Errorf("edited holiday: %v", cost)
	}
	// ... and the built-in National Day is gone from the edited data.
	if cost, _, _ := s.RequestCost(ds.ID, "deepseek-flash", bj(t, "2026-10-05 10:00"), prompt, hit, completion); !near(cost, flashPeak) {
		t.Errorf("edited data without National Day: %v", cost)
	}
}

func TestPeakState(t *testing.T) {
	s, _ := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	plain := create(t, s, "Plain", model.ProviderOpenAI, "https://llm.example.org", fakeKey)
	peak, next, only, ok := s.PeakState(ds.ID, bj(t, "2026-10-21 10:30"))
	if !ok || !peak || only || !next.Equal(bj(t, "2026-10-21 12:00")) {
		t.Fatalf("peak state %v %v %v %v", peak, next, only, ok)
	}
	peak, next, _, _ = s.PeakState(ds.ID, bj(t, "2026-10-23 18:30")) // Friday evening
	if peak || !next.Equal(bj(t, "2026-10-26 09:00")) {
		t.Fatalf("off-peak state %v %v", peak, next)
	}
	if _, err := s.UpdateProvider(bg(), ds.ID, ProviderInput{OffpeakOnly: bp(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, only, _ = s.PeakState(ds.ID, bj(t, "2026-10-21 10:30")); !only {
		t.Fatal("offpeak_only not reported")
	}
	if _, _, _, ok := s.PeakState(plain.ID, bj(t, "2026-10-21 10:30")); ok {
		t.Fatal("provider without tariff has a peak state")
	}
	models := `{"timezone": "UTC", "models": {"m": {"input": 1, "output": 1, "input_cache_hit": 1}}}`
	if _, err := s.UpdateProvider(bg(), plain.ID, ProviderInput{Tariff: sp(models)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := s.PeakState(plain.ID, bj(t, "2026-10-21 10:30")); ok {
		t.Fatal("a tariff without peak windows is never peak")
	}
	if _, _, _, ok := s.PeakState("missing", time.Now()); ok {
		t.Fatal("unknown provider")
	}
}

func TestHolidaysEditAndRestore(t *testing.T) {
	s, st := newService(t)
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })
	h, err := s.Holidays(bg())
	if err != nil || h.Edited || len(h.Years) != 1 || len(h.Years["2026"]) != 33 || h.Source == "" || h.Source != tariff.HolidaysCNSource() {
		t.Fatalf("built-in %+v %v", h, err)
	}
	if !s.HolidaySet().IsHoliday("2026-10-01") || s.HolidaySet().Has(2027) {
		t.Fatal("built-in set")
	}

	err = s.SetHolidays(bg(), map[string][]string{"2027": {"2027-02-07", " 2027-01-01", "2027-01-01"}, "2026": {}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ = s.Holidays(bg())
	want := map[string][]string{"2026": {}, "2027": {"2027-01-01", "2027-02-07"}}
	if !h.Edited || !reflect.DeepEqual(h.Years, want) || h.Source == "" {
		t.Fatalf("edited %+v", h)
	}
	if hs := s.HolidaySet(); !hs.Has(2027) || !hs.Has(2026) || hs.IsHoliday("2026-10-01") || !hs.IsHoliday("2027-02-07") {
		t.Fatal("edited set")
	}
	if v, _, _ := st.GetKV(bg(), model.KVHolidaysCN); v != `{"2026":[],"2027":["2027-01-01","2027-02-07"]}` {
		t.Fatalf("kv %q", v)
	}
	if hooks.Load() != 1 {
		t.Fatalf("hooks %d", hooks.Load())
	}

	// Invalid data: the *tariff.Error, nothing changed, no hook.
	for _, bad := range []map[string][]string{
		{"2027": {"2026-01-01"}},
		{"27": {"2027-01-01"}},
		{"2027": {"2027-02-30"}},
	} {
		err := s.SetHolidays(bg(), bad)
		var te *tariff.Error
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &te) || te.Code != tariff.CodeInvalidHolidayData || !containsHan(err.Error()) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if h2, _ := s.Holidays(bg()); !reflect.DeepEqual(h2.Years, want) || hooks.Load() != 1 {
		t.Fatalf("failed writes changed data: %+v hooks %d", h2, hooks.Load())
	}

	// The edit survives a restart.
	s2, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := s2.Holidays(bg()); !h2.Edited || !reflect.DeepEqual(h2.Years, want) {
		t.Fatalf("after restart %+v", h2)
	}

	// nil restores the built-in file.
	if err := s.SetHolidays(bg(), nil); err != nil {
		t.Fatal(err)
	}
	if h, _ = s.Holidays(bg()); h.Edited || len(h.Years["2026"]) != 33 || hooks.Load() != 2 {
		t.Fatalf("restored %+v hooks %d", h, hooks.Load())
	}

	// Broken kv data (written by hand): the built-in file is used.
	if err := st.SetKV(bg(), model.KVHolidaysCN, "{broken"); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h3, _ := s3.Holidays(bg()); h3.Edited || len(h3.Years["2026"]) != 33 {
		t.Fatalf("broken kv %+v", h3)
	}
}

func TestUISettings(t *testing.T) {
	s, st := newService(t)
	if l, z, err := s.UISettings(bg()); err != nil || l != "" || z != "" {
		t.Fatalf("defaults %q %q %v", l, z, err)
	}
	for _, l := range UILocales {
		for _, z := range append([]string{""}, ZhDisplays...) {
			if err := s.SetUISettings(bg(), l, z); err != nil {
				t.Fatalf("%s %s: %v", l, z, err)
			}
			if gl, gz, _ := s.UISettings(bg()); gl != l || gz != z {
				t.Fatalf("read back %q %q", gl, gz)
			}
		}
	}
	if err := s.SetUISettings(bg(), " zh-TW ", "tw"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{{"fr", ""}, {"zh-tw", ""}, {"zh-CN", "traditional"}, {"zh-CN", "TW"}} {
		if err := s.SetUISettings(bg(), bad[0], bad[1]); !errors.Is(err, ErrInvalid) || !containsHan(err.Error()) ||
			(codeOf(err) != CodeLocaleUnsupported && codeOf(err) != CodeZhDisplayInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if l, z, _ := s.UISettings(bg()); l != "zh-TW" || z != "tw" {
		t.Fatalf("failed writes changed %q %q", l, z)
	}
	if v, _, _ := st.GetKV(bg(), model.KVUILocale); v != "zh-TW" {
		t.Fatalf("kv %q", v)
	}
	if err := s.SetUISettings(bg(), "", ""); err != nil {
		t.Fatal(err)
	}
	if l, z, _ := s.UISettings(bg()); l != "" || z != "" {
		t.Fatalf("cleared %q %q", l, z)
	}
}

func TestHasAnyPriceAndCurrency(t *testing.T) {
	preset := DeepSeekPresetJSON()
	noModels := `{"timezone":"UTC","peak_windows":[{"days":[1],"from":"09:00","to":"10:00"}],"offpeak_multiplier":0.5}`
	mixed, _ := json.Marshal(tariff.Tariff{Timezone: "UTC", Models: map[string]tariff.ModelPrice{
		"b": {Input: 1, Currency: "USD"}, "a": {Input: 1}, "c": {Input: 1, Currency: "EUR"}}})
	mixedS := string(mixed)
	broken := `{"timezone": "Nowhere/Land", "models": {"m": {"input": 1}}}`
	for _, tc := range []struct {
		name     string
		p        model.Provider
		priced   bool
		currency string
	}{
		{"nothing", model.Provider{}, false, ""},
		{"prices", model.Provider{PriceInput: fp(1), PriceCacheHit: fp(1), PriceOutput: fp(1), Currency: sp("CNY")}, true, "CNY"},
		{"incomplete prices", model.Provider{PriceInput: fp(1)}, false, ""},
		{"preset", model.Provider{Tariff: &preset}, true, "CNY"},
		{"preset, own currency wins", model.Provider{Tariff: &preset, Currency: sp("USD")}, true, "USD"},
		{"windows only", model.Provider{Tariff: &noModels}, false, ""},
		{"first model currency by name", model.Provider{Tariff: &mixedS}, true, "USD"},
		{"invalid tariff", model.Provider{Tariff: &broken}, false, ""},
	} {
		if got := HasAnyPrice(tc.p); got != tc.priced {
			t.Errorf("%s: HasAnyPrice %v", tc.name, got)
		}
		got := ProviderCurrency(tc.p)
		if (got == nil) != (tc.currency == "") || (got != nil && *got != tc.currency) {
			t.Errorf("%s: currency %v", tc.name, got)
		}
	}
	if FallbackPrice(model.Provider{PriceInput: fp(1)}) != nil {
		t.Error("incomplete prices are no fallback")
	}
	if mp := FallbackPrice(model.Provider{PriceInput: fp(2), PriceCacheHit: fp(0.5), PriceOutput: fp(8), Currency: sp("CNY")}); mp == nil ||
		*mp != (tariff.ModelPrice{Input: 2, InputCacheHit: 0.5, Output: 8, Currency: "CNY"}) {
		t.Errorf("fallback %+v", mp)
	}
}
