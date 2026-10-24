// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"strings"
	"testing"
)

// The retry feedback is appended after the unchanged user message; the
// verbatim prompts are not touched.
func TestWithRetryFeedback(t *testing.T) {
	user := FilenameRuleRequest(FilenameRuleInput{Samples: []FilenameSample{{Filename: "a.mp4", UploadedAt: "2026-10-20 09:00"}}}).User
	if got := WithRetryFeedback(user, nil); got != user {
		t.Errorf("no problems changed the message: %q", got)
	}
	got := WithRetryFeedback(user, []string{"first", " second\n  a.mp4 "})
	want := user + "\n\nYOUR PREVIOUS ANSWER WAS REJECTED. Fix the following and answer again in the same JSON format:\n- first\n- second\n  a.mp4"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if p := NotJSONProblem("bad JSON: no JSON object in \"x\"\n"); p != `The answer is not a valid JSON object (bad JSON: no JSON object in "x"). Respond with the JSON object only.` {
		t.Errorf("not-JSON problem %q", p)
	}
	if p := NotJSONProblem(strings.Repeat("x", 500)); len(p) > 300 {
		t.Errorf("not-JSON detail not cut: %d bytes", len(p))
	}
}

func TestFilenameRuleProblems(t *testing.T) {
	// A compile error or missing groups say only that.
	got := FilenameRuleProblems(FilenameRuleRejection{Regex: "(", CompileError: "error parsing regexp: missing closing ): `(`", Total: 3, Unmatched: []string{"a"}})
	if len(got) != 2 || got[0] != "Your regex was: (" || !strings.HasPrefix(got[1], "It does not compile as a Go RE2 regular expression: error parsing regexp: missing closing ): `(`.") {
		t.Errorf("compile error: %q", got)
	}
	got = FilenameRuleProblems(FilenameRuleRejection{Regex: "x", MissingGroups: []string{"month", "day"}})
	if len(got) != 2 || !strings.HasPrefix(got[1], "It lacks the required named group(s) month, day;") {
		t.Errorf("missing groups: %q", got)
	}
	// Too few matches, the unmatched names (at most 15) and the invalid dates.
	var unmatched []string
	for i := range 20 {
		unmatched = append(unmatched, fmt.Sprintf("L%02d.mp4", i))
	}
	got = FilenameRuleProblems(FilenameRuleRejection{
		Regex: "r", Matched: 2, Total: 22, MinMatchPercent: 60, Unmatched: unmatched,
		Invalid: []FilenameRuleInvalidDate{{Filename: "X 2026-13-01.mp4", Groups: []string{"year=2026", "month=13", "day=01"}, Reason: "the captured groups do not form a valid date and time"}},
	})
	if len(got) != 4 {
		t.Fatalf("%d problems: %q", len(got), got)
	}
	if got[1] != "It matches only 2 of 22 file names; at least 60% must match." {
		t.Errorf("match count: %q", got[1])
	}
	if !strings.HasPrefix(got[2], "File names it does not match:\n  L00.mp4\n  L01.mp4") || !strings.HasSuffix(got[2], "\n  L14.mp4\n  … and 5 more") {
		t.Errorf("unmatched list: %q", got[2])
	}
	if !strings.HasSuffix(got[3], "\n  X 2026-13-01.mp4 → year=2026 month=13 day=01: the captured groups do not form a valid date and time") {
		t.Errorf("invalid dates: %q", got[3])
	}
	// Enough matches, only invalid dates: no match-count line.
	got = FilenameRuleProblems(FilenameRuleRejection{Regex: "r", Matched: 3, Total: 3, MinMatchPercent: 60,
		Invalid: []FilenameRuleInvalidDate{{Filename: "a", Reason: "late"}}})
	if len(got) != 2 || !strings.HasSuffix(got[1], "\n  a: late") {
		t.Errorf("invalid only: %q", got)
	}
}
