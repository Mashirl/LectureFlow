// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// holidaysCNJSON is mainland China's public holidays as published by the
// General Office of the State Council (appendix D4: only years whose schedule
// has been published; see its "_source").
//
//go:embed holidays_cn.json
var holidaysCNJSON []byte

var holidaysCN = sync.OnceValue(func() HolidaySet {
	h, err := ParseHolidays(holidaysCNJSON)
	if err != nil {
		panic("tariff: embedded holidays_cn.json: " + err.Error())
	}
	return h
})

// HolidaySet is a set of public-holiday dates by year. A year either has data
// (Has) or not; a year without data is computed by weekday and time only
// (P12.2), and MissingHolidayData lets the UI say so. The zero value has no
// years. A HolidaySet is immutable and safe for concurrent use.
type HolidaySet struct {
	years map[int]bool
	dates map[string]bool // "YYYY-MM-DD"
}

// HolidaysCN returns the embedded internal/tariff/holidays_cn.json.
func HolidaysCN() HolidaySet { return holidaysCN() }

var holidaysCNSource = sync.OnceValue(func() string {
	var top struct {
		Source string `json:"_source"`
	}
	_ = json.Unmarshal(holidaysCNJSON, &top)
	return top.Source
})

// HolidaysCNSource is the "_source" note of the embedded holidays_cn.json
// (where the dates come from), for the settings page.
func HolidaysCNSource() string { return holidaysCNSource() }

// ParseHolidays parses the holidays_cn.json format: an object whose keys are
// four-digit years, each mapped to that year's holiday dates ("YYYY-MM-DD",
// days off only, never the weekend days worked in exchange). Keys starting
// with "_" are comments ("_source"). Errors are *Error.
func ParseHolidays(raw []byte) (HolidaySet, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return HolidaySet{}, invalid(CodeInvalidHolidayData, nil, "holiday data must be a JSON object of years")
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // deterministic first error
	h := HolidaySet{years: map[int]bool{}, dates: map[string]bool{}}
	for _, key := range keys {
		year, err := strconv.Atoi(key)
		if err != nil || len(key) != 4 || year < 1000 {
			return HolidaySet{}, invalid(CodeInvalidHolidayData, map[string]any{"year": key},
				"%q is not a four-digit year", key)
		}
		var dates []string
		val := top[key]
		if err := json.Unmarshal(val, &dates); err != nil || bytes.Equal(bytes.TrimSpace(val), []byte("null")) {
			return HolidaySet{}, invalid(CodeInvalidHolidayData, map[string]any{"year": key},
				"%s: want a list of dates", key)
		}
		h.years[year] = true
		for _, d := range dates {
			t, err := time.Parse(time.DateOnly, d)
			if err != nil || t.Format(time.DateOnly) != d || t.Year() != year {
				return HolidaySet{}, invalid(CodeInvalidHolidayData, map[string]any{"year": key, "date": d},
					"%s: %q is not a date YYYY-MM-DD in that year", key, d)
			}
			h.dates[d] = true
		}
	}
	return h, nil
}

// Has reports whether the set has data for the year.
func (h HolidaySet) Has(year int) bool { return h.years[year] }

// IsHoliday reports whether date ("YYYY-MM-DD", in the tariff's time zone) is
// a holiday. It is false for every date of a year without data.
func (h HolidaySet) IsHoliday(date string) bool { return h.dates[date] }

// Years returns the years with data, ascending.
func (h HolidaySet) Years() []int {
	out := make([]int, 0, len(h.years))
	for y := range h.years {
		out = append(out, y)
	}
	sort.Ints(out)
	return out
}

// Dates returns the holidays of a year, ascending; nil without data.
func (h HolidaySet) Dates(year int) []string {
	var out []string
	prefix := strconv.Itoa(year) + "-"
	for d := range h.dates {
		if strings.HasPrefix(d, prefix) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// lastYear is the latest year with data, 0 for none.
func (h HolidaySet) lastYear() int {
	last := 0
	for y := range h.years {
		if y > last {
			last = y
		}
	}
	return last
}
