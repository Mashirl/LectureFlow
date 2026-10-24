// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// ---------------------------------------------------------------------------
// v2 (SPEC-v2.md). FROZEN by the lead at the start of v2; changes only by the
// lead, recorded in NOTES.md ("v2 冻结与裁决").
// ---------------------------------------------------------------------------

// Term tiers (G1 term.tier).
const (
	TierEnforce   = "enforce"   // translation mandatory, checked by qa, whisper prompt candidate
	TierReference = "reference" // preferred in the technical sense only; not checked, not prompted
)

// Origins of a term's zh (G6 rule 3, term.zh_source).
const (
	ZhSourceExtractor = "extractor" // glossary_seed / material_terms
	ZhSourceJanitor   = "janitor"   // fix_zh of the glossary janitor
	ZhSourceUser      = "user"      // typed by the user; the janitor never changes it
)

// Roles added by v2. role_setting's CHECK accepts all three from migration
// 005; Roles (model.go) lists glossary_janitor from phase 1, chapters and
// organizer are added to it in phase 2.
const (
	RoleGlossaryJanitor = "glossary_janitor" // G3.1
	RoleChapters        = "chapters"         // P7
	RoleOrganizer       = "organizer"        // P13.2: titles, blurbs, filename rules
)

// StageGlossaryReview is the course task of G3.1 (scope = course). It is not
// part of the course stage chain of glossary_seed: its dependencies are
// computed by state.Deps (see NOTES.md "v2 冻结与裁决").
const StageGlossaryReview = "glossary_review"

// WaitGlossaryReview: asr waits for the course's glossary_review (G3.1).
// UI: "等待整理术语表".
const WaitGlossaryReview = "glossary_review"

// Janitor actions (G3.3).
const (
	JanitorEnforce   = "enforce"
	JanitorReference = "reference"
	JanitorDisable   = "disable"
	JanitorMerge     = "merge"
	JanitorFixZh     = "fix_zh"
)

// KV keys added in v2.
const (
	// janitor_report:{course_id}:{glossary_version} → JSON JanitorReport (G3.3).
	KVJanitorReportPrefix = "janitor_report:"
	// janitor_report_latest:{course_id} → the glossary_version of the latest report.
	KVJanitorLatestPrefix = "janitor_report_latest:"
	// janitor_last_run:{course_id} → JSON JanitorReport of the latest run,
	// including runs that changed nothing (time, model, rejected; Items may
	// be empty). The course header (G3.5) reads it; undo uses the versioned report.
	KVJanitorLastRunPrefix = "janitor_last_run:"
	// glossary_review_dirty:{course_id} → "1" while a running glossary_review
	// must run once more after it finishes (new terms arrived meanwhile).
	KVGlossaryReviewDirtyPrefix = "glossary_review_dirty:"
	// v2.common_word_backfilled → "1" once common_word was computed for the
	// terms that existed before migration 005.
	KVCommonWordBackfill = "v2.common_word_backfilled"
	// v2.role_default:{role} → "1" while the role holds the default that
	// appendix D1 wrote and the user has not changed it (SetRole clears it).
	// Nothing is recorded when D1 found no default to write.
	KVRoleDefaultPrefix = "v2.role_default:"
)

// Prompt versions (G5.5). Written into the LLMSnapshot of the stage when a
// lecture is launched or a snapshot is replaced with the current settings.
// A snapshot without prompt_version is version 1. A change never makes a
// stage stale; the rerun decision (v1.1 A4.3) shows "提示词 v1 → v2".
const (
	PromptVersionSummary   = 2
	PromptVersionRepair    = 2
	PromptVersionTranslate = 2
)

// PromptVersionOf returns the current prompt version of an LLM lecture stage
// (0 for other stages).
func PromptVersionOf(stage string) int {
	switch stage {
	case StageSummary:
		return PromptVersionSummary
	case StageRepair:
		return PromptVersionRepair
	case StageTranslate:
		return PromptVersionTranslate
	}
	return 0
}

// StaleReasonJanitor is the stale_reason of a job made stale by a janitor
// run (G3.1): "术语表已整理（vN → vM）". IsGlossaryReason accepts it, so it
// follows later glossary edits like StaleReasonGlossary does.
func StaleReasonJanitor(from *int, to int) string {
	if from == nil {
		return staleJanitorPrefix + "（现为 v" + itoa(to) + "）"
	}
	return staleJanitorPrefix + "（v" + itoa(*from) + " → v" + itoa(to) + "）"
}

const staleJanitorPrefix = "术语表已整理"

// TermState is the part of a term the janitor can change (G3.3), used for
// the before/after columns of the report and for undo (G3.5).
type TermState struct {
	Tier     string `json:"tier"`
	Enabled  bool   `json:"enabled"`
	Zh       string `json:"zh"`
	ZhSource string `json:"zh_source"`
}

// StateOf returns the janitor-visible state of t.
func StateOf(t Term) TermState {
	return TermState{Tier: t.Tier, Enabled: t.Enabled, Zh: t.Zh, ZhSource: t.ZhSource}
}

// JanitorItem is one applied action in a JanitorReport.
type JanitorItem struct {
	TermID string    `json:"term_id"`
	En     string    `json:"en"`
	Action string    `json:"action"`            // Janitor* constant
	Into   string    `json:"into,omitempty"`    // merge target term id
	IntoEn string    `json:"into_en,omitempty"` // merge target en (display)
	Reason string    `json:"reason"`
	Before TermState `json:"before"`
	After  TermState `json:"after"`
	// Undone is set by undo (G3.5): the term was restored to Before and locked.
	Undone   bool   `json:"undone,omitempty"`
	UndoneAt string `json:"undone_at,omitempty"`
}

// JanitorRejected is an action dropped by validation (G3.3), kept in the report.
type JanitorRejected struct {
	TermID string `json:"term_id"`
	Action string `json:"action"`
	Reason string `json:"reason"` // the model's reason
	Why    string `json:"why"`    // why it was dropped (Chinese, for the report UI)
	// WhyCode is the coded reason (v2 P14.3, e.g. janitor.rejected.locked);
	// reports stored before it fall back to Why.
	WhyCode string `json:"why_code,omitempty"`
}

// JanitorReport is kv janitor_report:{course_id}:{glossary_version} (G3.3).
// Version is the glossary_version after the actions were applied (the run
// bumps it by exactly 1; a run that changed nothing keeps the version and
// still stores its report).
type JanitorReport struct {
	CourseID     string `json:"course_id"`
	FromVersion  int    `json:"from_version"`
	Version      int    `json:"version"`
	CreatedAt    string `json:"created_at"` // UTC RFC3339
	ProviderName string `json:"provider_name,omitempty"`
	Model        string `json:"model,omitempty"`
	// Fallback: the G2 deterministic rule produced this report (no janitor
	// role configured, or the janitor failed: appendix D2).
	Fallback bool              `json:"fallback,omitempty"`
	Note     string            `json:"note,omitempty"`
	NoteInfo *ErrorInfo        `json:"note_info,omitempty"` // coded Note (v2 P14.3)
	Items    []JanitorItem     `json:"items"`
	Rejected []JanitorRejected `json:"rejected,omitempty"`
}
