// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
	"lectureflow/internal/subs"
)

// v2 (SPEC-v2.md) API types. FROZEN by the lead; changes only by the lead.

// JanitorStatus is Course.Janitor (G3.5): the header of the glossary area.
type JanitorStatus struct {
	// Configured: the glossary_janitor role has a usable provider and model.
	// When false, "整理术语表" applies the G2 rule at once (no model call).
	Configured bool `json:"configured"`
	// Running: the course's glossary_review job is queued or running.
	Running bool `json:"running"`
	// Last* describe the latest report (empty when there is none).
	LastAt       string `json:"last_at,omitempty"`    // UTC RFC3339
	LastModel    string `json:"last_model,omitempty"` // "Anthropic / claude-opus-…"; "常用词规则" for a fallback report
	LastVersion  int    `json:"last_version,omitempty"`
	LastChanges  int    `json:"last_changes,omitempty"` // len(items) not undone
	LastFallback bool   `json:"last_fallback,omitempty"`
}

// GlossaryReviewResult is POST /api/courses/{id}/glossary/review.
//   - janitor configured: the glossary_review job is (re)queued → Queued=true (HTTP 202);
//   - not configured: the G2 rule runs at once → Report is the fallback report (HTTP 200).
type GlossaryReviewResult struct {
	Queued bool                 `json:"queued"`
	Report *model.JanitorReport `json:"report,omitempty"`
}

// JanitorUndoInput is the body of POST /api/courses/{id}/glossary/report/undo.
type JanitorUndoInput struct {
	Version int    `json:"version"` // report version (JanitorReport.Version)
	TermID  string `json:"term_id"`
}

// TermPatch is the body of PATCH /api/terms/{id} (v2 G1: every user change
// locks the term; a zh change sets zh_source = user). Locked can only be
// set to false ("解锁"); it then changes nothing else and does not bump
// glossary_version.
type TermPatch struct {
	Zh      *string `json:"zh,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	Tier    *string `json:"tier,omitempty"`
	Locked  *bool   `json:"locked,omitempty"`
}

// ---------------------------------------------------------------------------
// Phase 2: the player (SPEC-v2 P2–P8). FROZEN by the lead.
// ---------------------------------------------------------------------------

// Cue is one subtitle cue of GET /api/lectures/{id}/cues (P8.2), which
// returns a JSON array of them in time order (subs.SegmentLecture of the
// lecture's sentences; English is always sentence.text, P4.3).
type Cue = subs.Cue

// Chapter is one chapter of P7. StartMS is the start of sentence StartIdx.
type Chapter struct {
	StartIdx int    `json:"start_idx"`
	StartMS  int64  `json:"start_ms"`
	TitleEn  string `json:"title_en"`
	TitleZh  string `json:"title_zh"`
}

// ChaptersView is GET /api/lectures/{id}/chapters (200 with no chapters,
// stale false and running false while none was ever generated and none is
// running; power management P4, lead ruling 9.4). POST starts the generation and answers
// 202 with Running=true; completion is announced by SSE changed
// {kind:"chapters", id: lecture_id}.
type ChaptersView struct {
	Chapters    []Chapter `json:"chapters"`
	GeneratedAt string    `json:"generated_at,omitempty"`
	Model       string    `json:"model,omitempty"` // "DeepSeek / deepseek-flash"
	// Stale: the summary or a sentence text changed since generation (P7):
	// the button says "章节可能已过时，重新生成"; never regenerated automatically.
	Stale   bool   `json:"stale"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"` // last generation failed (validation failed twice)
}

// ResumeView is GET/PUT /api/lectures/{id}/resume (P6.4). GET answers
// {position_ms: 0, saved_at: ""} when nothing was saved.
type ResumeView struct {
	PositionMS int64  `json:"position_ms"`
	SavedAt    string `json:"saved_at"`
}

// StreamInfo is Lecture.Stream: the playback file of P8.1.
type StreamInfo struct {
	// Status: "ready" (stream.mp4 exists), "generating" (background job for an
	// old lecture), "pending" (not generated yet; GET /media starts it),
	// "audio_only" (the source has no video), "no_space" (appendix D6: under
	// 10 GB free), "failed".
	Status     string  `json:"status"`
	Transcoded bool    `json:"transcoded"` // re-encoded to H.264/AAC ("已转码")
	Progress   float64 `json:"progress,omitempty"`
	Error      string  `json:"error,omitempty"`
	SizeBytes  int64   `json:"size_bytes,omitempty"`
}

// ---------------------------------------------------------------------------
// Phase 2: calendar, filename rules, tariff, errors, UI language. FROZEN by
// the lead.
// ---------------------------------------------------------------------------

// FilenameRule is course.filename_rule (P13.3.1).
type FilenameRule = calendar.FilenameRule

// FilenameRuleView is GET /api/courses/{id}/filename-rule (P13.3.3).
type FilenameRuleView struct {
	Rule     *FilenameRule        `json:"rule"`     // the saved rule, null = none
	Preview  []FilenamePreviewRow `json:"preview"`  // the saved rule over the course's lectures
	Proposal *FilenameProposal    `json:"proposal"` // generated, waiting for the user; null = none
	Task     *StageView           `json:"task,omitempty"`
	// KindMap is the kind_map of the saved rule (code captured by the rule's
	// "kind" group → lecture kind id; lecture kinds). It is stored in
	// course.filename_rule (Rule.kind_map); repeated here next to the rule.
	KindMap map[string]string `json:"kind_map,omitempty"`
}

// FilenameProposal is a generated rule with its preview (P13.3.3).
type FilenameProposal struct {
	Rule        FilenameRule         `json:"rule"`
	Explanation string               `json:"explanation"`
	CreatedAt   string               `json:"created_at"`
	Preview     []FilenamePreviewRow `json:"preview"`
	Matched     int                  `json:"matched"`
	Total       int                  `json:"total"`
	OK          bool                 `json:"ok"`
	Problems    []string             `json:"problems,omitempty"`
	// ProblemsInfo: the same problems coded (P14.3, calendar.Problem codes;
	// a calendar.file_invalid problem has params.reason, the row's
	// ErrorInfo). Problems stay as the Chinese texts (transitional).
	ProblemsInfo []model.ErrorInfo `json:"problems_info,omitempty"`
	KindMap      map[string]string `json:"kind_map,omitempty"` // lecture kinds: as FilenameRuleView.KindMap
}

// FilenamePreviewInput is POST /api/courses/{id}/filename-rule/preview
// (live preview while editing, P13.3.3) and the body of PUT (apply).
type FilenamePreviewInput struct {
	Regex string `json:"regex"`
	// Confirm: apply even though lecture numbers change after an earlier
	// citation export (P13.6); PUT only.
	Confirm bool `json:"confirm,omitempty"`
	// KindMap: code → lecture kind id for the rule's optional "kind" group
	// ({"L": "lecture", "T": "tutorial"}). Empty codes are dropped; a kind
	// not in the catalog is 400 kind.unknown on PUT (the preview ignores it).
	KindMap map[string]string `json:"kind_map,omitempty"`
}

// FilenamePreviewResult answers the preview: the deterministic validation.
type FilenamePreviewResult struct {
	Preview  []FilenamePreviewRow `json:"preview"`
	Matched  int                  `json:"matched"`
	Total    int                  `json:"total"`
	OK       bool                 `json:"ok"`
	Problems []string             `json:"problems,omitempty"`
	// ProblemsInfo: see FilenameProposal.ProblemsInfo.
	ProblemsInfo []model.ErrorInfo `json:"problems_info,omitempty"`
}

// CoursePatch is PATCH /api/courses/{id} (v1.1 name/code plus v2 P13.4).
type CoursePatch struct {
	Name      *string   `json:"name,omitempty"`
	Code      *string   `json:"code,omitempty"`       // "" clears
	TermStart *string   `json:"term_start,omitempty"` // any day of week 1, aligned to Monday; "" clears
	SkipWeeks *[]string `json:"skip_weeks,omitempty"`
	Confirm   bool      `json:"confirm,omitempty"` // see RenumberConflict
	// Numbering: Numbering* (types_kinds.go); display only, never asks to
	// confirm a renumbering.
	Numbering *string `json:"numbering,omitempty"`
}

// LecturePatch is PATCH /api/lectures/{id} (v1.1 title plus v2 P13).
// Editing title or title_zh makes title_source "user"; recorded_at makes
// recorded_at_source "user" ("" clears it back to automatic).
type LecturePatch struct {
	// Seq is the v1.1 "修改序号"; from v2 seq is computed from recorded_at
	// (P13.2) and a request with seq is rejected (400).
	Seq        *int    `json:"seq,omitempty"`
	Title      *string `json:"title,omitempty"`
	TitleZh    *string `json:"title_zh,omitempty"`
	RecordedAt *string `json:"recorded_at,omitempty"` // local "YYYY-MM-DDTHH:MM" or "YYYY-MM-DD"
	Confirm    bool    `json:"confirm,omitempty"`
}

// RenumberConflict is the 409 body when a change would renumber lectures of
// a course whose citations were exported (P13.6):
// {"error":{"code":"calendar.renumber"},"changes":[...]}; resend with confirm.
type RenumberConflict struct {
	Error   model.ErrorInfo  `json:"error"`
	Changes []RenumberChange `json:"changes"`
}

type RenumberChange struct {
	LectureID string `json:"lecture_id"`
	Title     string `json:"title"`
	From      string `json:"from"` // citation label (Lecture.cite_label): "W02L1", "W??-01JM8Q"
	To        string `json:"to"`
}

// TitleRegenerateInput is POST /api/lectures/{id}/title (P13.5 重新生成):
// Force is required to overwrite a user title (otherwise 409
// {"error":{"code":"title.user_set"}}).
type TitleRegenerateInput struct {
	Force bool `json:"force,omitempty"`
}

// TariffState is the current peak state of a provider with a tariff (P12.4),
// pushed with SSE "resources" every minute. Times are UTC RFC3339; the
// front end shows local time first and Beijing time second.
type TariffState struct {
	Peak            bool        `json:"peak"`
	NextChange      string      `json:"next_change,omitempty"`
	Timezone        string      `json:"timezone"`         // the tariff's zone, e.g. Asia/Shanghai
	MissingHolidays bool        `json:"missing_holidays"` // "节假日数据缺失"
	TodayPeak       [][2]string `json:"today_peak"`       // UTC intervals overlapping the server's local day
	OffpeakOnly     bool        `json:"offpeak_only"`
	Multiplier      float64     `json:"offpeak_multiplier"`
}

// HolidaysView is GET/PUT /api/settings/holidays (P12.2: view and edit).
type HolidaysView struct {
	Years  map[string][]string `json:"years"`  // "2026": ["2026-01-01", ...]
	Edited bool                `json:"edited"` // user data replaces the built-in file
	Source string              `json:"source,omitempty"`
	// SourceInfo is Source as a code (P14.3: holidays.source_cn, params.years
	// = the years of the built-in data); Detail is the built-in note.
	SourceInfo *model.ErrorInfo `json:"source_info,omitempty"`
}

// UISettings is GET/PUT /api/settings/ui (P14.6, P14.7).
type UISettings struct {
	Locale    string `json:"locale"`     // BCP 47, "" = not chosen yet (front end detects)
	ZhDisplay string `json:"zh_display"` // "simplified" | "tw" | "hk"; "" = default for the locale
}

// ErrorBody is every JSON error response from v2 on (P14.3):
// {"error": {"code": "...", "params": {...}, "detail": "..."}}.
type ErrorBody struct {
	Error model.ErrorInfo `json:"error"`
}

// FilenamePreviewRow is one row of a filename-rule preview table (P13.3.3),
// built from calendar.Parsed: Status is Parsed.Status() (ok | unmatched |
// invalid | suspicious; the front end translates it), RecordedAt is
// calendar.FormatRecordedAt ("YYYY-MM-DDTHH:MM", local), Error is the
// validation detail text (shown under "查看详情").
type FilenamePreviewRow struct {
	Filename   string  `json:"filename"`
	Status     string  `json:"status"`
	RecordedAt *string `json:"recorded_at"`
	HasTime    bool    `json:"has_time"`
	Title      string  `json:"title,omitempty"`
	Suspicious bool    `json:"suspicious,omitempty"`
	Error      string  `json:"error,omitempty"`
	// ErrorInfo: why the row failed, coded (P14.3, calendar.Problem codes;
	// detail = Error, the Chinese text kept for compatibility).
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
	// Lecture kinds: what the rule's "kind" group captured, and the kind it
	// maps to through kind_map ("" when kind_map has no entry for that code).
	KindCode string `json:"kind_code,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Past: the date is more than 30 days before the term start, so the
	// lecture is numbered as a past lecture (lead ruling 1.1; P13.3.2 rule 4
	// no longer rejects it).
	Past bool `json:"past,omitempty"`
}
