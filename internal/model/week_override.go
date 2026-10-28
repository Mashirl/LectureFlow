// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// Week overrides (user request 2026-10-27, course-ux; migrations/
// 011_week_override.sql): the user places a lecture in a teaching week that
// its recording time does not give. A new table, so no column of lecture
// changes (CLAUDE.md: §3.3 tables are not altered). One override per
// lecture; it is deleted with the lecture (ON DELETE CASCADE).
//
//   - supplement: a teaching material (a lecture whose kind is of the
//     material group: Teaching Material - Additional, Past Lecture or a
//     user-defined material kind) hangs under week N as its supplementary
//     material. It keeps its own material number (M1, P1) and does not take
//     a number among the week's sessions.
//   - makeup ("补"): the lecture is the content that week N should have had
//     (a make-up class recorded later, or one recorded in advance). It is
//     numbered in week N like a session there, after the week's own
//     sessions of its kind, so their numbers do not move.
//
// Both take effect only when the course has a term start (without one
// there are no teaching weeks); the week then also decides the lecture's
// week-bound slides (state.LectureWeek).
const (
	WeekOverrideSupplement = "supplement"
	WeekOverrideMakeup     = "makeup"
)

// WeekOverride is one row of lecture_week_override.
type WeekOverride struct {
	Week      int    // teaching week, 1–MaxBindWeek
	Kind      string // WeekOverrideSupplement | WeekOverrideMakeup
	CreatedAt string // RFC 3339 UTC
}

// ValidWeekOverrideKind reports whether k is a WeekOverride kind.
func ValidWeekOverrideKind(k string) bool {
	return k == WeekOverrideSupplement || k == WeekOverrideMakeup
}

// ValidOverrideWeek reports whether w is a week an override may name (the
// same range as a material bound to a week).
func ValidOverrideWeek(w int) bool { return w >= 1 && w <= MaxBindWeek }
