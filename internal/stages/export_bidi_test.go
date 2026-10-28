// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"strings"
	"testing"
	"unicode"
)

// test-media I8: no bidirectional control character in an export's file
// name (a right-to-left override would make it show another extension)
// nor in a title the model wrote.

func hasBidi(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) })
}

func TestSafeFileNameDropsBidi(t *testing.T) {
	for in, want := range map[string]string{
		"W3 \u202etxt.exe":         "W3 txt.exe",
		"\u202e":                   "lecture",
		"\u2066Lec 3\u2069 \u200f": "Lec 3",
		"CON\u202e":                "CON_",
		"第3讲\u200e 缓存":             "第3讲 缓存",
	} {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, name := range ExportNames("W3-1", "Lecture \u202etxt.exe") {
		if hasBidi(name) {
			t.Errorf("export name %q", name)
		}
	}
	if name := ExportDownloadName("W3-1", "evil\u202e4pm.exe", "W3-1 old.bilingual.md"); hasBidi(name) || !strings.HasSuffix(name, ".bilingual.md") {
		t.Errorf("download name %q", name)
	}
}

func TestTitleAnswerDropsBidi(t *testing.T) {
	a := titleAnswer{TitleEn: "Stream \u202eCiphers", TitleZh: "\u2067流密码\u2069"}
	_ = checkTitleAnswer(&a) // the limits do not matter here
	if a.TitleEn != "Stream Ciphers" || a.TitleZh != "流密码" {
		t.Fatalf("titles %q %q", a.TitleEn, a.TitleZh)
	}
}
