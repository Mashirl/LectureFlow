// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// ---------------------------------------------------------------------------
// Lecture kinds, per-kind numbering and slides bound to a teaching week
// (user request 2026-10-23; lead freeze .claude/kinds-brief.md; migration
// 007_lecture_kinds.sql). Added by K1 (core-builder) on the lead's order.
// ---------------------------------------------------------------------------

// LectureKind is one row of lecture_kind: the global kind catalog.
type LectureKind struct {
	ID      string  // built-in: KindLecture … KindMaterial; custom: a ULID
	Builtin bool    // seeded by migration 007; never edited or deleted
	Code    string  // 1–3 capital letters, unique (ValidKindCode); part of the citation number
	Name    *string // custom kinds only; nil for built-in kinds (the UI translates them)
	Group   string  // KindGroupSession | KindGroupMaterial
	Order   int     // lecture_kind.ord: display order
}

// Built-in kind ids (seeded by migration 007).
const (
	KindLecture     = "lecture"      // L, session
	KindTutorial    = "tutorial"     // T, session
	KindSeminar     = "seminar"      // S, session
	KindPastLecture = "past_lecture" // P, material: a recording from an earlier year
	KindMaterial    = "material"     // M, material: additional teaching material
)

// BuiltinKindIDs lists the built-in kinds in catalog order.
var BuiltinKindIDs = []string{KindLecture, KindTutorial, KindSeminar, KindPastLecture, KindMaterial}

// Kind groups (lecture_kind.grp). A session kind is numbered per teaching
// week and kind ("W02T1"); a material kind per course and kind ("P1").
const (
	KindGroupSession  = "session"
	KindGroupMaterial = "material"
)

// Sources of lecture.kind (lecture.kind_source). The kind is decided in
// the order user > filename (the course's file-name rule, its "kind" group
// through kind_map) > llm (the title task) > none (treated as a lecture).
const (
	KindSourceNone     = "none"
	KindSourceFilename = "filename"
	KindSourceLLM      = "llm"
	KindSourceUser     = "user"
)

// Numbering modes (course.numbering): whether a lecture row shows its
// number within its week and kind ("第 n 节"). Display only.
const (
	NumberingAuto   = "auto"   // only when the week has more than one of that kind
	NumberingAlways = "always" // whenever the lecture has a number
	NumberingNever  = "never"
)

// Limits of a custom kind (validated by the API).
const (
	KindNameMaxLen = 40 // runes
	KindCodeMaxLen = 3
)

// ValidKindCode reports whether code is 1–3 capital ASCII letters.
func ValidKindCode(code string) bool {
	if len(code) < 1 || len(code) > KindCodeMaxLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

// ValidKindGroup reports whether g is a kind group.
func ValidKindGroup(g string) bool { return g == KindGroupSession || g == KindGroupMaterial }

// ValidNumbering reports whether n is a numbering mode ("" is not).
func ValidNumbering(n string) bool {
	return n == NumberingAuto || n == NumberingAlways || n == NumberingNever
}

// ValidKindSource reports whether s is a kind source ("" is not).
func ValidKindSource(s string) bool {
	return s == KindSourceNone || s == KindSourceFilename || s == KindSourceLLM || s == KindSourceUser
}

// MaxBindWeek is the largest teaching week a material can be bound to
// (material.bind_week is 1–60).
const MaxBindWeek = 60

// Material binding scopes (api Material.bind.scope): material.lecture_id
// set → lecture; material.bind_week set → week; neither → course.
const (
	BindLecture = "lecture"
	BindWeek    = "week"
	BindCourse  = "course"
)

// BindScope returns the binding scope of m.
func BindScope(m Material) string {
	switch {
	case m.LectureID != nil:
		return BindLecture
	case m.BindWeek != nil:
		return BindWeek
	}
	return BindCourse
}

// StaleCodeMaterials is the stale reason of a lecture whose material set
// (bound to it ∪ bound to its teaching week ∪ course-wide) changed: the
// course calendar, its recorded_at, a binding, or materials added or
// deleted. Params: added, removed — []string of file names, each absent
// when empty (no params when the previous set is unknown).
const StaleCodeMaterials = "materials_changed"

// KVMaterialSetsPrefix: material_sets:{course_id} → JSON of the material
// set of every lecture of the course as last seen by the invalidation
// (state.InvalidateCourseWith with the materials cause); the baseline that
// tells which lectures' sets changed and what was added or removed.
const KVMaterialSetsPrefix = "material_sets:"
