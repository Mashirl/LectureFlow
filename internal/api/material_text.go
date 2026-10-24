// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import "strings"

// TextWarningIncomplete is Material.TextWarning when the slide file's text
// could probably not be extracted in full (SPEC-v3 V6): too many of its pages
// came out empty (scans without a text layer, pictures of slides, PDFs whose
// fonts pypdf cannot map).
const TextWarningIncomplete = "text_incomplete"

// emptyPages counts the pages whose text is empty after trimming white space.
func emptyPages(pages []string) int {
	n := 0
	for _, p := range pages {
		if strings.TrimSpace(p) == "" {
			n++
		}
	}
	return n
}

// textIncomplete is the threshold of the V6 warning: at least half the
// pages are empty, or at least 3 pages and at least 20 % of them are. One or
// two empty pages in a longer deck (title picture, full-page figure, section
// divider) are normal and do not warn. No pages: nothing to judge.
func textIncomplete(empty, total int) bool {
	if total <= 0 || empty <= 0 {
		return false
	}
	return 2*empty >= total || (empty >= 3 && 5*empty >= total)
}

// materialTextWarning is Material.TextWarning for the extracted page texts
// (nil until extracted: no warning).
func materialTextWarning(pages []string) string {
	if textIncomplete(emptyPages(pages), len(pages)) {
		return TextWarningIncomplete
	}
	return ""
}
