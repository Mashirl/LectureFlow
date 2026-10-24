// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/tariff"
)

// v2 P12.4: the peak state of providers with a tariff in the resource bar.

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// withLocal runs the test with time.Local set to loc (the server's zone
// decides "today" for TodayPeak). Tests in this package do not run in parallel.
func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func bjTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, zone(t, "Asia/Shanghai"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func presetProvider(id string, offpeakOnly bool) model.Provider {
	raw := settings.DeepSeekPresetJSON()
	return model.Provider{ID: id, Name: id, BalanceAPI: model.BalanceNone, MaxConcurrency: 4, Tariff: &raw, OffpeakOnly: offpeakOnly}
}

func TestTariffStateOf(t *testing.T) {
	withLocal(t, zone(t, "Europe/London"))
	hol := tariff.HolidaysCN()
	p := presetProvider("ds", true)

	// Wednesday 21 October 2026, 10:30 in Beijing (03:30 in London, BST).
	ts := TariffStateOf(p, bjTime(t, "2026-10-21 10:30"), hol)
	want := &TariffState{
		Peak: true, NextChange: "2026-10-21T04:00:00Z", Timezone: "Asia/Shanghai", OffpeakOnly: true, Multiplier: 0.5,
		// The London day (23:00Z to 23:00Z): Beijing 09:00–12:00 and 14:00–18:00.
		TodayPeak: [][2]string{{"2026-10-21T01:00:00Z", "2026-10-21T04:00:00Z"}, {"2026-10-21T06:00:00Z", "2026-10-21T10:00:00Z"}},
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("got %+v\nwant %+v", ts, want)
	}

	// Saturday: off-peak all day, next change Monday 09:00 Beijing.
	ts = TariffStateOf(presetProvider("ds", false), bjTime(t, "2026-10-24 15:00"), hol)
	if ts == nil || ts.Peak || ts.NextChange != "2026-10-26T01:00:00Z" || ts.OffpeakOnly || len(ts.TodayPeak) != 0 || ts.TodayPeak == nil {
		t.Fatalf("saturday %+v", ts)
	}
	b, _ := json.Marshal(ts)
	if !strings.Contains(string(b), `"today_peak":[]`) || !strings.Contains(string(b), `"offpeak_multiplier":0.5`) {
		t.Fatalf("json %s", b)
	}

	// National Day holiday (Monday 5 October): off-peak.
	if ts = TariffStateOf(p, bjTime(t, "2026-10-05 10:00"), hol); ts.Peak || len(ts.TodayPeak) != 0 || ts.MissingHolidays {
		t.Fatalf("holiday %+v", ts)
	}
	// 2027 has no holiday data yet (D4): computed by weekday and time, flagged.
	if ts = TariffStateOf(p, bjTime(t, "2027-01-04 10:00"), hol); !ts.Peak || !ts.MissingHolidays {
		t.Fatalf("2027 %+v", ts)
	}

	// No tariff, prices only, invalid: no state.
	models := `{"timezone":"UTC","models":{"m":{"input":1,"output":1,"input_cache_hit":1}}}`
	broken := `{"timezone":"Nowhere/Land"}`
	for _, raw := range []*string{nil, &models, &broken} {
		if ts := TariffStateOf(model.Provider{ID: "x", Tariff: raw}, bjTime(t, "2026-10-21 10:30"), hol); ts != nil {
			t.Errorf("%v: %+v", raw, ts)
		}
	}
}

// The resource bar rows carry the state (refreshed with every snapshot) and
// today's accumulated estimated cost; rows without a tariff omit the field.
func TestSnapshotTariffStateAndHolidays(t *testing.T) {
	withLocal(t, zone(t, "Asia/Shanghai"))
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	clk := &fakeClock{t: bjTime(t, "2026-10-21 11:58")}
	ds := presetProvider("ds", false)
	plain := model.Provider{ID: "plain", Name: "Plain", BalanceAPI: model.BalanceNone, MaxConcurrency: 2}
	if err := st.AddUsageDaily(ctx, model.UsageDaily{ProviderID: "ds", Day: "2026-10-21", Requests: 2, PromptTokens: 1000, CompletionTokens: 10, EstimatedCost: 0.75}); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Store: st, Now: clk.Now, RunCmd: (&fakeSMI{}).Run, Interval: time.Second,
		Providers: func() []model.Provider { return []model.Provider{ds, plain} }})
	snap := s.Snapshot(ctx)
	d, p := snap.Providers[0], snap.Providers[1]
	if d.TariffState == nil || !d.TariffState.Peak || d.TariffState.NextChange != "2026-10-21T04:00:00Z" {
		t.Fatalf("ds state %+v", d.TariffState)
	}
	// The preset carries model prices (CNY): the accumulated estimate shows.
	if d.CostToday == nil || *d.CostToday != 0.75 || d.Currency == nil || *d.Currency != "CNY" {
		t.Fatalf("ds cost %v %v", d.CostToday, d.Currency)
	}
	if p.TariffState != nil || p.CostToday != nil || p.Currency != nil {
		t.Fatalf("plain %+v", p)
	}
	b, _ := json.Marshal(snap)
	if strings.Count(string(b), `"tariff_state"`) != 1 {
		t.Fatalf("tariff_state must be omitted without a tariff: %s", b)
	}

	// Three minutes later the peak has ended.
	clk.Advance(3 * time.Minute)
	if ts := s.Snapshot(ctx).Providers[0].TariffState; ts.Peak || ts.NextChange != "2026-10-21T06:00:00Z" {
		t.Fatalf("after 12:00 %+v", ts)
	}

	// User-edited holidays (kv tariff.holidays_cn) apply without Deps.Holidays.
	clk.Advance(-3 * time.Minute)
	if err := st.SetKV(ctx, model.KVHolidaysCN, `{"2026":["2026-10-21"]}`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second) // a new sample
	if ts := s.Snapshot(ctx).Providers[0].TariffState; ts.Peak {
		t.Fatalf("edited holiday ignored %+v", ts)
	}
	// Deps.Holidays wins over the kv data.
	s2 := New(Deps{Store: st, Now: clk.Now, RunCmd: (&fakeSMI{}).Run,
		Providers: func() []model.Provider { return []model.Provider{ds} },
		Holidays:  tariff.HolidaysCN})
	if ts := s2.Snapshot(ctx).Providers[0].TariffState; !ts.Peak {
		t.Fatalf("Deps.Holidays ignored %+v", ts)
	}
}

// D3 (lead ruling at the phase-2 gate): a provider row says how many of
// today's requests had no price (cost_unpriced_requests, omitted at 0); the
// amount is null when every request of the day was unpriced, and for a
// provider without any price all its requests count as unpriced.
func TestSnapshotUnpricedRequests(t *testing.T) {
	withLocal(t, zone(t, "Asia/Shanghai"))
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	clk := &fakeClock{t: bjTime(t, "2026-10-21 13:00")}
	day := "2026-10-21"
	part := presetProvider("part", false) // tariff model prices only
	part.Name = "Part"
	all := presetProvider("all", false)
	all.Name = "All"
	plain := model.Provider{ID: "plain", Name: "Plain", BalanceAPI: model.BalanceNone, MaxConcurrency: 2}
	for _, u := range []model.UsageDaily{
		{ProviderID: "part", Day: day, Requests: 3, PromptTokens: 3000, EstimatedCost: 0.5},
		{ProviderID: "all", Day: day, Requests: 2, PromptTokens: 2000},
		{ProviderID: "plain", Day: day, Requests: 4, PromptTokens: 4000},
		{ProviderID: "all", Day: "2026-10-20", Requests: 1, PromptTokens: 10, EstimatedCost: 0.1}, // yesterday
	} {
		if err := st.AddUsageDaily(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for id, n := range map[string]int{"part": 1, "all": 2} {
		for i := 0; i < n; i++ {
			if err := settings.AddUnpricedRequest(ctx, st, id, day); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := New(Deps{Store: st, Now: clk.Now, RunCmd: (&fakeSMI{}).Run, Interval: time.Second,
		Providers: func() []model.Provider { return []model.Provider{part, all, plain} }})
	snap := s.Snapshot(ctx)
	byID := map[string]Provider{}
	for _, p := range snap.Providers {
		byID[p.ID] = p
	}
	if p := byID["part"]; p.CostToday == nil || *p.CostToday != 0.5 || p.CostUnpricedRequests != 1 {
		t.Errorf("part: cost %v unpriced %d, want 0.5 and 1", p.CostToday, p.CostUnpricedRequests)
	}
	if p := byID["all"]; p.CostToday != nil || p.CostUnpricedRequests != 2 {
		t.Errorf("all: cost %v unpriced %d, want null and 2", p.CostToday, p.CostUnpricedRequests)
	}
	if p := byID["plain"]; p.CostToday != nil || p.CostUnpricedRequests != 4 {
		t.Errorf("plain: cost %v unpriced %d, want null and 4", p.CostToday, p.CostUnpricedRequests)
	}
	b, _ := json.Marshal(snap)
	if strings.Count(string(b), `"cost_unpriced_requests"`) != 3 {
		t.Errorf("cost_unpriced_requests: %s", b)
	}
	b, _ = json.Marshal(Provider{ID: "x"})
	if strings.Contains(string(b), "cost_unpriced_requests") {
		t.Errorf("omitted at 0: %s", b)
	}
}
