// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import "testing"

// test-media I8: the bidirectional controls go, nothing else.
func TestStripBidi(t *testing.T) {
	for _, r := range []rune{0x061C, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069} {
		if got := StripBidi("a" + string(r) + "b"); got != "ab" {
			t.Errorf("U+%04X: %q", r, got)
		}
	}
	for in, want := range map[string]string{
		"Lecture \u202etxt.exe":  "Lecture txt.exe",
		"\u2067\u202e\u200f":     "",
		"plain title":            "plain title",
		"第3讲 缓存":                 "第3讲 缓存",
		"محاضرة 3":               "محاضرة 3",
		"क्षत्रिय":               "क्षत्रिय",
		"a\u200db\u200cc\u200bd": "a\u200db\u200cc\u200bd", // ZWJ, ZWNJ, ZWSP are no bidi controls
		"👩\u200d🏫 emoji \ufeff":  "👩\u200d🏫 emoji \ufeff",
	} {
		if got := StripBidi(in); got != want {
			t.Errorf("StripBidi(%q) = %q, want %q", in, got, want)
		}
	}
}
