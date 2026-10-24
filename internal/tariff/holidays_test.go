// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The embedded data is the 2026 schedule of the State Council's notice (days
// off only), and nothing for 2027, whose schedule is not published yet (D4).
func TestHolidaysCN(t *testing.T) {
	h := HolidaysCN()
	if got := h.Years(); !reflect.DeepEqual(got, []int{2026}) {
		t.Fatalf("Years() = %v, want [2026]", got)
	}
	want := []string{
		"2026-01-01", "2026-01-02", "2026-01-03", // New Year's Day
		"2026-02-15", "2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19", // Spring Festival
		"2026-02-20", "2026-02-21", "2026-02-22", "2026-02-23",
		"2026-04-04", "2026-04-05", "2026-04-06", // Qingming
		"2026-05-01", "2026-05-02", "2026-05-03", "2026-05-04", "2026-05-05", // Labour Day
		"2026-06-19", "2026-06-20", "2026-06-21", // Dragon Boat
		"2026-09-25", "2026-09-26", "2026-09-27", // Mid-Autumn
		"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", // National Day
		"2026-10-06", "2026-10-07",
	}
	if got := h.Dates(2026); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dates(2026) = %v\nwant %v", got, want)
	}
	for _, d := range want {
		if !h.IsHoliday(d) {
			t.Errorf("IsHoliday(%s) = false", d)
		}
	}
	// Weekend days worked in exchange are not holidays (and stay weekends).
	for _, d := range []string{"2026-01-04", "2026-02-14", "2026-02-28", "2026-05-09", "2026-09-20", "2026-10-10",
		"2026-10-08", "2026-12-31", "2027-01-01", "2026-2-17", ""} {
		if h.IsHoliday(d) {
			t.Errorf("IsHoliday(%q) = true", d)
		}
	}
	for y, want := range map[int]bool{2025: false, 2026: true, 2027: false} {
		if h.Has(y) != want {
			t.Errorf("Has(%d) = %v", y, !want)
		}
	}
	if h.Dates(2027) != nil {
		t.Errorf("Dates(2027) = %v", h.Dates(2027))
	}
}

func TestParseHolidays(t *testing.T) {
	h, err := ParseHolidays([]byte(`{
		"_source": "test", "_note": {"any": ["thing"]},
		"2027": [],
		"2026": ["2026-10-21", "2026-10-01", "2026-10-21"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Years(); !reflect.DeepEqual(got, []int{2026, 2027}) {
		t.Errorf("Years() = %v", got)
	}
	// An empty list is data: that year has no holidays.
	if !h.Has(2027) || h.Dates(2027) != nil {
		t.Errorf("2027: Has = %v, Dates = %v", h.Has(2027), h.Dates(2027))
	}
	if got := h.Dates(2026); !reflect.DeepEqual(got, []string{"2026-10-01", "2026-10-21"}) {
		t.Errorf("Dates(2026) = %v", got)
	}
	if empty, err := ParseHolidays([]byte(`{}`)); err != nil || len(empty.Years()) != 0 {
		t.Errorf("ParseHolidays({}) = %v, %v", empty.Years(), err)
	}
	var zero HolidaySet
	if zero.Has(2026) || zero.IsHoliday("2026-10-01") || len(zero.Years()) != 0 || zero.Dates(2026) != nil {
		t.Error("zero HolidaySet is not empty")
	}
	for _, raw := range []string{
		``, `null`, `[]`, `"2026"`,
		`{"26": []}`, `{"02026": []}`, `{"abcd": []}`, `{"0999": []}`,
		`{"2026": null}`, `{"2026": "2026-10-01"}`, `{"2026": [20261001]}`,
		`{"2026": ["2026-10-1"]}`, `{"2026": ["2026-02-30"]}`, `{"2026": ["2025-10-01"]}`,
		`{"2026": ["2026-10-01 "]}`, `{"2026": ["2026/10/01"]}`,
	} {
		got, err := ParseHolidays([]byte(raw))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalidHolidayData || len(got.Years()) != 0 {
			t.Errorf("ParseHolidays(%s) = %v, %v; want %s", raw, got.Years(), err, CodeInvalidHolidayData)
		}
	}
}

func TestHolidaysCNSource(t *testing.T) {
	if src := HolidaysCNSource(); !strings.Contains(src, "2026") || !strings.Contains(src, "国务院办公厅") {
		t.Fatalf("source %q", src)
	}
}
