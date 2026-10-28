// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import "strings"

// The statistical repair of degenerate ASR text (textrepair-int; user
// decision 2026-10-27 03:30, NOTES "决定（textrepair：worker-builder…）" and
// "决定（textrepair-int：worker-builder…）"). Every transcription runs it
// before the sentence split (worker/textrepair.py); a lecture transcribed
// before it can be repaired without transcribing it again ("修复断句"): an
// asr rerun with text_only, whose job carries ASRModeTextRepair.

// ASRModeTextRepair is JobStats.ASRMode of an asr job that repairs the
// lecture's stored transcript instead of transcribing it: the scheduler runs
// it in the CPU pool, the asr stage builds textrepair.py's job.json from the
// sentence rows and stores the result like a transcription (§4.6: the
// downstream stages run again). "" is a transcription.
const ASRModeTextRepair = "textrepair"

// ActionRerunTextOnly ("修复断句") is offered in a done or stale asr stage's
// allowed_actions when the stored transcript has word timestamps, the
// lecture is English and its text is degenerate enough to be worth repairing
// (state.WithTextRepair); the front end sends it as the asr rerun with
// text_only true (POST /api/lectures/{id}/stages/asr/rerun).
const ActionRerunTextOnly = "rerun_text_only"

// ASRTextRepair is job.stats.asr_timing.postprocess.textrepair: what the
// repair found and did (worker/textrepair.py's stats). Skipped is "language"
// when the lecture is not English (nothing was changed). Segments are the
// degenerate spans it worked on (UnpunctuatedSegments of them stretches of
// 25 words or more without punctuation, CapsSegments all-caps spans);
// SentenceEndsAdded / CommasAdded the marks it appended; WordsRecased the
// words whose case it changed; TrainSentences the sentence ends of the
// lecture's normal text it learned from. KeptSentences, SentencesIn and
// SentencesOut: a "修复断句" run only (sentences a person reviewed, kept as
// they were; the sentences before and after). MS is the repair's time.
type ASRTextRepair struct {
	Skipped              string `json:"skipped,omitempty"`
	Version              int    `json:"version,omitempty"`
	Words                int    `json:"words"`
	Segments             int    `json:"segments"`
	UnpunctuatedSegments int    `json:"unpunctuated_segments"`
	CapsSegments         int    `json:"caps_segments"`
	SentenceEndsAdded    int    `json:"sentence_ends_added"`
	CommasAdded          int    `json:"commas_added"`
	WordsRecased         int    `json:"words_recased"`
	TrainSentences       int    `json:"train_sentences"`
	KeptSentences        int    `json:"kept_sentences,omitempty"`
	SentencesIn          int    `json:"sentences_in,omitempty"`
	SentencesOut         int    `json:"sentences_out,omitempty"`
	MS                   int64  `json:"ms"`
}

// IsTextRepairJob reports whether j is a "修复断句" run: a lecture's asr job
// with ASRModeTextRepair, unless the user then chose "改用 Colab 处理" for it
// (RemotePin: that is a transcription on Colab, V14.4).
func IsTextRepairJob(j Job) bool {
	return j.Scope == ScopeLecture && j.Stage == StageASR && j.Stats.ASRMode == ASRModeTextRepair && !j.Stats.RemotePin
}

// TextRepairLanguage reports whether the repair runs for a lecture language:
// English only (lead ruling 2026-10-27; worker/textrepair.py enabled_for:
// surrounding space and case ignored, "" is not English).
func TextRepairLanguage(lang string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "en")
}
