// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"testing"
	"unicode"
)

// test-media I8: the title a file-name rule takes from a name leaves out
// bidirectional control characters.
func TestParseTitleDropsBidi(t *testing.T) {
	p := Parse(FilenameRule{Regex: `^(?P<title>.+?) (?P<month>\d+)-(?P<day>\d+)\.mp4$`},
		Sample{Filename: "\u202eStream_Ciphers\u2069 10-7.mp4", UploadedAt: at(2026, 10, 20, 21, 0)})
	if p.Status() != StatusOK || p.Title != "Stream Ciphers" {
		t.Fatalf("%+v", p)
	}
}

// dropBidiControl drops exactly the Bidi_Control characters.
func TestDropBidiControlMatchesUnicode(t *testing.T) {
	for r := rune(0); r <= 0x2FFFF; r++ {
		if dropped := dropBidiControl(r) < 0; dropped != unicode.Is(unicode.Bidi_Control, r) {
			t.Errorf("U+%04X: dropped %v", r, dropped)
		}
	}
}
