// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import "strings"

// WER of the self-test (SPEC-v2 I9). FROZEN signature; body: task D.

// WER is the word error rate of hyp against ref, measured like the 3a
// benchmark (docs/benchmarks/intel-openvino.md §2, spikes/openvino/common.py
// word_errors): lower case; apostrophes removed (it's → its); every other
// character that is not a letter or digit is a space; word-level edit
// distance divided by the number of reference words. refWords is that
// number; with refWords 0 the WER is 0 and meaningless (no reference).
// Must match jiwer on the 3a golden cases.
//
// "Letter or digit" is exactly 3a's regular expression [^0-9a-z]+ applied
// after lower-casing: ASCII only, so a non-ASCII letter splits a word
// ("Vigenère" → "vigen re") on both sides alike (task D, NOTES.md).
func WER(ref, hyp string) (wer float64, refWords int) {
	r, h := werWords(ref), werWords(hyp)
	if len(r) == 0 {
		return 0, 0
	}
	return float64(editDistance(r, h)) / float64(len(r)), len(r)
}

// werWords is 3a's normalize(text).split().
func werWords(s string) []string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("’", "", "'", "").Replace(s)
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			out = append(out, byte(r))
		} else {
			out = append(out, ' ')
		}
	}
	return strings.Fields(string(out))
}

// editDistance is the word-level Levenshtein distance (substitution,
// deletion and insertion each cost 1), as jiwer's S + D + I.
func editDistance(r, h []string) int {
	prev := make([]int, len(h)+1)
	cur := make([]int, len(h)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(r); i++ {
		cur[0] = i
		for j := 1; j <= len(h); j++ {
			sub := prev[j-1]
			if r[i-1] != h[j-1] {
				sub++
			}
			cur[j] = min(sub, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(h)]
}
