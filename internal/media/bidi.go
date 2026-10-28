// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"strings"
	"unicode"
)

// StripBidi removes the Unicode bidirectional control characters
// (Bidi_Control: U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069)
// from s. A lecture's title comes from its file name and ends up in the
// names of the exported files, where a right-to-left override (U+202E)
// before "4pm.exe" makes Explorer show the name ending in "exe.mp4": an
// extension that is not the real one (test-media I8). Titles and export
// file names leave them out; the characters that shape text (ZWJ, ZWNJ)
// are not bidi controls and stay.
func StripBidi(s string) string {
	if !strings.ContainsFunc(s, isBidiControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isBidiControl(r) {
			return -1
		}
		return r
	}, s)
}

func isBidiControl(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }
