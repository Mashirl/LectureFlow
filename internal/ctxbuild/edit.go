// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import "strings"

// Levenshtein returns the character-level (rune) edit distance between a and b:
// the minimum number of single-rune insertions, deletions and substitutions.
func Levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// EditExceedsHalf reports whether after differs from before by more than 50%
// of before's length in runes (SPEC §6.4: such patches are downgraded to
// "decide").
func EditExceedsHalf(before, after string) bool {
	return 2*Levenshtein(before, after) > len([]rune(before))
}

// StripMarks removes the ⟦ ⟧ low-confidence marks (SPEC §13.4) that a model
// may have copied into a corrected sentence.
func StripMarks(s string) string {
	return strings.NewReplacer("⟦", "", "⟧", "").Replace(s)
}
