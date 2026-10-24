// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package tariff

import (
	"sort"
	"time"
)

// Peak membership is a function of the tariff's wall clock: the local date
// (weekday, holiday) and the minute of the day. Between two "edges" — a window
// boundary or midnight on the wall clock, or a change of UTC offset where the
// wall clock jumps — the wall clock runs continuously at a fixed offset, so the
// state cannot change. The functions below step from edge to edge; that keeps
// nextChange exact across days, weekends, holidays and DST in either zone.

// span is a compiled Window: [from, to) minutes after local midnight on the
// ISO weekdays marked in days.
type span struct {
	days     [8]bool
	from, to int
}

// plan is a Tariff compiled for evaluation.
type plan struct {
	loc      *time.Location
	spans    []span
	edges    []int       // distinct window edges in minutes, ascending, 0 < e < 24:00
	holidays *HolidaySet // nil when the tariff does not exclude holidays
}

// compile prepares cfg. It expects a validated Tariff (Parse): an unknown time
// zone is read as UTC and malformed windows are ignored.
func compile(cfg Tariff, holidays HolidaySet) *plan {
	loc, err := loadZone(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	p := &plan{loc: loc}
	seen := map[int]bool{}
	for _, w := range cfg.PeakWindows {
		s, reason := compileWindow(w)
		if reason != "" {
			continue
		}
		p.spans = append(p.spans, s)
		for _, e := range []int{s.from, s.to} {
			if e > 0 && e < minutesPerDay && !seen[e] {
				seen[e] = true
				p.edges = append(p.edges, e)
			}
		}
	}
	sort.Ints(p.edges)
	if cfg.ExcludeHolidays != "" {
		p.holidays = &holidays
	}
	return p
}

// State is P12.3: whether t is in a peak window, and when that next changes
// (the first instant with the other state, in t's location). A tariff without
// peak windows is always off-peak and nextChange is the zero time; so is a
// tariff whose state never changes (peak around the clock).
//
// Days are the windows' ISO weekdays read literally: weekend days worked in
// exchange for holidays are not modelled (P12.2). With ExcludeHolidays set,
// a holiday in holidays is off-peak all day; a year missing from holidays is
// computed by weekday and time only (see MissingHolidayData).
func State(t time.Time, cfg Tariff, holidays HolidaySet) (isPeak bool, nextChange time.Time) {
	p := compile(cfg, holidays)
	if len(p.spans) == 0 {
		return false, time.Time{}
	}
	t = t.Round(0) // drop the monotonic reading
	isPeak = p.peakAt(t)
	if next, ok := p.nextFlip(t, isPeak, p.horizon(t)); ok {
		nextChange = next.In(t.Location())
	}
	return isPeak, nextChange
}

// MissingHolidayData reports whether the holiday data lacks t's year (in
// cfg.Timezone) while cfg.ExcludeHolidays is set: that year is computed by
// weekday and time only, and the UI shows "节假日数据缺失" (P12.2).
func MissingHolidayData(t time.Time, cfg Tariff, holidays HolidaySet) bool {
	if cfg.ExcludeHolidays == "" {
		return false
	}
	loc, err := loadZone(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return !holidays.Has(t.In(loc).Year())
}

// PeakIntervals returns the peak intervals that overlap the LOCAL day
// containing t (loc = the user's zone), clipped to that day, in loc and in
// order — for "今日高峰（本地时间）：01:00–04:00、06:00–10:00" (P12.4). An
// interval clipped at the end of the day ends at the next day's 00:00. Empty
// means the whole local day is off-peak. A nil loc is UTC.
func PeakIntervals(t time.Time, loc *time.Location, cfg Tariff, holidays HolidaySet) [][2]time.Time {
	if loc == nil {
		loc = time.UTC
	}
	p := compile(cfg, holidays)
	if len(p.spans) == 0 {
		return nil
	}
	y, m, d := t.In(loc).Date()
	start, end := startOfDay(y, m, d, loc), startOfDay(y, m, d+1, loc)
	var out [][2]time.Time
	from, cur := start, p.peakAt(start)
	for x := start; ; {
		next, ok := p.nextFlip(x, cur, end)
		if !ok {
			next = end
		}
		if cur {
			out = append(out, [2]time.Time{from.In(loc), next.In(loc)})
		}
		if !ok {
			return out
		}
		from, x, cur = next, next, !cur
	}
}

// peakAt reports whether the instant x falls in a peak window.
func (p *plan) peakAt(x time.Time) bool {
	w := x.In(p.loc)
	// IsHoliday is false for every date of a year without data, which then
	// counts by weekday and time only.
	if p.holidays != nil && p.holidays.IsHoliday(w.Format(time.DateOnly)) {
		return false
	}
	day := int(w.Weekday())
	if day == 0 {
		day = 7 // ISO Sunday
	}
	// Edges are whole minutes, so the minute of the day decides [from, to).
	minute := w.Hour()*60 + w.Minute()
	for _, s := range p.spans {
		if s.days[day] && s.from <= minute && minute < s.to {
			return true
		}
	}
	return false
}

// nextFlip returns the first edge after x and before limit at which the state
// differs from cur; ok is false when there is none.
func (p *plan) nextFlip(x time.Time, cur bool, limit time.Time) (time.Time, bool) {
	for {
		x = p.nextEdge(x)
		if !x.Before(limit) {
			return time.Time{}, false
		}
		if p.peakAt(x) != cur {
			return x, true
		}
	}
}

// nextEdge returns the next instant after x at which the state may change: a
// window edge or midnight on the tariff's wall clock, or the end of the
// current UTC offset.
func (p *plan) nextEdge(x time.Time) time.Time {
	w := x.In(p.loc)
	tod := time.Duration(w.Hour())*time.Hour + time.Duration(w.Minute())*time.Minute +
		time.Duration(w.Second())*time.Second + time.Duration(w.Nanosecond())
	next := 24 * time.Hour // the next midnight
	for _, e := range p.edges {
		if d := time.Duration(e) * time.Minute; d > tod {
			next = d
			break
		}
	}
	// The offset is fixed until the zone ends, so wall-clock distance is real
	// distance up to there.
	y := x.Add(next - tod)
	if _, end := w.ZoneBounds(); !end.IsZero() && end.Before(y) {
		y = end
	}
	return y
}

// horizon bounds the search for the next change. After the last year of
// holiday data the pattern repeats weekly, so a tariff whose state can change
// does so within a week of that (32 days also cover a window skipped by a DST
// gap and the following week); a tariff that never changes (peak around the
// clock) gives up there.
func (p *plan) horizon(t time.Time) time.Time {
	from := t
	if p.holidays != nil {
		if last := p.holidays.lastYear(); last != 0 {
			if end := time.Date(last+1, 1, 1, 0, 0, 0, 0, p.loc); end.After(from) {
				from = end
			}
		}
	}
	return from.Add(32 * 24 * time.Hour)
}

// startOfDay returns the first instant of the local date y-m-d (normalised, so
// d may be one past the month's end) in loc. Where local midnight falls in a
// DST gap (Havana, Santiago, Cairo), time.Date resolves it to an instant of
// the previous date; the day then starts when the gap ends.
func startOfDay(y int, m time.Month, d int, loc *time.Location) time.Time {
	y, m, d = time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Date()
	s := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if sy, sm, sd := s.Date(); sy != y || sm != m || sd != d {
		if _, end := s.ZoneBounds(); !end.IsZero() {
			s = end
		}
	}
	return s
}
