// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // Europe/London for the DST test, independent of the machine
)

func TestAlignMonday(t *testing.T) {
	cases := []struct {
		in, want time.Time
	}{
		{at(2026, 9, 30, 9, 35), day(2026, 9, 28)},  // Wednesday
		{at(2026, 9, 28, 0, 0), day(2026, 9, 28)},   // Monday itself
		{at(2026, 10, 4, 23, 59), day(2026, 9, 28)}, // Sunday belongs to the week before
		{at(2027, 1, 1, 12, 0), day(2026, 12, 28)},  // across the year boundary
		{at(2028, 3, 1, 8, 0), day(2028, 2, 28)},    // across a leap day
	}
	for _, c := range cases {
		got := AlignMonday(c.in)
		if !got.Equal(c.want) || got.Location() != bst {
			t.Errorf("AlignMonday(%v) = %v, want %v in %v", c.in, got, c.want, bst)
		}
	}
}

func TestFormatLabel(t *testing.T) {
	cases := []struct {
		week, n int
		ok      bool
		want    string
	}{
		{2, 1, true, "W02L1"},
		{11, 2, true, "W11L2"},
		{1, 10, true, "W01L10"},
		{100, 3, true, "W100L3"},
		{2, 1, false, "W??"},
		{0, 1, true, "W??"},
		{3, 0, true, "W??"},
	}
	for _, c := range cases {
		if got := FormatLabel(c.week, c.n, c.ok); got != c.want {
			t.Errorf("FormatLabel(%d, %d, %v) = %q, want %q", c.week, c.n, c.ok, got, c.want)
		}
	}
}

// CSC1003 autumn 2026: teaching week 1 is the week of Monday 28 September.
// The term start is given as a Wednesday to check the alignment.
func TestLabel(t *testing.T) {
	termStart := day(2026, 9, 30)
	readingWeek := day(2026, 10, 21) // a Wednesday: the week of 19 October is skipped
	skip := []time.Time{readingWeek}
	all := []time.Time{
		at(2026, 9, 29, 12, 35), at(2026, 9, 30, 9, 35), // week 1, two lectures
		at(2026, 10, 7, 9, 35),  // week 2
		at(2026, 10, 14, 9, 35), // week 3
		at(2026, 10, 21, 9, 35), // skipped week
		at(2026, 10, 28, 9, 35), // calendar week 5 → teaching week 4
	}
	cases := []struct {
		rec     time.Time
		week, n int
		ok      bool
	}{
		{at(2026, 9, 29, 12, 35), 1, 1, true},
		{at(2026, 9, 30, 9, 35), 1, 2, true},
		{at(2026, 10, 7, 9, 35), 2, 1, true},
		{at(2026, 10, 14, 9, 35), 3, 1, true},
		{at(2026, 10, 21, 9, 35), 0, 0, false}, // in a skipped week
		{at(2026, 10, 28, 9, 35), 4, 1, true},  // the skipped week does not count
		{at(2026, 9, 27, 23, 59), 0, 0, false}, // Sunday before the term
		{at(2026, 9, 28, 0, 0), 1, 1, true},    // first minute of the term
		{at(2026, 11, 1, 23, 59), 4, 2, true},  // Sunday ends the teaching week; ranks after the 28th
		{time.Time{}, 0, 0, false},             // no recording time
	}
	for _, c := range cases {
		w, n, ok := Label(c.rec, termStart, skip, append(slices.Clone(all), c.rec))
		if w != c.week || n != c.n || ok != c.ok {
			t.Errorf("Label(%v) = (%d, %d, %v), want (%d, %d, %v)", c.rec, w, n, ok, c.week, c.n, c.ok)
		}
	}
	if _, _, ok := Label(at(2026, 10, 7, 9, 35), time.Time{}, nil, all); ok {
		t.Error("Label without a term start must not be ok")
	}
	// Without skipped weeks the 28 October lecture is in week 5.
	if w, n, ok := Label(at(2026, 10, 28, 9, 35), termStart, nil, all); w != 5 || n != 1 || !ok {
		t.Errorf("no skip: got (%d, %d, %v), want (5, 1, true)", w, n, ok)
	}
	// Identical times share a rank; LabelAll breaks the tie.
	same := []time.Time{at(2026, 10, 16, 9, 35), at(2026, 10, 16, 9, 35), at(2026, 10, 13, 13, 35)}
	if w, n, ok := Label(same[0], termStart, skip, same); w != 3 || n != 2 || !ok {
		t.Errorf("tie: got (%d, %d, %v), want (3, 2, true)", w, n, ok)
	}
}

// A skipped week that is the term start's own week: week 1 is the next one.
func TestLabelSkipFirstWeek(t *testing.T) {
	termStart := day(2026, 9, 28)
	skip := []time.Time{day(2026, 9, 28), day(2026, 9, 28)} // duplicates count once
	if _, _, ok := Label(at(2026, 9, 30, 9, 35), termStart, skip, nil); ok {
		t.Error("a lecture in the skipped first week must not be ok")
	}
	if w, _, ok := Label(at(2026, 10, 7, 9, 35), termStart, skip, nil); w != 1 || !ok {
		t.Errorf("got week %d ok=%v, want 1 true", w, ok)
	}
}

// Week boundaries follow the wall clock across the end of British Summer
// Time (Sunday 25 October 2026), where that day is 25 hours long.
func TestLabelDST(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	termStart := time.Date(2026, 9, 28, 0, 0, 0, 0, london)
	cases := []struct {
		rec  time.Time
		week int
	}{
		{time.Date(2026, 10, 25, 23, 30, 0, 0, london), 4}, // Sunday evening, after the clocks went back
		{time.Date(2026, 10, 26, 0, 30, 0, 0, london), 5},  // Monday
		{time.Date(2027, 3, 28, 23, 30, 0, 0, london), 26}, // Sunday after the clocks went forward
		{time.Date(2027, 3, 29, 0, 30, 0, 0, london), 27},
	}
	for _, c := range cases {
		if w, _, ok := Label(c.rec, termStart, nil, nil); w != c.week || !ok {
			t.Errorf("Label(%v) week = %d ok=%v, want %d", c.rec, w, ok, c.week)
		}
	}
	mon, sun, ok := WeekRange(5, termStart, nil)
	if !ok || mon.Format("2006-01-02") != "2026-10-26" || sun.Format("2006-01-02") != "2026-11-01" || mon.Location() != london {
		t.Errorf("WeekRange(5) = %v – %v ok=%v", mon, sun, ok)
	}
}

func TestWeekRange(t *testing.T) {
	termStart := day(2026, 9, 30)
	skip := []time.Time{day(2026, 10, 19)}
	cases := []struct {
		week     int
		skip     []time.Time
		mon, sun string
		ok       bool
	}{
		{1, nil, "2026-09-28", "2026-10-04", true},
		{2, nil, "2026-10-05", "2026-10-11", true},
		{3, skip, "2026-10-12", "2026-10-18", true},
		{4, skip, "2026-10-26", "2026-11-01", true}, // the week of 19 October is skipped
		{14, nil, "2026-12-28", "2027-01-03", true}, // across the year boundary
		{1, []time.Time{day(2026, 9, 29)}, "2026-10-05", "2026-10-11", true},
		{0, nil, "", "", false},
		{-1, nil, "", "", false},
	}
	for _, c := range cases {
		mon, sun, ok := WeekRange(c.week, termStart, c.skip)
		if ok != c.ok {
			t.Errorf("WeekRange(%d) ok = %v, want %v", c.week, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got := FormatDate(mon); got != c.mon {
			t.Errorf("WeekRange(%d) monday = %s, want %s", c.week, got, c.mon)
		}
		if got := FormatDate(sun); got != c.sun {
			t.Errorf("WeekRange(%d) sunday = %s, want %s", c.week, got, c.sun)
		}
		if HasClock(mon) || HasClock(sun) || mon.Location() != bst {
			t.Errorf("WeekRange(%d) = %v, %v: want 00:00 in %v", c.week, mon, sun, bst)
		}
		// Round trip: a lecture on that Monday or Sunday is in that week.
		for _, d := range []time.Time{mon, sun.Add(23 * time.Hour)} {
			if w, _, ok := Label(d, termStart, c.skip, nil); w != c.week || !ok {
				t.Errorf("Label(%v) = %d, want %d", d, w, c.week)
			}
		}
	}
	if _, _, ok := WeekRange(1, time.Time{}, nil); ok {
		t.Error("WeekRange without a term start must not be ok")
	}

	// Consecutive skipped weeks, a skipped week before the term (ignored)
	// and unsorted, duplicated input: every week round-trips through Label
	// and never lands in a skipped week.
	skips := []time.Time{day(2026, 12, 28), day(2026, 10, 21), day(2026, 9, 21), day(2026, 12, 21), day(2026, 10, 26), day(2026, 12, 24)}
	prev := time.Time{}
	for w := 1; w <= 30; w++ {
		mon, _, ok := WeekRange(w, termStart, skips)
		if !ok || !mon.After(prev) {
			t.Fatalf("WeekRange(%d) = %v ok=%v after %v", w, mon, ok, prev)
		}
		prev = mon
		if got, _, ok := Label(mon, termStart, skips, nil); got != w || !ok {
			t.Errorf("WeekRange(%d) = %s, but Label gives week %d ok=%v", w, FormatDate(mon), got, ok)
		}
	}
	if mon, _, _ := WeekRange(17, termStart, skips); FormatDate(mon) != "2027-02-15" {
		t.Errorf("WeekRange(17) = %s, want 2027-02-15 (18 January plus 4 skipped weeks)", FormatDate(mon))
	}
	if _, _, ok := WeekRange(1_000_000, termStart, skips); !ok {
		t.Error("WeekRange of a large week must not fail")
	}
}

// csc3121 is the CSC1002 lecture folder (real file names): three lectures on
// Tuesdays and three tutorials on one Friday, plus two made-up lectures, one
// recorded in the reading week and one before the term, and one without a
// recording time.
func csc3121() []LectureTime {
	return []LectureTime{
		{ID: "a1", RecordedAt: tp(at(2026, 10, 6, 13, 35)), OriginalFilename: "CSC1002-L01-01 Tue Oct 06 13-35-00 BST 2026.mp4"},
		{ID: "a2", RecordedAt: tp(at(2026, 10, 13, 13, 35)), OriginalFilename: "CSC1002-L01-01 Tue Oct 13 13-35-00 BST 2026.mp4"},
		{ID: "a3", RecordedAt: tp(at(2026, 9, 29, 13, 35)), OriginalFilename: "CSC1002-L01-01 Tue Sep 29 13-35-00 BST 2026.mp4"},
		{ID: "a4", RecordedAt: tp(at(2026, 10, 16, 13, 35)), OriginalFilename: "CSC1002-T01-01 Fri Oct 16 13-35-00 BST 2026.mp4"},
		{ID: "a5", RecordedAt: tp(at(2026, 10, 16, 12, 35)), OriginalFilename: "CSC1002-T01-02 Fri Oct 16 12-35-00 BST 2026.mp4"},
		{ID: "a6", RecordedAt: tp(at(2026, 10, 16, 9, 35)), OriginalFilename: "CSC1002-T01-03 Fri Oct 16 09-35-00 BST 2026.mp4"},
		{ID: "a7", RecordedAt: nil, OriginalFilename: "screen recording.mp4"},
		{ID: "a8", RecordedAt: tp(at(2026, 10, 20, 13, 35)), OriginalFilename: "CSC1002-L01-01 Tue Oct 20 13-35-00 BST 2026.mp4"},
		{ID: "a9", RecordedAt: tp(at(2026, 10, 27, 13, 35)), OriginalFilename: "CSC1002-L01-01 Tue Oct 27 13-35-00 GMT 2026.mp4"},
		{ID: "a10", RecordedAt: tp(at(2026, 9, 22, 13, 35)), OriginalFilename: "CSC1002 induction Tue Sep 22.mp4"},
	}
}

func TestLabelAllGolden(t *testing.T) {
	lectures := csc3121()
	termStart := day(2026, 10, 1) // any day of week 1
	skip := []time.Time{day(2026, 10, 19)}
	nums := LabelAll(lectures, &termStart, skip)
	golden(t, "labelall_csc3121.golden", renderNumbered(lectures, nums))

	want := map[string]string{
		"a3": "W01L1", "a1": "W02L1", "a2": "W03L1",
		"a6": "W03L2", "a5": "W03L3", "a4": "W03L4", // three tutorials on Friday, in time order
		"a8": "W??", "a9": "W04L1", "a10": "W??", "a7": "W??",
	}
	for i, n := range nums {
		if n.ID != lectures[i].ID {
			t.Fatalf("result %d is %s, want %s (results are parallel to the input)", i, n.ID, lectures[i].ID)
		}
		if n.Label != want[n.ID] {
			t.Errorf("%s: label %s, want %s", n.ID, n.Label, want[n.ID])
		}
		if n.SameDayByName {
			t.Errorf("%s: SameDayByName with distinct times", n.ID)
		}
	}
}

func TestLabelAllWithoutTermStart(t *testing.T) {
	lectures := csc3121()
	for _, ts := range []*time.Time{nil, {}} {
		for _, n := range LabelAll(lectures, ts, nil) {
			if n.OK || n.Label != Unknown || n.Week != 0 || n.N != 0 {
				t.Errorf("term start %v: %s = %+v, want W??", ts, n.ID, n)
			}
		}
	}
}

// Same-day lectures without a time of day are ordered by file name (P13.3.4).
func TestLabelAllSameDayByName(t *testing.T) {
	termStart := day(2026, 9, 28)
	lectures := []LectureTime{
		{ID: "c", RecordedAt: tp(day(2026, 10, 7)), OriginalFilename: "Wk2 Oct 07 part c.mp4"},
		{ID: "a", RecordedAt: tp(day(2026, 10, 7)), OriginalFilename: "Wk2 Oct 07 part a.mp4"},
		{ID: "b", RecordedAt: tp(day(2026, 10, 7)), OriginalFilename: "Wk2 Oct 07 part b.mp4"},
		{ID: "x", RecordedAt: tp(day(2026, 10, 5)), OriginalFilename: "Wk2 Oct 05.mp4"}, // alone on its day
		// A day mixing known and unknown times: the known times keep their
		// order (Z before A) and the unknown one is merged in by file name.
		{ID: "tA", RecordedAt: tp(at(2026, 10, 14, 14, 0)), OriginalFilename: "A afternoon.mp4"},
		{ID: "tZ", RecordedAt: tp(at(2026, 10, 14, 9, 0)), OriginalFilename: "Z morning.mp4"},
		{ID: "uM", RecordedAt: tp(day(2026, 10, 14)), OriginalFilename: "M unknown.mp4"},
		// Identical times: file name, then the caller's order.
		{ID: "s2", RecordedAt: tp(at(2026, 10, 21, 9, 35)), OriginalFilename: "same b.mp4"},
		{ID: "s1", RecordedAt: tp(at(2026, 10, 21, 9, 35)), OriginalFilename: "same a.mp4"},
		{ID: "s3", RecordedAt: tp(at(2026, 10, 21, 11, 0)), OriginalFilename: "later.mp4"},
	}
	nums := LabelAll(lectures, &termStart, nil)
	golden(t, "labelall_same_day.golden", renderNumbered(lectures, nums))

	type exp struct {
		label  string
		byName bool
	}
	want := map[string]exp{
		"x": {"W02L1", false}, "a": {"W02L2", true}, "b": {"W02L3", true}, "c": {"W02L4", true},
		"uM": {"W03L1", true}, "tZ": {"W03L2", true}, "tA": {"W03L3", true},
		"s1": {"W04L1", true}, "s2": {"W04L2", true}, "s3": {"W04L3", false},
	}
	for _, n := range nums {
		if w := want[n.ID]; n.Label != w.label || n.SameDayByName != w.byName {
			t.Errorf("%s: %s byName=%v, want %s byName=%v", n.ID, n.Label, n.SameDayByName, w.label, w.byName)
		}
	}
	// The hint describes the order, so it is reported without a term start too.
	for i, n := range LabelAll(lectures, nil, nil) {
		if n.SameDayByName != nums[i].SameDayByName {
			t.Errorf("%s: SameDayByName differs without a term start", n.ID)
		}
	}
}

func TestOrder(t *testing.T) {
	lectures := []LectureTime{
		{ID: "u1", OriginalFilename: "z.mp4"},
		{ID: "d2", RecordedAt: tp(at(2026, 10, 7, 9, 35)), OriginalFilename: "b.mp4"},
		{ID: "u2", OriginalFilename: "a.mp4"},
		{ID: "d1", RecordedAt: tp(at(2026, 9, 29, 12, 35)), OriginalFilename: "c.mp4"},
		{ID: "z", RecordedAt: &time.Time{}, OriginalFilename: "0.mp4"}, // a zero time counts as unknown
	}
	var got []string
	for _, i := range Order(lectures) {
		got = append(got, lectures[i].ID)
	}
	// Dated lectures by time; undated ones last in the caller's order.
	if want := []string{"d1", "d2", "u1", "u2", "z"}; !slices.Equal(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
	if got := Order(nil); len(got) != 0 {
		t.Errorf("Order(nil) = %v", got)
	}
}

func TestCiteLabels(t *testing.T) {
	// ULIDs of one upload batch share their first 6 (timestamp) characters.
	nums := []Numbered{
		{ID: "01JB2X7K3M9ZQ4V8T6R2N1P0AA", Week: 2, N: 1, OK: true, Label: "W02L1"},
		{ID: "01JB2X7K4N1ABCDEFGHJKMNPQR", Label: Unknown},
		{ID: "01JB2X7K5P2ABCDEFGHJKMNPQR", Label: Unknown},
		{ID: "01JB2X7K5Q3ABCDEFGHJKMNPQR", Label: Unknown},
	}
	got := CiteLabels(nums)
	// 10 characters are needed to tell the last two apart; all unknown
	// labels use the same length.
	want := []string{"W02L1", "W??-01JB2X7K4N", "W??-01JB2X7K5P", "W??-01JB2X7K5Q"}
	if !slices.Equal(got, want) {
		t.Errorf("CiteLabels = %v, want %v", got, want)
	}

	distinct := []Numbered{{ID: "01JB2AAAAA", Label: Unknown}, {ID: "01JC3BBBBB", Label: Unknown}, {ID: "x", Label: Unknown}}
	if got, want := CiteLabels(distinct), []string{"W??-01JB2A", "W??-01JC3B", "W??-x"}; !slices.Equal(got, want) {
		t.Errorf("CiteLabels = %v, want %v (6 characters when they are unique)", got, want)
	}
	if got := CiteLabels(nil); len(got) != 0 {
		t.Errorf("CiteLabels(nil) = %v", got)
	}
}

func TestDateText(t *testing.T) {
	if got := FormatDate(at(2026, 9, 28, 13, 5)); got != "2026-09-28" {
		t.Errorf("FormatDate = %s", got)
	}
	d, err := ParseDate(" 2026-09-28 ", bst)
	if err != nil || !d.Equal(day(2026, 9, 28)) || d.Location() != bst {
		t.Errorf("ParseDate = %v, %v", d, err)
	}
	if got := FormatRecordedAt(time.Date(2026, 10, 7, 9, 35, 42, 0, bst)); got != "2026-10-07T09:35" {
		t.Errorf("FormatRecordedAt = %s (minute precision)", got)
	}
	cases := []struct {
		in       string
		want     time.Time
		hasClock bool
	}{
		{"2026-10-07T09:35", at(2026, 10, 7, 9, 35), true},
		{"2026-10-07T09:35:00", at(2026, 10, 7, 9, 35), true},
		{"2026-10-07 09:35", at(2026, 10, 7, 9, 35), true},
		{"2026-10-07", day(2026, 10, 7), false},
		{"2026-10-07T00:00", day(2026, 10, 7), false},
	}
	for _, c := range cases {
		got, err := ParseRecordedAt(c.in, bst)
		if err != nil || !got.Equal(c.want) || HasClock(got) != c.hasClock {
			t.Errorf("ParseRecordedAt(%q) = %v (clock %v), %v; want %v (clock %v)", c.in, got, HasClock(got), err, c.want, c.hasClock)
		}
	}
	for _, bad := range []string{"", "2026-13-01", "07/10/2026", "2026-10-07T25:00"} {
		if _, err := ParseRecordedAt(bad, bst); err == nil {
			t.Errorf("ParseRecordedAt(%q) accepted", bad)
		}
	}
}

func TestNormalizeSkipWeeks(t *testing.T) {
	got := NormalizeSkipWeeks([]time.Time{day(2026, 12, 23), day(2026, 10, 21), day(2026, 10, 19), {}, day(2026, 12, 27)})
	var s []string
	for _, d := range got {
		s = append(s, FormatDate(d))
	}
	if want := []string{"2026-10-19", "2026-12-21"}; !slices.Equal(s, want) {
		t.Errorf("NormalizeSkipWeeks = %v, want %v", s, want)
	}
	if got := NormalizeSkipWeeks(nil); got == nil || len(got) != 0 {
		t.Errorf("NormalizeSkipWeeks(nil) = %#v, want an empty slice (stored as [])", got)
	}
}
