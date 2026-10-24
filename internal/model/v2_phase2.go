// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// v2 phase 2 (SPEC-v2 part 2). FROZEN by the lead; changes only by the lead,
// recorded in NOTES.md ("v2 第二阶段冻结与裁决").
// ---------------------------------------------------------------------------

// Word is one word timestamp of P4.2 (sentence.words JSON). S and E are ms
// from the start of the lecture. Same JSON as subs.Word and the worker output.
type Word struct {
	W string `json:"w"`
	S int64  `json:"s"`
	E int64  `json:"e"`
}

// Sources of lecture.recorded_at (P13.2, P13.3.4).
const (
	RecordedAtNone     = "none"
	RecordedAtMetadata = "metadata"
	RecordedAtFilename = "filename"
	RecordedAtUser     = "user"
)

// Sources of lecture.title / title_zh (P13.2, P13.5).
const (
	TitleSourceFilename = "filename"
	TitleSourceLLM      = "llm"
	TitleSourceUser     = "user"
)

// Auxiliary tasks (P13.5 "辅助任务", P7, P8.1, P13.3.2). They are jobs, but
// not pipeline stages: they never appear in progress bars, never take part in
// dependencies, invalidation or display_status, and a failure only shows a
// retryable hint. A failed auxiliary task is not retried automatically.
const (
	StageTitle        = "title"         // scope lecture, role organizer (P13.5)
	StageChapters     = "chapters"      // scope lecture, role chapters (P7)
	StageStream       = "stream"        // scope lecture, CPU: stream.mp4 for lectures prepared before v2 (P8.1)
	StageFilenameRule = "filename_rule" // scope course, role organizer (P13.3.2)
)

// AuxStages lists the auxiliary task stages.
var AuxStages = []string{StageTitle, StageChapters, StageStream, StageFilenameRule}

// IsAuxStage reports whether stage is an auxiliary task.
func IsAuxStage(stage string) bool {
	for _, s := range AuxStages {
		if s == stage {
			return true
		}
	}
	return false
}

// AuxScope returns the job scope of an auxiliary stage ("" if not one).
func AuxScope(stage string) string {
	switch stage {
	case StageTitle, StageChapters, StageStream:
		return ScopeLecture
	case StageFilenameRule:
		return ScopeCourse
	}
	return ""
}

// AuxRole returns the LLM role of an auxiliary stage ("" for stream).
func AuxRole(stage string) string {
	switch stage {
	case StageTitle, StageFilenameRule:
		return RoleOrganizer
	case StageChapters:
		return RoleChapters
	}
	return ""
}

// WaitOffpeakPrefix: the provider dispatches only off-peak (P12.6);
// "offpeak:{HH:MM}" with the local time the off-peak period starts.
// UI: "等待空闲时段（本地时间 04:00 开始）".
const WaitOffpeakPrefix = "offpeak:"

func WaitOffpeak(localHHMM string) string { return WaitOffpeakPrefix + localHHMM }

// KV keys added in phase 2.
const (
	KVResumePrefix      = "resume:"                 // resume:{lecture_id} → JSON {position_ms, saved_at} (P6.4)
	KVChaptersPrefix    = "chapters:"               // chapters:{lecture_id} → JSON Chapters (P7)
	KVStreamPrefix      = "stream:"                 // stream:{lecture_id} → JSON StreamState (P8.1)
	KVUILocale          = "ui.locale"               // BCP 47 code of the UI language (P14.7)
	KVUIZhDisplay       = "ui.zh_display"           // "simplified" | "tw" | "hk" (P14.6)
	KVCiteExportPrefix  = "cite_exported:"          // cite_exported:{course_id} → time of the latest citation export (P13.6)
	KVFilenameProposal  = "filename_rule_proposal:" // filename_rule_proposal:{course_id} → JSON FilenameProposal awaiting the user (P13.3.3)
	KVFilenameSamples   = "filename_rule_samples:"  // filename_rule_samples:{course_id} → JSON []string of original filenames already used for a rule
	KVFilenameUndo      = "filename_rule_undo:"     // filename_rule_undo:{course_id} → JSON state before an automatic apply (P13.3.3 首次例外)
	KVHolidaysCN        = "tariff.holidays_cn"      // user-edited holiday data (P12.2); absent = the embedded file
	KVStaleReasonJSON   = "v2.stale_reason_json"    // "1" once old stale_reason texts were converted (P14.3)
	KVOriginalFilenames = "v2.original_filenames"   // "1" once original_filename was backfilled
)

// Chapters is kv chapters:{lecture_id} (P7).
type Chapters struct {
	Chapters    []ChapterEntry `json:"chapters"`
	InputHash   string         `json:"input_hash"` // sentence texts + summary at generation
	GeneratedAt string         `json:"generated_at"`
	Provider    string         `json:"provider,omitempty"`
	Model       string         `json:"model,omitempty"`
}

// ChapterEntry is one chapter as the model returns it (P7).
type ChapterEntry struct {
	StartIdx int    `json:"start_idx"`
	TitleEn  string `json:"title_en"`
	TitleZh  string `json:"title_zh"`
}

// StreamState is kv stream:{lecture_id} (P8.1). Status values are those of
// api.StreamInfo.Status.
type StreamState struct {
	Status     string `json:"status"`
	Transcoded bool   `json:"transcoded"`
	Error      string `json:"error,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// StreamFile is the playback file inside a lecture directory (P8.1).
const StreamFile = "stream.mp4"

// FilenameProposal is kv filename_rule_proposal:{course_id} (P13.3.3): a
// generated rule that passed validation, waiting for 应用 / 编辑 / 忽略.
type FilenameProposal struct {
	Rule        json.RawMessage `json:"rule"`        // calendar.FilenameRule
	Explanation string          `json:"explanation"` // the model's one-sentence explanation
	CreatedAt   string          `json:"created_at"`
}

// ---------------------------------------------------------------------------
// P14.3: back-end texts become codes + params. stale_reason is stored as
// JSON {"code": ..., "params": {...}}; texts written before v2 are converted
// once at startup, unknown ones to {"code":"legacy","params":{"text":...}}.
// ---------------------------------------------------------------------------

// Stale reason codes (P14.3). The front end translates "stale.reason.{code}".
const (
	StaleCodeGlossary = "glossary_updated" // params from (absent = not recorded), to
	StaleCodeJanitor  = "glossary_janitor" // params from, to
	StaleCodeSummary  = "summary_updated"
	StaleCodeMaterial = "material_changed" // params filename (may be absent)
	StaleCodeInput    = "input_changed"
	StaleCodeLegacy   = "legacy" // params text
)

// StaleReasonCode is the decoded stale_reason.
type StaleReasonCode struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// StaleReasonJSON encodes a stale_reason.
func StaleReasonJSON(code string, params map[string]any) string {
	b, _ := json.Marshal(StaleReasonCode{Code: code, Params: params})
	return string(b)
}

// ParseStaleReason decodes a stale_reason: JSON as written by
// StaleReasonJSON, or (not yet converted) plain text as legacy.
func ParseStaleReason(raw string) StaleReasonCode {
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "{") {
		var r StaleReasonCode
		if json.Unmarshal([]byte(t), &r) == nil && r.Code != "" {
			return r
		}
	}
	return StaleReasonCode{Code: StaleCodeLegacy, Params: map[string]any{"text": raw}}
}

// ErrorInfo is the language-neutral form of a back-end error (P14.3): HTTP
// error bodies, job errors and notifications carry it; the front end shows
// t("errors."+Code, Params) and offers Detail under "查看详情".
type ErrorInfo struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	Detail string         `json:"detail,omitempty"`
}

// CodedError is an error with an ErrorInfo. Stages return it (wrapped or
// not); the scheduler stores job.error as the JSON of its ErrorInfo
// (Detail = the wrapped error's text), and plain errors as legacy text.
type CodedError struct {
	Info ErrorInfo
	Err  error
}

func (e *CodedError) Error() string {
	if e.Err != nil {
		return e.Info.Code + ": " + e.Err.Error()
	}
	return e.Info.Code
}

func (e *CodedError) Unwrap() error { return e.Err }

// Errorf builds a CodedError.
func Errorf(code string, params map[string]any, err error) error {
	return &CodedError{Info: ErrorInfo{Code: code, Params: params}, Err: err}
}
