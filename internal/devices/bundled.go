// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/config"
)

// The bundled sample lecture (SPEC-v3 V4.3, SPEC-v2 I9 "没有时使用随程序附带
// 的一段公开授权的短音频"): config.Config.SampleDir holds these files
// (assets/sample in the repository, resources/sample when packaged).
const (
	BundledAudioFile     = config.SampleAudioFile // 16 kHz mono FLAC, about one minute
	BundledReferenceFile = "reference.txt"        // its verbatim text (the WER reference)
)

// bom is the UTF-8 byte order mark an editor may put before the reference.
var bom = string(rune(0xFEFF))

// BundledClip returns the bundled sample's audio file and its reference
// text. ok is false when sampleDir is empty or the audio file is missing;
// a missing or unreadable reference gives ref "" (the self-test then has
// no WER). The self-test uses the whole sample (no PlanClip).
func BundledClip(sampleDir string) (audio, ref string, ok bool) {
	if sampleDir == "" {
		return "", "", false
	}
	audio = filepath.Join(sampleDir, BundledAudioFile)
	if fi, err := os.Stat(audio); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return "", "", false
	}
	if abs, err := filepath.Abs(audio); err == nil {
		audio = abs
	}
	if b, err := os.ReadFile(filepath.Join(sampleDir, BundledReferenceFile)); err == nil {
		ref = strings.TrimSpace(strings.TrimPrefix(string(b), bom))
	}
	return audio, ref, true
}
