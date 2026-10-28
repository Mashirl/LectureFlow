// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import "strings"

// ASRPostprocess is job.stats.asr_timing.postprocess: one object per step the
// asr worker ran between decoding and the sentence split (redecode,
// 2026-10-27, NOTES "决定（redecode：worker-builder，2026-10-27）"). The
// counters describe the run, never the transcript; a later step adds its own
// field next to Redecode.
type ASRPostprocess struct {
	// Redecode: worker/asr.py decoding degenerate windows once more (nil:
	// the worker did not run that step, e.g. OpenVINO or MLX).
	Redecode *ASRRedecode `json:"redecode,omitempty"`
	// TextRepair (textrepair-int): the statistical repair of degenerate text
	// before the sentence split (worker/textrepair.py), run by every worker,
	// and by a "修复断句" run (ASRModeTextRepair); nil: not reported (older
	// workers, fake results).
	TextRepair *ASRTextRepair `json:"textrepair,omitempty"`
	// Split (colab-obs-core): "极速模式" cut the lecture at silences into
	// parts transcribed at once (worker/colab_remote.py transcribe_split,
	// result.json timing.postprocess.split); nil: transcribed whole. The
	// lecture page says "本节用极速模式".
	Split *ASRSplit `json:"split,omitempty"`
}

// ASRSplit is timing.postprocess.split of result.json: Parts the lecture
// was cut into and the cuts (ms from the start, ascending, Parts - 1 of
// them).
type ASRSplit struct {
	Parts  int     `json:"parts"`
	CutsMS []int64 `json:"cuts_ms"`
}

// MaxASRSplit is the most parts a lecture is cut into (colab_remote.py
// MAX_SPLIT).
const MaxASRSplit = 4

// Checked is s as job.stats keep it: nil for nil, for fewer than two or
// more than MaxASRSplit parts, and for cuts that are not Parts - 1
// ascending positive times (a broken worker); cuts_ms never null.
func (s *ASRSplit) Checked() *ASRSplit {
	if s == nil || s.Parts < 2 || s.Parts > MaxASRSplit || len(s.CutsMS) != s.Parts-1 {
		return nil
	}
	prev := int64(0)
	for _, c := range s.CutsMS {
		if c <= prev {
			return nil
		}
		prev = c
	}
	return &ASRSplit{Parts: s.Parts, CutsMS: append([]int64{}, s.CutsMS...)}
}

// ASRRedecode counts the faster-whisper decoding windows (30 s each) of a
// lecture: Windows in all; Unpunctuated / Uppercase of them in a degenerate
// style (worker/asr_common.py window_style); Redecoded of those decoded once
// more (a degenerate window with no normal text in the lecture to prompt it
// is not); Accepted of those replaced by the second decode (the others keep
// the first); RedecodeMS the time of the second decodes, the VAD they need
// included. Skipped: the switch (KVASRRedecode) was off for this run, so the
// degenerate windows were counted but none was decoded again. A run resumed
// from a checkpoint reports the whole lecture.
type ASRRedecode struct {
	Skipped      bool  `json:"skipped,omitempty"`
	Windows      int   `json:"windows"`
	Unpunctuated int   `json:"unpunctuated"`
	Uppercase    int   `json:"uppercase"`
	Redecoded    int   `json:"redecoded"`
	Accepted     int   `json:"accepted"`
	RedecodeMS   int64 `json:"redecode_ms"`
}

// KVASRRedecode is the switch "修复退化的转写窗口" (settings page, 转写;
// user request 2026-10-27): ASRRedecodeOff turns the second decode of
// degenerate windows off; absent and any other value mean on (the default),
// so only an explicit off turns it off (as colab.memory_mode). Read when a
// faster-whisper job.json is written (local worker, self-test and Colab);
// not part of the asr input hash, so switching it never makes a lecture
// stale, it applies to the transcriptions after it.
const (
	KVASRRedecode  = "asr.redecode"
	ASRRedecodeOff = "off"
	ASRRedecodeOn  = "on"
)

// RedecodeEnabled is the switch for the kv value v (found false: never set);
// "off" counts in any case and with surrounding space, as colab.memory_mode.
func RedecodeEnabled(v string, found bool) bool {
	return !found || !strings.EqualFold(strings.TrimSpace(v), ASRRedecodeOff)
}
