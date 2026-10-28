// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import "testing"

// test-media I8: a platform's title loses its bidirectional controls,
// in the lecture title and in the file name made from it.
func TestTitleDropsBidi(t *testing.T) {
	if got := cleanTitle(" Week 3 \u202etxt.exe \u2069"); got != "Week 3 txt.exe" {
		t.Errorf("cleanTitle = %q", got)
	}
	for in, want := range map[string]string{
		"Week 3 \u202etxt.exe": "Week 3 txt.exe",
		"\u202e\u200f":         "lecture",
		"nul\u200e.x":          "nul_.x",
	} {
		if got := fileStem(in); got != want {
			t.Errorf("fileStem(%q) = %q, want %q", in, got, want)
		}
	}
}
