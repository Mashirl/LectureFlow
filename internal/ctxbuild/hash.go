// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// hashParts returns the hex SHA-256 of the parts, each encoded as
// "{byte length}:{bytes}" so that different part lists never collide.
// The first part is a domain tag naming the hash.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func asrTexts(lec LectureSnapshot) []string {
	out := make([]string, len(lec.Sentences))
	for i, s := range lec.Sentences {
		out[i] = s.ASRText
	}
	return out
}

// SummaryInputHash is the summary stage input_hash (SPEC §4.4): every
// sentence.asr_text + the slides text used in the request (LectureSlides).
func SummaryInputHash(lec LectureSnapshot) string {
	parts := append([]string{"summary", strconv.Itoa(len(lec.Sentences))}, asrTexts(lec)...)
	parts = append(parts, LectureSlides(lec))
	return hashParts(parts...)
}

// RepairInputHash is the repair stage input_hash (SPEC §4.4): every
// sentence.asr_text + summary + the enabled glossary as GlossaryHashText
// (en, zh and tier of each enabled term in en_key order). The prompt text
// is not part of it (lead ruling 9): for an all-enforce glossary the hash is
// the v1 one, and a tier change makes repair stale.
func RepairInputHash(lec LectureSnapshot) string {
	parts := append([]string{"repair", strconv.Itoa(len(lec.Sentences))}, asrTexts(lec)...)
	parts = append(parts, lec.Summary, GlossaryHashText(lec.Terms))
	return hashParts(parts...)
}

// ASRParams are the ASR settings of the asr input_hash (SPEC-v1.1 A1:
// model, compute_type, language). ComputeType is the configured value of
// the job's ASR snapshot (e.g. "auto"), not the per-run resolution, so a
// transient nvidia-smi failure never changes the hash. The device is not
// part of the hash (A1 lists only these three; lead ruling 3).
type ASRParams struct {
	Model       string
	ComputeType string
	Language    string
}

// ASRInputHash is the asr stage input_hash of SPEC-v1.1 A1 (asr jobs with
// stats.hash_version = 2): wav file hash (hex SHA-256 computed by the
// caller) + ASR config. The prompt is no longer part of it: a glossary
// change never makes asr stale.
func ASRInputHash(wavSHA256 string, p ASRParams) string {
	return hashParts("asr", wavSHA256, p.Model, p.ComputeType, p.Language)
}

// ASRInputHashV3 is the asr input_hash of SPEC-v2 I6 (revising A1; asr jobs
// with stats.hash_version = 3, phase 3b freeze): the wav file hash and the
// language only. Device, backend, model and precision are not part of it,
// so changing any of them never makes asr stale (A4); a rerun with another
// device goes through the A4.3 decision instead.
func ASRInputHashV3(wavSHA256, language string) string {
	return hashParts("asr", "v3", wavSHA256, language)
}
