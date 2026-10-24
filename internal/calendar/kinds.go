// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"fmt"
	"strings"
	"time"
)

// Lecture kinds and per-kind numbering (user request 2026-10-23; lead freeze
// .claude/kinds-brief.md section 1). The constants mirror internal/model
// (calendar stays pure and imports nothing but the standard library;
// TestModelConstants checks they agree).

// Built-in kind ids (lecture_kind, migration 007).
const (
	KindLecture     = "lecture"
	KindTutorial    = "tutorial"
	KindSeminar     = "seminar"
	KindPastLecture = "past_lecture"
	KindMaterial    = "material"
)

// Kind groups: a session kind is numbered per teaching week and kind
// ("W02T1"), a material kind per course and kind ("P1").
const (
	GroupSession  = "session"
	GroupMaterial = "material"
)

// Sources of lecture.kind, in increasing precedence (lead ruling 1.2).
const (
	KindSourceNone     = "none"
	KindSourceLLM      = "llm"
	KindSourceFilename = "filename"
	KindSourceUser     = "user"
)

// Numbering modes (course.numbering): when a row shows "第 n 节" (ShowN).
const (
	NumberingAuto   = "auto"
	NumberingAlways = "always"
	NumberingNever  = "never"
)

// Kind is one entry of the kind catalog, as the numbering needs it.
type Kind struct {
	ID    string
	Code  string // the letters of the number: W02{Code}1, {Code}1
	Group string // GroupSession | GroupMaterial
}

// BuiltinKinds is the catalog seeded by migration 007.
func BuiltinKinds() []Kind {
	return []Kind{
		{ID: KindLecture, Code: "L", Group: GroupSession},
		{ID: KindTutorial, Code: "T", Group: GroupSession},
		{ID: KindSeminar, Code: "S", Group: GroupSession},
		{ID: KindPastLecture, Code: "P", Group: GroupMaterial},
		{ID: KindMaterial, Code: "M", Group: GroupMaterial},
	}
}

// catalog looks kinds up by id, falling back to the built-in lecture and
// past_lecture when the given catalog lacks them.
type catalog map[string]Kind

func newCatalog(kinds []Kind) catalog {
	if kinds == nil {
		kinds = BuiltinKinds()
	}
	c := catalog{}
	for _, k := range kinds {
		if k.ID != "" {
			c[k.ID] = k
		}
	}
	for _, k := range BuiltinKinds() {
		if k.ID == KindLecture || k.ID == KindPastLecture {
			if _, ok := c[k.ID]; !ok {
				c[k.ID] = k
			}
		}
	}
	return c
}

// effective is EffectiveKind on a catalog.
func (c catalog) effective(kind, source string, recordedAt, termStart *time.Time) Kind {
	k, ok := c[kind]
	if !ok || (k.Group != GroupSession && k.Group != GroupMaterial) {
		k = c[KindLecture]
	}
	if source != KindSourceUser && k.Group == GroupSession && PastRecording(recordedAt, termStart) {
		k = c[KindPastLecture]
	}
	return k
}

// EffectiveKind is the kind a lecture is numbered as (lead ruling 1.1;
// computed on every read, never stored): the stored kind (any source;
// empty or not in the catalog → lecture); then the past-lecture rule: a
// session kind not set by the user, recorded more than MaxDaysBeforeTerm
// days before the Monday of the term start, is a past_lecture (the same
// threshold as P13.3.2 rule 4, which now accepts such samples). kinds is
// the catalog (nil = BuiltinKinds()).
func EffectiveKind(kind, source string, recordedAt, termStart *time.Time, kinds []Kind) Kind {
	return newCatalog(kinds).effective(kind, source, recordedAt, termStart)
}

// PastRecording reports whether recordedAt is more than MaxDaysBeforeTerm
// days before the Monday of termStart's week (false when either is unknown).
func PastRecording(recordedAt, termStart *time.Time) bool {
	if !dated(recordedAt) || !dated(termStart) {
		return false
	}
	return dayNum(*recordedAt) < mondayNum(*termStart)-MaxDaysBeforeTerm
}

// KindSourceRank orders the kind sources (lead ruling 1.2): user 3 >
// filename 2 > llm 1 > none or unknown 0.
func KindSourceRank(source string) int {
	switch source {
	case KindSourceUser:
		return 3
	case KindSourceFilename:
		return 2
	case KindSourceLLM:
		return 1
	}
	return 0
}

// KindOverrides reports whether a kind decided by newSource may replace the
// stored kind of oldSource: a source never replaces a stronger one (the
// title task's llm kind never replaces a file-name or user kind), and does
// replace its own (a rule applied again).
func KindOverrides(newSource, oldSource string) bool {
	return KindSourceRank(newSource) >= KindSourceRank(oldSource)
}

// KindLabel renders a number: session kinds "W{week:02}{code}{n}" (W02T1),
// material kinds "{code}{n}" (P1); Unknown ("W??") when there is no number
// (n < 1, or a session kind without a week).
func KindLabel(k Kind, week, n int) string {
	switch {
	case n < 1:
		return Unknown
	case k.Group == GroupMaterial:
		return fmt.Sprintf("%s%d", k.Code, n)
	case week < 1:
		return Unknown
	}
	return fmt.Sprintf("W%02d%s%d", week, k.Code, n)
}

// NumberOptions are the course-level inputs of LabelKinds.
type NumberOptions struct {
	Kinds     []Kind // the kind catalog; nil = BuiltinKinds()
	Numbering string // course.numbering ("" = auto)
}

// LabelKinds numbers every lecture of a course (lead ruling 1.4), parallel
// to lectures, in the list order of Order:
//
//   - Kind/Code/Group: the effective kind (EffectiveKind).
//   - Week: the teaching week (P13.4) for any kind, 0 when it cannot be
//     computed.
//   - session kinds with a week: N = KindN = rank among the same week and
//     kind, Label "W02T1"; without a week: Label Unknown, OK false, N,
//     KindN, KindCount 0.
//   - material kinds: N = rank among the course's lectures of that kind
//     (dated by time, undated last), Label "P1", OK true (also without a
//     week). KindN/KindCount count within the same week and kind, or, for
//     those without a week, among the same kind without a week.
//   - KindCount: lectures sharing that KindN counter; ShowN by the course's
//     numbering: auto KindCount > 1, always KindN > 0, never false.
//   - CiteLabel: CiteLabels over the whole result (W02T1, P1, W??-{id}).
//
// A course whose lectures are all of kind lecture (none recorded more than
// MaxDaysBeforeTerm days before the term) gets exactly the Week, N, OK and
// Label of P13.4.
func LabelKinds(lectures []LectureTime, termStart *time.Time, skipWeeks []time.Time, opt NumberOptions) []Numbered {
	cat := newCatalog(opt.Kinds)
	out := make([]Numbered, len(lectures))
	order, byName := sortLectures(lectures)
	var tm *term
	if dated(termStart) {
		t := newTerm(*termStart, skipWeeks)
		tm = &t
	}
	if !dated(termStart) {
		termStart = nil
	}
	for i, l := range lectures {
		k := cat.effective(l.Kind, l.KindSource, l.RecordedAt, termStart)
		out[i] = Numbered{ID: l.ID, Label: Unknown, SameDayByName: byName[i], Kind: k.ID, Code: k.Code, Group: k.Group}
		if tm != nil && dated(l.RecordedAt) {
			if w, ok := tm.week(*l.RecordedAt); ok {
				out[i].Week = w
			}
		}
	}

	type counter struct {
		week int // 0 for material kinds without a week
		kind string
	}
	counts := map[counter]int{}
	inCourse := map[string]int{}
	keyed := make([]bool, len(lectures))
	keys := make([]counter, len(lectures))
	for _, i := range order {
		n := &out[i]
		switch {
		case n.Group == GroupMaterial:
			inCourse[n.Kind]++
			n.N, n.OK = inCourse[n.Kind], true
		case n.Week > 0:
		default:
			continue // a session kind without a week: W??
		}
		keys[i], keyed[i] = counter{n.Week, n.Kind}, true
		counts[keys[i]]++
		n.KindN = counts[keys[i]]
		if n.Group == GroupSession {
			n.N, n.OK = n.KindN, true
		}
		n.Label = KindLabel(Kind{ID: n.Kind, Code: n.Code, Group: n.Group}, n.Week, n.N)
	}
	for i := range out {
		if keyed[i] {
			out[i].KindCount = counts[keys[i]]
		}
		out[i].ShowN = showN(opt.Numbering, out[i].KindN, out[i].KindCount)
	}
	for i, cl := range CiteLabels(out) {
		out[i].CiteLabel = cl
	}
	return out
}

// showN applies a numbering mode ("" or unknown = auto).
func showN(numbering string, kindN, kindCount int) bool {
	switch numbering {
	case NumberingAlways:
		return kindN > 0
	case NumberingNever:
		return false
	}
	return kindCount > 1
}

// TeachingWeek is the teaching week of recordedAt (P13.4); ok is false
// without a term start or a recording time, before the term or in a
// skipped week.
func TeachingWeek(recordedAt, termStart *time.Time, skipWeeks []time.Time) (week int, ok bool) {
	if !dated(recordedAt) || !dated(termStart) {
		return 0, false
	}
	return newTerm(*termStart, skipWeeks).week(*recordedAt)
}

// CourseTerm parses course.term_start and skip_weeks in loc (the server's
// local zone): nil / dropped when unset or unreadable.
func CourseTerm(termStart *string, skipWeeks []string, loc *time.Location) (*time.Time, []time.Time) {
	var ts *time.Time
	if termStart != nil && strings.TrimSpace(*termStart) != "" {
		if t, err := ParseDate(*termStart, loc); err == nil {
			ts = &t
		}
	}
	skips := make([]time.Time, 0, len(skipWeeks))
	for _, w := range skipWeeks {
		if t, err := ParseDate(w, loc); err == nil {
			skips = append(skips, t)
		}
	}
	return ts, skips
}

// RecordedTime parses lecture.recorded_at in loc; nil when unset or unreadable.
func RecordedTime(recordedAt *string, loc *time.Location) *time.Time {
	if recordedAt == nil || strings.TrimSpace(*recordedAt) == "" {
		return nil
	}
	t, err := ParseRecordedAt(*recordedAt, loc)
	if err != nil {
		return nil
	}
	return &t
}
