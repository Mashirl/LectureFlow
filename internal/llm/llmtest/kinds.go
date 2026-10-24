// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest

import (
	"encoding/json"
	"fmt"
)

// Fake answers with lecture kinds (user request 2026-10-23, lead freeze
// .claude/kinds-brief.md; K2). The defaults (DefaultTitleAnswer,
// DefaultFilenameRuleAnswer, testdata/llm/title.json and filename_rule.json)
// carry no kind, so tests and fixtures written before lecture kinds are
// unchanged; tests that need kinds answer with these through Handle.

// PanoptoNames are lecture-capture file names as Panopto writes them: the
// course code, the activity code (L lecture, T tutorial) with its group and
// sequence number, then the weekday, date, time, zone and year.
var PanoptoNames = []string{
	"CSC1002-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4",
	"CSC1002-T01-01 Thu Oct 08 14-00-00 BST 2026.mp4",
	"CSC1002-L01-02 Wed Oct 14 09-35-00 BST 2026.mp4",
	"CSC1002-T01-02 Thu Oct 15 14-00-00 BST 2026.mp4",
}

// PanoptoRegex is the rule of PanoptoFilenameRuleAnswer: the activity code
// is the kind group; group and sequence numbers are not captured.
const PanoptoRegex = `^[A-Z]{3}\d{4}-(?P<kind>[A-Z]+)\d{2}-\d{2} \w{3} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3,4} (?P<year>\d{4})`

// PanoptoKindMap is the kind_map of PanoptoFilenameRuleAnswer.
var PanoptoKindMap = map[string]string{"L": "lecture", "T": "tutorial"}

// PanoptoFilenameRuleAnswer is a filename_rule answer (prompt version 2)
// for PanoptoNames: PanoptoRegex with kind_map L → lecture, T → tutorial.
var PanoptoFilenameRuleAnswer = FilenameRuleAnswer(PanoptoRegex, PanoptoKindMap)

// FilenameRuleAnswer is a filename_rule answer with regex and kind_map
// (nil = no kind_map key).
func FilenameRuleAnswer(regex string, kindMap map[string]string) string {
	a := map[string]any{"regex": regex, "explanation": "The names hold the activity code, then the recording date and time."}
	if kindMap != nil {
		a["kind_map"] = kindMap
	}
	b, err := json.Marshal(a)
	if err != nil {
		panic(fmt.Sprintf("llmtest: filename rule answer: %v", err))
	}
	return string(b)
}

// TitleAnswerWithKind is DefaultTitleAnswer with "kind": kind (prompt
// version 2), e.g. TitleAnswerWithKind("tutorial").
func TitleAnswerWithKind(kind string) string {
	var a map[string]any
	if err := json.Unmarshal([]byte(DefaultTitleAnswer), &a); err != nil {
		panic(fmt.Sprintf("llmtest: DefaultTitleAnswer: %v", err))
	}
	a["kind"] = kind
	b, err := json.Marshal(a)
	if err != nil {
		panic(fmt.Sprintf("llmtest: title answer: %v", err))
	}
	return string(b)
}
