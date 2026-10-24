// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// Lecture kinds with per-kind numbering and slides bound to a teaching week
// (user request 2026-10-23; lead freeze .claude/kinds-brief.md, section 3;
// the API shape of the proto-kinds prototype). The catalog is global
// (GET/POST /api/lecture-kinds, PATCH/DELETE /api/lecture-kinds/{id}); a
// lecture's kind is set with PUT /api/lectures/{id}/kind. The constants
// mirror internal/model.

// Kind groups: a "session" is taught in a teaching week and numbered per
// week (W02T1); a "material" is a recording used as teaching material (a
// past lecture, an additional video) and is numbered within the course (P1).
const (
	KindGroupSession  = model.KindGroupSession
	KindGroupMaterial = model.KindGroupMaterial
)

// Where a lecture's kind came from.
const (
	KindSourceFilename = model.KindSourceFilename // the kind group of the course's file-name rule and its kind_map
	KindSourceLLM      = model.KindSourceLLM      // judged from the summary when the file name says nothing
	KindSourceUser     = model.KindSourceUser     // set by hand (PUT /api/lectures/{id}/kind)
	KindSourceNone     = model.KindSourceNone     // not decided yet: shown as a lecture
	// KindSourceDate is a view value only (never stored): the stored kind
	// is a taught session (or none) not set by the user, but the recording
	// is more than 30 days older than the term start, so the effective kind
	// is past_lecture (lead ruling 1.1, calendar.PastRecording).
	KindSourceDate = "date"
)

// Course.Numbering: when the list shows "第 n 节" under the kind.
const (
	NumberingAuto   = model.NumberingAuto   // only when a week has more than one of that kind (default)
	NumberingAlways = model.NumberingAlways // every numbered lecture
	NumberingNever  = model.NumberingNever
)

// MaterialBind.Scope.
const (
	BindWeek    = model.BindWeek    // every lecture of that teaching week (all kinds)
	BindLecture = model.BindLecture // one lecture (the v1 binding)
	BindCourse  = model.BindCourse  // the whole course
)

// RenumberNotice is one entry of Course.renumber_notices
// (stages.RenumberNotice): {code: "calendar.renumbered_auto", params:
// {lecture, n, changes: [{lecture_id, from, to}]}, detail, at}.
type RenumberNotice = stages.RenumberNotice

// LectureKind is one entry of GET /api/lecture-kinds, the global catalog
// shared by every course. A built-in kind has Name == nil: the front end
// names it by ID in the interface language. A custom kind has the user's
// name (never translated).
type LectureKind struct {
	ID      string  `json:"id"`
	Builtin bool    `json:"builtin"`
	Code    string  `json:"code"` // 1–3 upper-case letters: W02{Code}1, {Code}1 in citations
	Name    *string `json:"name"`
	Group   string  `json:"group"` // KindGroup*
	Order   int     `json:"order"`
}

// LectureKindInput is the body of POST /api/lecture-kinds (name, code and
// group required) and PATCH /api/lecture-kinds/{id} (custom kinds only; the
// code and group of a kind in use cannot change: 409 kind.in_use).
type LectureKindInput struct {
	Name  *string `json:"name,omitempty"`
	Code  *string `json:"code,omitempty"`
	Group *string `json:"group,omitempty"`
}

// LectureKindSet is the body of PUT /api/lectures/{id}/kind. Kind "" goes
// back to the automatic kind (file name, else the model's judgement).
// Confirm: as LecturePatch (citation numbers change, P13.6).
type LectureKindSet struct {
	Kind    string `json:"kind"`
	Confirm bool   `json:"confirm,omitempty"`
}

// KindTag is a lecture's kind and number in lists that do not carry the
// whole lecture (dashboard reviews and failures): "第 3 周 · 辅导 · 第 2 节".
type KindTag struct {
	Kind  string `json:"kind,omitempty"`
	Week  int    `json:"week,omitempty"` // 0 when the lecture has no teaching week
	KindN int    `json:"kind_n,omitempty"`
	ShowN bool   `json:"show_n,omitempty"`
}

// MaterialBind is Material.Bind and the body of PATCH /api/materials/{id}
// {"bind": {...}} (the v1 body {"lecture_id": …} is still accepted).
type MaterialBind struct {
	Scope     string  `json:"scope"`      // Bind*
	Week      *int    `json:"week"`       // scope week: the teaching week (1…)
	LectureID *string `json:"lecture_id"` // scope lecture
}

// MaterialUse is one entry of Lecture.Materials: a slide file the
// lecture's stages use, and why ("第 3 周的资料").
type MaterialUse struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Pages    int    `json:"pages"`
	Via      string `json:"via"`            // Bind*: week, lecture or course
	Week     int    `json:"week,omitempty"` // via week
}
