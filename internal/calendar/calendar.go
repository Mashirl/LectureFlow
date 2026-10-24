// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package calendar computes the calendar-based lecture numbers of SPEC-v2
// P13.4 ("W02L1") and the recording dates of P13.3 (per-course file-name
// rules, the recorded_at resolution order).
//
// It is pure: no database, files, network, clock or randomness. Callers pass
// every time value. Times are local wall-clock times: the package reads only
// their calendar fields (date, hour, minute) in their own location and never
// subtracts instants, so results do not depend on the machine's time zone
// and a 23- or 25-hour day at a DST change does not shift a week.
package calendar

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Unknown is the label of a lecture whose number cannot be computed
// (P13.4: no term_start or recorded_at, before term_start, or in a skipped
// week).
const Unknown = "W??"

// Text layouts of the stored values (P13.2): course.term_start and the
// entries of course.skip_weeks are dates; lecture.recorded_at is local time
// to the minute. A recorded_at without a known time of day is stored as
// 00:00 (see HasClock).
const (
	DateLayout       = "2006-01-02"
	RecordedAtLayout = "2006-01-02T15:04"
)

// AlignMonday returns 00:00 of the Monday of d's calendar week (Monday to
// Sunday), in d's location. P13.4: the term start may be any day of
// teaching week 1.
func AlignMonday(d time.Time) time.Time {
	y, m, day := d.Date()
	return time.Date(y, m, day-mondayOffset(d), 0, 0, 0, 0, d.Location())
}

// Label is P13.4. week is the teaching week of recordedAt counted from the
// calendar week of termStart, not counting skipWeeks; ok is false when
// recordedAt is before that week, falls in a skipped week, or either time is
// zero. n is the 1-based rank of recordedAt among the sameWeekLectures that
// fall in the same week (the caller passes every lecture of the course,
// including this one). Lectures with an identical time share a rank here;
// LabelAll breaks such ties.
func Label(recordedAt, termStart time.Time, skipWeeks []time.Time, sameWeekLectures []time.Time) (week int, n int, ok bool) {
	if recordedAt.IsZero() || termStart.IsZero() {
		return 0, 0, false
	}
	c := newTerm(termStart, skipWeeks)
	week, ok = c.week(recordedAt)
	if !ok {
		return 0, 0, false
	}
	mon := mondayNum(recordedAt)
	n = 1
	for _, o := range sameWeekLectures {
		if !o.IsZero() && mondayNum(o) == mon && wallCmp(o, recordedAt) < 0 {
			n++
		}
	}
	return week, n, true
}

// FormatLabel renders a lecture number: "W02L1", "W11L2"; Unknown ("W??")
// when !ok.
func FormatLabel(week, n int, ok bool) string {
	if !ok || week < 1 || n < 1 {
		return Unknown
	}
	return fmt.Sprintf("W%02dL%d", week, n)
}

// WeekRange returns the Monday and the Sunday (both at 00:00, in termStart's
// location) of teaching week `week`, for the group headings of the lecture
// list ("第 2 周 · 10月6日–10月12日", formatted by the front end). ok is false
// for week < 1 or a zero termStart.
func WeekRange(week int, termStart time.Time, skipWeeks []time.Time) (monday, sunday time.Time, ok bool) {
	if week < 1 || termStart.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	c := newTerm(termStart, skipWeeks)
	mon := c.start + 7*int64(week-1)
	for _, s := range c.sortedSkips() {
		if s > mon {
			break
		}
		if s >= c.start {
			mon += 7 // a skipped week before (or at) it pushes it one week later
		}
	}
	y, m, d := time.Unix(mon*secondsPerDay, 0).UTC().Date()
	loc := termStart.Location()
	return time.Date(y, m, d, 0, 0, 0, 0, loc), time.Date(y, m, d+6, 0, 0, 0, 0, loc), true
}

// LectureTime is one lecture of a course as LabelAll needs it.
type LectureTime struct {
	ID               string
	RecordedAt       *time.Time
	OriginalFilename string
	// Kind and KindSource are the stored lecture.kind ("" = not decided)
	// and kind_source (lecture kinds, K1); LabelKinds derives the effective
	// kind from them (EffectiveKind).
	Kind       string
	KindSource string
}

// Numbered is the computed number of one lecture.
type Numbered struct {
	ID string
	// Week is the teaching week (any kind; 0 = cannot be computed). N is the
	// number in Label: the rank within the week and kind for a session
	// kind, within the course and kind for a material kind (0 = none).
	Week, N int
	// OK: the lecture has a number (Label is not Unknown). A material kind
	// is numbered without a week; a session kind only with one.
	OK            bool
	Label         string // W02L1, W02T1, P1; Unknown ("W??")
	SameDayByName bool   // its order within its day was inferred from file names ("同日顺序按文件名推测")

	// Lecture kinds (K1; LabelKinds).
	Kind      string // the effective kind's id (EffectiveKind)
	Code      string // its code
	Group     string // its group (GroupSession | GroupMaterial)
	KindN     int    // display counter "第 n 节": rank within the same week and kind (material kinds without a week: within the same kind without a week); 0 = none
	KindCount int    // how many lectures share that counter
	ShowN     bool   // show KindN, by the course's numbering mode
	CiteLabel string // the citation label (CiteLabels over the course): Label, or "W??-{id prefix}"
}

// LabelAll numbers every lecture of a course at once (P13.4, P13.3.4) with
// the built-in kind catalog and the auto numbering mode: LabelKinds(...,
// NumberOptions{}). The result is parallel to lectures (out[i] belongs to
// lectures[i]). Lectures are ranked within a teaching week in the order of
// Order. termStart nil → every session kind's label is Unknown
// (SameDayByName is still reported: it describes the list order, which
// does not depend on the term start). Lectures without a stored kind are
// lectures, so for them the result is that of P13.4.
func LabelAll(lectures []LectureTime, termStart *time.Time, skipWeeks []time.Time) []Numbered {
	return LabelKinds(lectures, termStart, skipWeeks, NumberOptions{})
}

// Order returns the indices of lectures in list order (P13.4, P13.3.4):
// by recording date; within one day by time of day, and by original file
// name (byte order, then the caller's order) where the time cannot decide —
// lectures without a time of day, or with identical times. Lectures without
// a recording time come last, in the caller's order. The global sequence
// number lecture.seq (P13.2) is the position in this order plus one.
func Order(lectures []LectureTime) []int {
	order, _ := sortLectures(lectures)
	return order
}

// HasClock reports whether t carries a time of day. A recording time known
// only to the day is represented as 00:00 (a lecture at exactly midnight is
// treated the same way).
func HasClock(t time.Time) bool {
	h, m, s := t.Clock()
	return h != 0 || m != 0 || s != 0 || t.Nanosecond() != 0
}

// CiteLabels returns the cite-JSON label of each lecture (P13.6): its number,
// or "W??-" plus a prefix of its id when it has none. The prefix is 6
// characters, lengthened (for all such lectures alike) only as far as needed
// to keep the labels unique: lecture ids are ULIDs, whose first characters
// encode the creation time, so lectures uploaded in one batch usually share
// their first 6 characters.
func CiteLabels(nums []Numbered) []string {
	out := make([]string, len(nums))
	var unknown []int
	maxLen := 0
	for i, n := range nums {
		if n.OK && n.Label != Unknown {
			out[i] = n.Label
			continue
		}
		unknown = append(unknown, i)
		if len(n.ID) > maxLen {
			maxLen = len(n.ID)
		}
	}
	size := 6
	for ; size < maxLen; size++ {
		seen := map[string]bool{}
		unique := true
		for _, i := range unknown {
			p := prefix(nums[i].ID, size)
			if seen[p] {
				unique = false
				break
			}
			seen[p] = true
		}
		if unique {
			break
		}
	}
	for _, i := range unknown {
		out[i] = Unknown + "-" + prefix(nums[i].ID, size)
	}
	return out
}

// FormatDate renders a course date (term_start, a skip_weeks entry).
func FormatDate(t time.Time) string { return t.Format(DateLayout) }

// ParseDate parses a course date in loc.
func ParseDate(s string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation(DateLayout, strings.TrimSpace(s), loc)
}

// FormatRecordedAt renders lecture.recorded_at (local time to the minute).
func FormatRecordedAt(t time.Time) string { return t.Format(RecordedAtLayout) }

// ParseRecordedAt parses lecture.recorded_at in loc. Besides
// RecordedAtLayout it accepts seconds, a space instead of "T", and a bare
// date (which means "time of day unknown", see HasClock).
func ParseRecordedAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	var err error
	for _, layout := range []string{RecordedAtLayout, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05", DateLayout} {
		var t time.Time
		if t, err = time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

// NormalizeSkipWeeks aligns every entry to its Monday, drops duplicates and
// sorts them, for storing course.skip_weeks.
func NormalizeSkipWeeks(weeks []time.Time) []time.Time {
	seen := map[int64]bool{}
	out := []time.Time{}
	for _, w := range weeks {
		if w.IsZero() || seen[mondayNum(w)] {
			continue
		}
		seen[mondayNum(w)] = true
		out = append(out, AlignMonday(w))
	}
	sort.Slice(out, func(i, j int) bool { return mondayNum(out[i]) < mondayNum(out[j]) })
	return out
}

const secondsPerDay = 86400

// term is a term start and its skipped weeks, as day numbers of Mondays.
type term struct {
	start int64
	skip  map[int64]bool
}

func newTerm(termStart time.Time, skipWeeks []time.Time) term {
	t := term{start: mondayNum(termStart), skip: map[int64]bool{}}
	for _, w := range skipWeeks {
		if !w.IsZero() {
			t.skip[mondayNum(w)] = true
		}
	}
	return t
}

// week returns the teaching week of rec (P13.4).
func (t term) week(rec time.Time) (int, bool) {
	mon := mondayNum(rec)
	if mon < t.start || t.skip[mon] {
		return 0, false
	}
	week := int((mon-t.start)/7) + 1
	for s := range t.skip {
		if s >= t.start && s < mon {
			week--
		}
	}
	return week, true
}

func (t term) sortedSkips() []int64 {
	out := make([]int64, 0, len(t.skip))
	for s := range t.skip {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// dayNum is the number of days from 1970-01-01 to t's calendar date, read in
// t's own location.
func dayNum(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / secondsPerDay
}

// mondayOffset is the number of days since the Monday of t's week.
func mondayOffset(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// mondayNum is the day number of the Monday of t's week.
func mondayNum(t time.Time) int64 { return dayNum(t) - int64(mondayOffset(t)) }

// wallCmp compares the wall-clock readings of a and b (date and time of day,
// each in its own location).
func wallCmp(a, b time.Time) int {
	if da, db := dayNum(a), dayNum(b); da != db {
		return cmpInt64(da, db)
	}
	ha, ma, sa := a.Clock()
	hb, mb, sb := b.Clock()
	ka := int64(ha*3600+ma*60+sa)*1e9 + int64(a.Nanosecond())
	kb := int64(hb*3600+mb*60+sb)*1e9 + int64(b.Nanosecond())
	return cmpInt64(ka, kb)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func dated(t *time.Time) bool { return t != nil && !t.IsZero() }

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sortLectures returns the list order (see Order) and, parallel to
// lectures, whether each one's place within its day came from file names.
//
// Within a day, lectures with a time of day keep their time order and those
// without one are ordered by file name; the two sequences are merged by file
// name, so a known time is never contradicted.
func sortLectures(lectures []LectureTime) (order []int, byName []bool) {
	byName = make([]bool, len(lectures))
	days := map[int64][]int{}
	var dayKeys []int64
	var undated []int
	for i, l := range lectures {
		if !dated(l.RecordedAt) {
			undated = append(undated, i)
			continue
		}
		d := dayNum(*l.RecordedAt)
		if _, seen := days[d]; !seen {
			dayKeys = append(dayKeys, d)
		}
		days[d] = append(days[d], i)
	}
	sort.Slice(dayKeys, func(i, j int) bool { return dayKeys[i] < dayKeys[j] })

	byFile := func(a, b int) bool {
		if c := strings.Compare(lectures[a].OriginalFilename, lectures[b].OriginalFilename); c != 0 {
			return c < 0
		}
		return a < b
	}
	for _, d := range dayKeys {
		var timed, untimed []int
		for _, i := range days[d] {
			if HasClock(*lectures[i].RecordedAt) {
				timed = append(timed, i)
			} else {
				untimed = append(untimed, i)
			}
		}
		sort.SliceStable(timed, func(a, b int) bool {
			if c := wallCmp(*lectures[timed[a]].RecordedAt, *lectures[timed[b]].RecordedAt); c != 0 {
				return c < 0
			}
			return byFile(timed[a], timed[b])
		})
		sort.SliceStable(untimed, func(a, b int) bool { return byFile(untimed[a], untimed[b]) })

		if len(untimed) > 0 && len(days[d]) > 1 {
			for _, i := range days[d] {
				byName[i] = true
			}
		}
		for a := 0; a+1 < len(timed); a++ {
			if wallCmp(*lectures[timed[a]].RecordedAt, *lectures[timed[a+1]].RecordedAt) == 0 {
				byName[timed[a]], byName[timed[a+1]] = true, true
			}
		}

		for len(timed) > 0 || len(untimed) > 0 {
			if len(timed) == 0 || (len(untimed) > 0 && byFile(untimed[0], timed[0])) {
				order, untimed = append(order, untimed[0]), untimed[1:]
			} else {
				order, timed = append(order, timed[0]), timed[1:]
			}
		}
	}
	return append(order, undated...), byName
}
