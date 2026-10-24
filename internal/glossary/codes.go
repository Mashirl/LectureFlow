// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package glossary

import "lectureflow/internal/model"

// Codes of the janitor report notes of fallback runs (JanitorReport.NoteInfo,
// v2 P14.3, kind note); the Chinese texts are in
// testdata/api/error_codes_backend.json.
const (
	CodeNoteFallbackManual = "janitor.fallback_manual" // "整理术语表" without a configured janitor
	CodeNoteFallbackMerge  = "janitor.fallback_merge"  // MergeTerms without a configured janitor
	// CodeNoteFallbackFailed: the janitor failed for good (D2); params cause
	// = the code of its error (absent when it had none), Detail its text.
	CodeNoteFallbackFailed = "janitor.fallback_after_failure"
)

// ErrorCodes lists the codes this package defines.
var ErrorCodes = []string{CodeNoteFallbackManual, CodeNoteFallbackMerge, CodeNoteFallbackFailed}

// NoteInfo is a report note's coded form.
func NoteInfo(code string, params map[string]any) *model.ErrorInfo {
	return &model.ErrorInfo{Code: code, Params: params}
}

// NoteInfoFallbackFailed is the coded form of NoteFallbackFailed(reason):
// causeCode is the code of the janitor's error ("" when it had none), reason
// its text (the detail).
func NoteInfoFallbackFailed(causeCode, reason string) *model.ErrorInfo {
	info := &model.ErrorInfo{Code: CodeNoteFallbackFailed, Detail: reason}
	if causeCode != "" {
		info.Params = map[string]any{"cause": causeCode}
	}
	return info
}
