// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"regexp"
	"strconv"
)

// Slides bound to a teaching week (lead freeze .claude/kinds-brief.md 1.5).
// A material is bound to one lecture (material.lecture_id), to one teaching
// week (material.bind_week) or to neither (course-wide).

// Binding scopes (mirror model.Bind*).
const (
	BindLecture = "lecture"
	BindWeek    = "week"
	BindCourse  = "course"
)

// MaterialBinding is one material of a course as the set function needs it.
type MaterialBinding struct {
	ID        string
	LectureID string // bound to that lecture ("" = not bound to a lecture)
	Week      int    // bound to that teaching week (0 = not bound to a week)
}

// Scope is the binding scope of m (a lecture wins over a week, though the
// Store never stores both).
func (m MaterialBinding) Scope() string {
	switch {
	case m.LectureID != "":
		return BindLecture
	case m.Week > 0:
		return BindWeek
	}
	return BindCourse
}

// MaterialUse is one member of a lecture's material set and why it is in it.
type MaterialUse struct {
	ID  string
	Via string // BindLecture | BindWeek | BindCourse
}

// MaterialSet is the material set of one lecture (lead ruling 1.5): the
// materials bound to it, then those bound to its teaching week, then the
// course-wide ones, each group in the order of materials. week is the
// lecture's teaching week (Numbered.Week, TeachingWeek); 0 when it cannot
// be computed — such a lecture uses no week-bound material. Never nil.
func MaterialSet(materials []MaterialBinding, lectureID string, week int) []MaterialUse {
	out := []MaterialUse{}
	for _, via := range []string{BindLecture, BindWeek, BindCourse} {
		for _, m := range materials {
			if m.Scope() != via {
				continue
			}
			switch via {
			case BindLecture:
				if m.LectureID != lectureID {
					continue
				}
			case BindWeek:
				if week < 1 || m.Week != week {
					continue
				}
			}
			out = append(out, MaterialUse{ID: m.ID, Via: via})
		}
	}
	return out
}

// MaterialSets is MaterialSet for every numbered lecture (its Week), keyed
// by lecture id.
func MaterialSets(materials []MaterialBinding, nums []Numbered) map[string][]MaterialUse {
	out := make(map[string][]MaterialUse, len(nums))
	for _, n := range nums {
		out[n.ID] = MaterialSet(materials, n.ID, n.Week)
	}
	return out
}

// SetDiff compares two material sets by id: the ids in after but not in
// before (added) and in before but not in after (removed), each in the
// order of its set. A material whose reason (Via) changed is in neither.
func SetDiff(before, after []MaterialUse) (added, removed []string) {
	in := func(set []MaterialUse, id string) bool {
		for _, u := range set {
			if u.ID == id {
				return true
			}
		}
		return false
	}
	for _, u := range after {
		if !in(before, u.ID) {
			added = append(added, u.ID)
		}
	}
	for _, u := range before {
		if !in(after, u.ID) {
			removed = append(removed, u.ID)
		}
	}
	return added, removed
}

// MaxSuggestedWeek is the largest week WeekFromFilename suggests. Larger
// numbers are more likely ISO calendar weeks ("2026-W40") than teaching
// weeks; the user can still bind up to week 60 by hand.
const MaxSuggestedWeek = 30

var (
	// "Week3", "week 3", "Week_03", "wk3", "Wk-03", "weeks"… not: "Weekly", "weeks 3".
	weekWord = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:week|wk)[\s_.-]?0*([1-9][0-9]?)(?:[^0-9]|$)`)
	// "W03", "W3 - Block ciphers": a bare W needs a separator after the
	// number too, so "w3c" or "W3L1" are not weeks.
	weekLetter = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])w[\s_-]?0*([1-9][0-9]?)(?:[^\p{L}\p{N}]|$)`)
	// "第3周", "第 03 周".
	weekCJK = regexp.MustCompile(`第\s*0*([1-9][0-9]?)\s*周`)
)

// WeekFromFilename suggests the teaching week a slide file belongs to from
// its name (Material.week_suggested): "Week3-Slides.pdf", "week 3",
// "Week_03", "wk 3", "W03 - Block ciphers.pdf", "第3周". Numbers that are
// not marked as a week ("Lecture-2", "ch2", "StreamCiphers-2") are not
// weeks; nor are weeks above MaxSuggestedWeek. The first match wins, in the
// order: week words, bare W, Chinese.
func WeekFromFilename(name string) (week int, ok bool) {
	for _, re := range []*regexp.Regexp{weekWord, weekLetter, weekCJK} {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > MaxSuggestedWeek {
			continue
		}
		return n, true
	}
	return 0, false
}
