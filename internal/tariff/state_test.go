// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// stateCase: at `at` (in loc) the state is peak and next changes at `next`
// (in loc; "" = never).
type stateCase struct {
	name string
	at   string
	peak bool
	next string
}

func checkStates(t *testing.T, loc *time.Location, cfg Tariff, hol HolidaySet, cases []stateCase) {
	t.Helper()
	for _, c := range cases {
		peak, next := State(at(loc, c.at), cfg, hol)
		if peak != c.peak {
			t.Errorf("%s: State(%s) peak = %v, want %v", c.name, c.at, peak, c.peak)
		}
		if c.next == "" {
			if !next.IsZero() {
				t.Errorf("%s: State(%s) next = %v, want none", c.name, c.at, next)
			}
			continue
		}
		if want := at(loc, c.next); !next.Equal(want) {
			t.Errorf("%s: State(%s) next = %v, want %v", c.name, c.at, next.In(loc), want)
		}
	}
}

// P12.3 golden: weekday boundaries in Beijing time (Mon 2026-10-19 … Sun
// 2026-10-25, 2026 data present without holidays that week).
func TestStateWeekdayBoundaries(t *testing.T) {
	hol := holidays(t, `{"2026": []}`)
	checkStates(t, shanghai, DeepSeekPreset(), hol, []stateCase{
		{"midnight", "2026-10-19 00:00", false, "2026-10-19 09:00"},
		{"before 09:00", "2026-10-19 08:59:59.999999999", false, "2026-10-19 09:00"},
		{"09:00 sharp", "2026-10-19 09:00", true, "2026-10-19 12:00"},
		{"11:59", "2026-10-19 11:59", true, "2026-10-19 12:00"},
		{"last ns before noon", "2026-10-19 11:59:59.999999999", true, "2026-10-19 12:00"},
		{"12:00 sharp", "2026-10-19 12:00", false, "2026-10-19 14:00"},
		{"13:59", "2026-10-19 13:59", false, "2026-10-19 14:00"},
		{"14:00 sharp", "2026-10-19 14:00", true, "2026-10-19 18:00"},
		{"17:59:59", "2026-10-19 17:59:59", true, "2026-10-19 18:00"},
		{"18:00 sharp", "2026-10-19 18:00", false, "2026-10-20 09:00"},
		{"23:59", "2026-10-19 23:59", false, "2026-10-20 09:00"},
		{"Friday evening", "2026-10-23 18:00", false, "2026-10-26 09:00"},
		{"Saturday in window hours", "2026-10-24 10:00", false, "2026-10-26 09:00"},
		{"Sunday afternoon", "2026-10-25 15:00", false, "2026-10-26 09:00"},
		{"Sunday last second", "2026-10-25 23:59:59", false, "2026-10-26 09:00"},
		{"Monday after the weekend", "2026-10-26 09:00", true, "2026-10-26 12:00"},
	})
}

// P12.3 golden: public holidays, with test-only holiday sets.
func TestStateHolidays(t *testing.T) {
	cfg := DeepSeekPreset()
	checkStates(t, shanghai, cfg, holidays(t, `{"2026": ["2026-10-21"]}`), []stateCase{
		{"evening before a midweek holiday", "2026-10-20 18:00", false, "2026-10-22 09:00"},
		{"holiday in window hours", "2026-10-21 10:00", false, "2026-10-22 09:00"},
		{"holiday afternoon", "2026-10-21 15:00", false, "2026-10-22 09:00"},
		{"day after", "2026-10-22 09:00", true, "2026-10-22 12:00"},
	})
	national := holidays(t, `{"2026": ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04",
		"2026-10-05", "2026-10-06", "2026-10-07"]}`)
	checkStates(t, shanghai, cfg, national, []stateCase{
		{"last peak before the holiday", "2026-09-30 17:00", true, "2026-09-30 18:00"},
		{"holiday runs into a week", "2026-09-30 18:00", false, "2026-10-08 09:00"},
		{"inside the holiday", "2026-10-01 10:00", false, "2026-10-08 09:00"},
		// A Saturday worked in exchange is still a weekend (P12.2: literal).
		{"working Saturday", "2026-10-10 10:00", false, "2026-10-12 09:00"},
	})
	// Without exclude_holidays the same set is ignored.
	plain := cfg
	plain.ExcludeHolidays = ""
	checkStates(t, shanghai, plain, national, []stateCase{
		{"holidays not excluded", "2026-10-01 10:00", true, "2026-10-01 12:00"},
		{"weekday after the evening", "2026-09-30 18:00", false, "2026-10-01 09:00"},
	})
}

// P12.3 golden: the embedded holidays_cn.json.
func TestStateEmbeddedHolidays(t *testing.T) {
	checkStates(t, shanghai, DeepSeekPreset(), HolidaysCN(), []stateCase{
		// Spring Festival 2026-02-15 … 02-23; 02-14 and 02-28 are worked Saturdays.
		{"Friday before Spring Festival", "2026-02-13 18:00", false, "2026-02-24 09:00"},
		{"Spring Festival weekday", "2026-02-20 10:00", false, "2026-02-24 09:00"},
		{"worked Saturday after Spring Festival", "2026-02-28 10:00", false, "2026-03-02 09:00"},
		{"Qingming Monday", "2026-04-06 10:00", false, "2026-04-07 09:00"},
		{"Thursday before Dragon Boat", "2026-06-18 18:00", false, "2026-06-22 09:00"},
		// 2027 has no data yet: New Year's Day counts as a plain Friday.
		{"into a year without data", "2026-12-31 18:00", false, "2027-01-01 09:00"},
		{"year without data", "2027-01-01 10:00", true, "2027-01-01 12:00"},
	})
}

// P12.3 golden: missing holiday data falls back to weekday and time.
func TestStateMissingHolidayData(t *testing.T) {
	cfg := DeepSeekPreset()
	only2025 := holidays(t, `{"2025": ["2025-10-01"]}`)
	checkStates(t, shanghai, cfg, only2025, []stateCase{
		{"2026 holiday unknown", "2026-10-01 10:00", true, "2026-10-01 12:00"},
	})
	checkStates(t, shanghai, cfg, HolidaySet{}, []stateCase{
		{"zero set", "2026-10-01 10:00", true, "2026-10-01 12:00"},
	})

	cn := HolidaysCN()
	for _, c := range []struct {
		name string
		t    time.Time
		cfg  Tariff
		hol  HolidaySet
		want bool
	}{
		{"2026 present", at(shanghai, "2026-10-23 10:00"), cfg, cn, false},
		{"2027 missing", at(shanghai, "2027-01-01 00:30"), cfg, cn, true},
		// The year is the tariff's: 2026-12-31 17:00 UTC is 2027 in Beijing.
		{"year in tariff zone", at(time.UTC, "2026-12-31 17:00"), cfg, cn, true},
		{"still 2026 in Beijing", at(time.UTC, "2026-12-31 15:59"), cfg, cn, false},
		{"zero set", at(shanghai, "2026-10-23 10:00"), cfg, HolidaySet{}, true},
		{"holidays not excluded", at(shanghai, "2027-01-01 00:30"), Tariff{Timezone: "Asia/Shanghai",
			PeakWindows: cfg.PeakWindows, OffpeakMultiplier: 0.5}, cn, false},
	} {
		if got := MissingHolidayData(c.t, c.cfg, c.hol); got != c.want {
			t.Errorf("%s: MissingHolidayData = %v, want %v", c.name, got, c.want)
		}
	}
}

// P12.3 golden: the user is in London; Beijing has no DST, London switches on
// 2026-03-29 and 2026-10-25, so the same peak moves by an hour locally.
func TestStateLondonDST(t *testing.T) {
	hol := holidays(t, `{"2026": []}`)
	checkStates(t, london, DeepSeekPreset(), hol, []stateCase{
		{"Monday peak in BST", "2026-10-19 02:00", true, "2026-10-19 05:00"},
		{"Friday before the spring change", "2026-03-27 18:00", false, "2026-03-30 02:00"},
		{"Sunday of the spring change", "2026-03-29 12:00", false, "2026-03-30 02:00"},
		{"Friday before the autumn change", "2026-10-23 12:00", false, "2026-10-26 01:00"},
		{"Monday after the autumn change", "2026-10-26 00:30", false, "2026-10-26 01:00"},
		{"first GMT peak", "2026-10-26 01:00", true, "2026-10-26 04:00"},
	})
	// nextChange comes back in the caller's zone.
	_, next := State(at(london, "2026-03-27 18:00"), DeepSeekPreset(), hol)
	if next.Location() != london || next.Format("2006-01-02 15:04 MST") != "2026-03-30 02:00 BST" {
		t.Errorf("next = %s in %v, want 2026-03-30 02:00 BST", next.Format("2006-01-02 15:04 MST"), next.Location())
	}
	if bj := next.In(shanghai).Format("15:04"); bj != "09:00" {
		t.Errorf("next in Beijing = %s, want 09:00", bj)
	}
	// The same instant gives the same answer whatever zone it is given in.
	for _, loc := range []*time.Location{time.UTC, shanghai, london, havana} {
		x := at(london, "2026-10-23 12:00").In(loc)
		peak, next := State(x, DeepSeekPreset(), hol)
		if peak || !next.Equal(at(london, "2026-10-26 01:00")) {
			t.Errorf("in %v: State = %v, %v", loc, peak, next)
		}
	}
}

// A tariff in a DST zone: the wall clock jumps over or repeats a window.
func TestStateTariffZoneDST(t *testing.T) {
	cfg := Tariff{Timezone: "Europe/London", OffpeakMultiplier: 0.5,
		PeakWindows: []Window{{Days: []int{7}, From: "01:30", To: "02:30"}}}
	checkStates(t, time.UTC, cfg, HolidaySet{}, []stateCase{
		// 2026-03-29 01:00 UTC: 01:00 GMT → 02:00 BST, inside the window.
		{"spring: before the jump", "2026-03-29 00:30", false, "2026-03-29 01:00"},
		{"spring: jump lands in the window", "2026-03-29 01:00", true, "2026-03-29 01:30"},
		{"spring: next Sunday", "2026-03-29 01:30", false, "2026-04-05 00:30"},
		// 2026-10-25 01:00 UTC: 02:00 BST → 01:00 GMT, 01:30 happens twice.
		{"autumn: 01:00 BST", "2026-10-25 00:00", false, "2026-10-25 00:30"},
		{"autumn: first 01:30", "2026-10-25 00:30", true, "2026-10-25 01:00"},
		{"autumn: clock goes back to 01:00", "2026-10-25 01:00", false, "2026-10-25 01:30"},
		{"autumn: second 01:30", "2026-10-25 01:30", true, "2026-10-25 02:30"},
		{"autumn: next Sunday", "2026-10-25 02:30", false, "2026-11-01 01:30"},
	})
	// A window entirely inside the gap does not happen that day.
	gap := Tariff{Timezone: "Europe/London", OffpeakMultiplier: 0.5,
		PeakWindows: []Window{{Days: []int{7}, From: "01:15", To: "01:45"}}}
	checkStates(t, time.UTC, gap, HolidaySet{}, []stateCase{
		{"window skipped by the gap", "2026-03-22 01:45", false, "2026-04-05 00:15"},
	})
}

func TestStateWindowShapes(t *testing.T) {
	hol := holidays(t, `{"2026": ["2026-10-21"]}`)
	mon := []int{1}
	cases := []struct {
		name string
		cfg  Tariff
		hol  HolidaySet
		at   string
		peak bool
		next string
	}{
		{"no windows", Tariff{Timezone: "Asia/Shanghai"}, hol, "2026-10-19 10:00", false, ""},
		{"adjacent windows merge", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, PeakWindows: []Window{
			{Days: mon, From: "09:00", To: "12:00"}, {Days: mon, From: "12:00", To: "14:00"}}},
			hol, "2026-10-19 10:00", true, "2026-10-19 14:00"},
		{"overlapping windows", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, PeakWindows: []Window{
			{Days: mon, From: "09:00", To: "12:00"}, {Days: mon, From: "11:00", To: "13:00"}}},
			hol, "2026-10-19 11:30", true, "2026-10-19 13:00"},
		{"across midnight", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, PeakWindows: []Window{
			{Days: mon, From: "22:00", To: "24:00"}, {Days: []int{2}, From: "00:00", To: "02:00"}}},
			hol, "2026-10-19 23:00", true, "2026-10-20 02:00"},
		{"around the clock never changes", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, PeakWindows: []Window{
			{Days: []int{1, 2, 3, 4, 5, 6, 7}, From: "00:00", To: "24:00"}}},
			HolidaySet{}, "2026-10-19 10:00", true, ""},
		{"around the clock until a holiday", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, ExcludeHolidays: "CN",
			PeakWindows: []Window{{Days: []int{1, 2, 3, 4, 5, 6, 7}, From: "00:00", To: "24:00"}}},
			hol, "2026-10-20 10:00", true, "2026-10-21 00:00"},
		{"around the clock after a holiday", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, ExcludeHolidays: "CN",
			PeakWindows: []Window{{Days: []int{1, 2, 3, 4, 5, 6, 7}, From: "00:00", To: "24:00"}}},
			hol, "2026-10-21 05:00", false, "2026-10-22 00:00"},
		{"after the last holiday year", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, ExcludeHolidays: "CN",
			PeakWindows: []Window{{Days: []int{1, 2, 3, 4, 5, 6, 7}, From: "00:00", To: "24:00"}}},
			hol, "2027-03-01 05:00", true, ""},
		{"weekly window across holiday weeks", Tariff{Timezone: "Asia/Shanghai", OffpeakMultiplier: 0.5, ExcludeHolidays: "CN",
			PeakWindows: []Window{{Days: mon, From: "09:00", To: "09:01"}}},
			holidays(t, `{"2026": ["2026-10-26", "2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23"]}`),
			"2026-10-19 09:01", false, "2026-11-30 09:00"},
	}
	for _, c := range cases {
		checkStates(t, shanghai, c.cfg, c.hol, []stateCase{{c.name, c.at, c.peak, c.next}})
	}
}

// Exactness: for every minute of several weeks, nextChange is the first later
// minute at which the brute-force reading differs (all zones here have whole
// minute offsets, so every change happens on a UTC minute).
func TestStateMatchesBruteForce(t *testing.T) {
	sunday := Tariff{Timezone: "Europe/London", OffpeakMultiplier: 0.5, PeakWindows: []Window{
		{Days: []int{7}, From: "01:30", To: "02:30"}, {Days: []int{6, 7}, From: "00:00", To: "00:45"},
		{Days: []int{1}, From: "01:15", To: "01:45"}}}
	havanaTariff := Tariff{Timezone: "America/Havana", OffpeakMultiplier: 0.5, PeakWindows: []Window{
		{Days: []int{6}, From: "23:00", To: "24:00"}, {Days: []int{7}, From: "00:00", To: "00:30"},
		{Days: []int{7}, From: "00:45", To: "02:00"}}}
	for _, c := range []struct {
		name     string
		cfg      Tariff
		hol      HolidaySet
		from, to string // UTC
	}{
		{"preset around National Day", DeepSeekPreset(), HolidaysCN(), "2026-09-24 00:00", "2026-10-16 00:00"},
		{"preset around Spring Festival", DeepSeekPreset(), HolidaysCN(), "2026-02-10 00:00", "2026-03-04 00:00"},
		{"preset into 2027", DeepSeekPreset(), HolidaysCN(), "2026-12-25 00:00", "2027-01-08 00:00"},
		{"London spring", sunday, HolidaySet{}, "2026-03-20 00:00", "2026-04-10 00:00"},
		{"London autumn", sunday, HolidaySet{}, "2026-10-16 00:00", "2026-11-06 00:00"},
		{"Havana spring (midnight gap)", havanaTariff, HolidaySet{}, "2026-03-01 00:00", "2026-03-20 00:00"},
		{"Havana autumn (midnight repeat)", havanaTariff, HolidaySet{}, "2026-10-25 00:00", "2026-11-12 00:00"},
	} {
		loc := mustZone(c.cfg.Timezone)
		start, end := at(time.UTC, c.from), at(time.UTC, c.to)
		n := int(end.Sub(start) / time.Minute)
		ref := make([]bool, n+1)
		for i := range ref {
			ref[i] = refPeak(start.Add(time.Duration(i)*time.Minute), loc, c.cfg, c.hol)
		}
		// nextRef[i] = first j > i with ref[j] != ref[i], or -1 past the end.
		nextRef := make([]int, n+1)
		nextRef[n] = -1
		for i := n - 1; i >= 0; i-- {
			if ref[i+1] != ref[i] {
				nextRef[i] = i + 1
			} else {
				nextRef[i] = nextRef[i+1]
			}
		}
		bad := 0
		for i := 0; i < n && bad < 5; i++ {
			x := start.Add(time.Duration(i) * time.Minute)
			peak, next := State(x, c.cfg, c.hol)
			ok := peak == ref[i]
			if nextRef[i] >= 0 {
				ok = ok && next.Equal(start.Add(time.Duration(nextRef[i])*time.Minute))
			} else {
				ok = ok && (next.IsZero() || !next.Before(end))
			}
			if !ok {
				bad++
				t.Errorf("%s: State(%s) = %v, %s; brute force %v, +%d min", c.name,
					x.Format(time.RFC3339), peak, next.Format(time.RFC3339), ref[i], nextRef[i]-i)
			}
		}
	}
}

// intervals formats PeakIntervals as "15:04–15:04" in loc; an end on the next
// local day prints as 24:00.
func intervals(ivs [][2]time.Time, loc *time.Location) string {
	var parts []string
	for _, iv := range ivs {
		s, e := iv[0].In(loc), iv[1].In(loc)
		end := e.Format("15:04")
		if e.Day() != s.Day() {
			end = "24:00"
		}
		parts = append(parts, s.Format("15:04")+"–"+end)
	}
	return strings.Join(parts, "、")
}

func TestPeakIntervals(t *testing.T) {
	cfg := DeepSeekPreset()
	hol := holidays(t, `{"2026": []}`)
	for _, c := range []struct {
		name string
		loc  *time.Location
		t    time.Time
		want string
	}{
		{"London Friday, BST", london, at(london, "2026-10-23 12:00"), "02:00–05:00、07:00–11:00"},
		{"London Saturday", london, at(london, "2026-10-24 12:00"), ""},
		{"London Sunday, 25 hours", london, at(london, "2026-10-25 12:00"), ""},
		{"London Monday, GMT (P12.4 example)", london, at(london, "2026-10-26 12:00"), "01:00–04:00、06:00–10:00"},
		{"London Friday before spring change", london, at(london, "2026-03-27 23:59"), "01:00–04:00、06:00–10:00"},
		{"London Sunday, 23 hours", london, at(london, "2026-03-29 12:00"), ""},
		{"London Monday after spring change", london, at(london, "2026-03-30 00:00"), "02:00–05:00、07:00–11:00"},
		// t is given in Beijing time; the day is London's.
		{"t in another zone", london, at(shanghai, "2026-10-26 08:30"), "01:00–04:00、06:00–10:00"},
		{"Beijing", shanghai, at(shanghai, "2026-10-19 20:00"), "09:00–12:00、14:00–18:00"},
		// UTC-2: Beijing 09:00–12:00 is 23:00–02:00 the evening before.
		{"clipped: Sunday evening", utcMinus2, at(utcMinus2, "2026-10-25 12:00"), "23:00–24:00"},
		{"clipped: Monday", utcMinus2, at(utcMinus2, "2026-10-26 12:00"), "00:00–02:00、04:00–08:00、23:00–24:00"},
		{"clipped: Friday", utcMinus2, at(utcMinus2, "2026-10-23 12:00"), "00:00–02:00、04:00–08:00"},
		{"nil loc is UTC", nil, at(time.UTC, "2026-10-19 12:00"), "01:00–04:00、06:00–10:00"},
	} {
		loc := c.loc
		if loc == nil {
			loc = time.UTC
		}
		ivs := PeakIntervals(c.t, c.loc, cfg, hol)
		if got := intervals(ivs, loc); got != c.want {
			t.Errorf("%s: PeakIntervals = %q, want %q", c.name, got, c.want)
		}
		for _, iv := range ivs {
			if iv[0].Location() != loc || iv[1].Location() != loc {
				t.Errorf("%s: interval not in %v", c.name, loc)
			}
		}
	}
	// Exact instants on the clipped Sunday: [23:00, next day 00:00) local.
	ivs := PeakIntervals(at(utcMinus2, "2026-10-25 12:00"), utcMinus2, cfg, hol)
	if len(ivs) != 1 || !ivs[0][0].Equal(at(time.UTC, "2026-10-26 01:00")) || !ivs[0][1].Equal(at(time.UTC, "2026-10-26 02:00")) {
		t.Errorf("clipped Sunday = %v", ivs)
	}
	if ivs := PeakIntervals(at(shanghai, "2026-10-19 12:00"), shanghai, Tariff{Timezone: "Asia/Shanghai"}, hol); len(ivs) != 0 {
		t.Errorf("no windows: %v", ivs)
	}
	if ivs := PeakIntervals(at(shanghai, "2026-10-01 12:00"), shanghai, cfg, HolidaysCN()); len(ivs) != 0 {
		t.Errorf("National Day: %v", ivs)
	}
}

// The tariff's own DST: on 2026-10-25 London's 01:30–02:30 happens twice.
func TestPeakIntervalsTariffZoneDST(t *testing.T) {
	cfg := Tariff{Timezone: "Europe/London", OffpeakMultiplier: 0.5,
		PeakWindows: []Window{{Days: []int{7}, From: "01:30", To: "02:30"}}}
	ivs := PeakIntervals(at(london, "2026-10-25 12:00"), london, cfg, HolidaySet{})
	want := [][2]string{{"2026-10-25 00:30", "2026-10-25 01:00"}, {"2026-10-25 01:30", "2026-10-25 02:30"}}
	if len(ivs) != len(want) {
		t.Fatalf("intervals = %v", ivs)
	}
	for i, w := range want {
		if !ivs[i][0].Equal(at(time.UTC, w[0])) || !ivs[i][1].Equal(at(time.UTC, w[1])) {
			t.Errorf("interval %d = %v, want %v UTC", i, ivs[i], w)
		}
	}
	ivs = PeakIntervals(at(london, "2026-03-29 12:00"), london, cfg, HolidaySet{})
	if len(ivs) != 1 || !ivs[0][0].Equal(at(time.UTC, "2026-03-29 01:00")) || !ivs[0][1].Equal(at(time.UTC, "2026-03-29 01:30")) {
		t.Errorf("spring intervals = %v", ivs)
	}
}

// Havana's clocks go from 00:00 to 01:00 on 2026-03-08: that local day starts
// at 01:00, not at 23:00 the evening before.
func TestPeakIntervalsMidnightGap(t *testing.T) {
	if got := startOfDay(2026, 3, 8, havana); !got.Equal(at(time.UTC, "2026-03-08 05:00")) {
		t.Fatalf("startOfDay = %v, want 05:00 UTC", got)
	}
	if got := startOfDay(2026, 3, 32, havana); !got.Equal(at(time.UTC, "2026-04-01 04:00")) {
		t.Fatalf("startOfDay(Mar 32) = %v, want Apr 1 04:00 UTC", got)
	}
	cfg := Tariff{Timezone: "UTC", OffpeakMultiplier: 0.5,
		PeakWindows: []Window{{Days: []int{7}, From: "00:00", To: "24:00"}}}
	ivs := PeakIntervals(at(havana, "2026-03-08 12:00"), havana, cfg, HolidaySet{})
	if len(ivs) != 1 || !ivs[0][0].Equal(at(time.UTC, "2026-03-08 05:00")) || !ivs[0][1].Equal(at(time.UTC, "2026-03-09 00:00")) {
		t.Errorf("Havana intervals = %v", ivs)
	}
	if got := intervals(ivs, havana); got != "01:00–20:00" {
		t.Errorf("Havana intervals = %q", got)
	}
}

// Every local day's intervals equal the runs of the brute-force reading.
func TestPeakIntervalsMatchBruteForce(t *testing.T) {
	cfg := DeepSeekPreset()
	hol := HolidaysCN()
	bj := mustZone(cfg.Timezone)
	for _, loc := range []*time.Location{london, utcMinus2, havana, mustZone("America/New_York"), mustZone("Asia/Kathmandu")} {
		for _, day := range []string{"2026-03-06", "2026-03-07", "2026-03-08", "2026-03-09", "2026-03-27",
			"2026-03-28", "2026-03-29", "2026-03-30", "2026-09-30", "2026-10-01", "2026-10-08", "2026-10-23",
			"2026-10-25", "2026-10-26", "2026-11-01", "2026-11-02"} {
			noon := at(loc, day+" 12:00")
			y, m, d := noon.Date()
			start, end := startOfDay(y, m, d, loc), startOfDay(y, m, d+1, loc)
			var want []string
			var runStart time.Time
			in := false
			for x := start; x.Before(end); x = x.Add(time.Minute) {
				p := refPeak(x, bj, cfg, hol)
				if p && !in {
					runStart = x
				}
				if !p && in {
					want = append(want, runStart.UTC().Format("01-02 15:04")+"/"+x.UTC().Format("01-02 15:04"))
				}
				in = p
			}
			if in {
				want = append(want, runStart.UTC().Format("01-02 15:04")+"/"+end.UTC().Format("01-02 15:04"))
			}
			var got []string
			for _, iv := range PeakIntervals(noon, loc, cfg, hol) {
				got = append(got, iv[0].UTC().Format("01-02 15:04")+"/"+iv[1].UTC().Format("01-02 15:04"))
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("%v %s: got %v, want %v", loc, day, got, want)
			}
		}
	}
}

// P12.3 golden sweep for a user in London with the real holiday data: each
// local day's peak schedule and, at local midnight, when the next change
// comes (local time first, then Beijing time, as P12.4 shows them).
func TestLondonGolden(t *testing.T) {
	cfg, hol := DeepSeekPreset(), HolidaysCN()
	var b strings.Builder
	b.WriteString("# DeepSeek preset, holidays_cn.json, user in Europe/London.\n")
	b.WriteString("# day  local peak intervals  |  at local 00:00: state, next change (local / Beijing)\n")
	for _, r := range [][2]string{{"2026-03-23", "2026-04-07"}, {"2026-09-28", "2026-10-11"}, {"2026-10-19", "2026-11-01"}} {
		for day := at(london, r[0]+" 12:00"); !day.After(at(london, r[1]+" 12:00")); day = day.AddDate(0, 0, 1) {
			y, m, d := day.Date()
			mid := startOfDay(y, m, d, london)
			sched := intervals(PeakIntervals(day, london, cfg, hol), london)
			if sched == "" {
				sched = "全天空闲"
			}
			peak, next := State(mid, cfg, hol)
			state := "空闲"
			if peak {
				state = "高峰"
			}
			fmt.Fprintf(&b, "%s %s %s  %s  |  00:00 %s → %s / %s\n", day.Format("2006-01-02"), day.Format("Mon"),
				mid.Format("MST"), sched, state, next.In(london).Format("01-02 15:04 MST"), next.In(shanghai).Format("01-02 15:04"))
		}
	}
	golden(t, "london.golden", b.String())
}
