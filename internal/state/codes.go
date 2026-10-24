// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"encoding/json"
	"maps"

	"lectureflow/internal/model"
)

// Codes of the job errors this package writes (v2 P14.3); the Chinese texts
// are in testdata/api/error_codes_backend.json.
const (
	// CodeInterrupted: the job was running when the program stopped (§4.7
	// startup recovery). job.error stores model.ErrInterruptedByRestart as
	// plain text; ParseJobError maps it to this code (detail = the text).
	CodeInterrupted = "job.interrupted"
)

// Codes of the notes the API adds to a stage's hover card (kind note; the
// API's StageView.note_info). They describe job states this package knows.
const (
	CodeNoteDownloadingModel = "stage.downloading_model" // asr's first run downloads the model (§17.2)
	CodeNoteBudgetPaused     = "stage.budget_paused"     // the daily budget is used up (A5.2)
	CodeNoteBudgetUnpriced   = "stage.budget_unpriced"   // a budget applies and n of today's requests had no price (D3): it may be low; never blocks
	CodeNoteIncremental      = "translate.incremental"   // an incremental translate run (§7.3)
	CodeNoteMediaDeleted     = "prep.media_deleted"      // keep_original_media = false deleted the original
	// SPEC-v2 I7 (phase 3b freeze): the OpenVINO worker's STATUS lines
	// (model.ASRStatusPreparingModel / ASRStatusCompilingModel).
	CodeNotePreparingModel = "stage.preparing_model" // first use: downloading and converting the model
	CodeNoteCompilingModel = "stage.compiling_model" // first use on this GPU / NPU: compiling (NPU about 4 min, once)
	// CodeNoteLegacy: a note written without a code (before v2 P14.3);
	// params text is the note as stored.
	CodeNoteLegacy = "legacy"
)

// ErrorCodes lists the codes this package defines.
var ErrorCodes = []string{CodeInterrupted, CodeNoteDownloadingModel, CodeNoteBudgetPaused, CodeNoteBudgetUnpriced,
	CodeNoteIncremental, CodeNoteMediaDeleted, CodeNoteLegacy,
	CodeNotePreparingModel, CodeNoteCompilingModel, // SPEC-v2 I7 (phase 3b)
	CodeRenumberedUpgrade, // past_rule.go: a note on the course page (lecture kinds review)
}

// JoinNote is the coded form of a note made of parts (v2 P14.3): the first
// part, with the others in params.extra; nil without parts. A part that has
// extra parts of its own is flattened (NoteParts).
func JoinNote(parts ...model.ErrorInfo) *model.ErrorInfo {
	var flat []model.ErrorInfo
	for i := range parts {
		flat = append(flat, NoteParts(&parts[i])...)
	}
	if len(flat) == 0 {
		return nil
	}
	info := flat[0]
	if len(flat) > 1 {
		info.Params = maps.Clone(info.Params)
		if info.Params == nil {
			info.Params = map[string]any{}
		}
		info.Params["extra"] = flat[1:]
	}
	return &info
}

// NoteParts splits a coded note into its main part (without extra) and its
// extra parts, in order; params.extra may be []model.ErrorInfo (as written)
// or decoded JSON (as read back from job.stats or a report).
func NoteParts(info *model.ErrorInfo) []model.ErrorInfo {
	if info == nil {
		return nil
	}
	main := *info
	raw, ok := main.Params["extra"]
	if !ok {
		return []model.ErrorInfo{main}
	}
	main.Params = maps.Clone(main.Params)
	delete(main.Params, "extra")
	if len(main.Params) == 0 {
		main.Params = nil
	}
	var extra []model.ErrorInfo
	switch x := raw.(type) {
	case []model.ErrorInfo:
		extra = x
	default:
		if b, err := json.Marshal(x); err == nil {
			_ = json.Unmarshal(b, &extra)
		}
	}
	return append([]model.ErrorInfo{main}, extra...)
}

// IsInterrupted reports whether job.error is the §4.7 recovery error: the
// plain text (as Recover writes it) or the coded form written by the
// phase-2 build before the gate ruling.
func IsInterrupted(raw *string) bool {
	if raw == nil {
		return false
	}
	info, text := ParseJobError(*raw)
	if info != nil {
		return info.Code == CodeInterrupted
	}
	return text == model.ErrInterruptedByRestart
}
