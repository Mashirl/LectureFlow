// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
)

// TestPanoptoFilenameRuleAnswer: the fake rule with kinds compiles, matches
// every Panopto name with the right date and maps L / T to lecture /
// tutorial through its kind_map.
func TestPanoptoFilenameRuleAnswer(t *testing.T) {
	var a struct {
		Regex   string            `json:"regex"`
		KindMap map[string]string `json:"kind_map"`
	}
	if err := json.Unmarshal([]byte(llmtest.PanoptoFilenameRuleAnswer), &a); err != nil {
		t.Fatal(err)
	}
	if a.Regex != llmtest.PanoptoRegex || len(a.KindMap) != 2 {
		t.Fatalf("answer %+v", a)
	}
	rule := calendar.FilenameRule{Regex: a.Regex, KindMap: a.KindMap}
	up := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	want := []struct{ date, code, kind string }{
		{"2026-10-07T09:35", "L", "lecture"},
		{"2026-10-08T14:00", "T", "tutorial"},
		{"2026-10-14T09:35", "L", "lecture"},
		{"2026-10-15T14:00", "T", "tutorial"},
	}
	for i, n := range llmtest.PanoptoNames {
		p := calendar.Parse(rule, calendar.Sample{Filename: n, UploadedAt: up})
		if p.RecordedAt == nil || calendar.FormatRecordedAt(*p.RecordedAt) != want[i].date || p.KindCode != want[i].code || p.Kind != want[i].kind {
			t.Errorf("%s: %+v", n, p)
		}
	}
	if b := llmtest.FilenameRuleAnswer("x", nil); !json.Valid([]byte(b)) || strings.Contains(b, "kind_map") {
		t.Errorf("no kind_map: %s", b)
	}
}

// TestTitleAnswerWithKind: DefaultTitleAnswer plus the kind, served through
// Handle like any answer.
func TestTitleAnswerWithKind(t *testing.T) {
	s := llmtest.New(t)
	c := organizerClient(t, s)
	s.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind("tutorial") })
	res, err := c.Complete(context.Background(), "organizer", ctxbuild.TitleSystem, "SUMMARY:\nS.", true)
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]string
	if err := llm.ParseJSON(res.Text, &a); err != nil {
		t.Fatal(err)
	}
	var def map[string]string
	if err := json.Unmarshal([]byte(llmtest.DefaultTitleAnswer), &def); err != nil {
		t.Fatal(err)
	}
	if a["kind"] != "tutorial" || a["title_en"] != def["title_en"] || a["blurb_zh"] != def["blurb_zh"] {
		t.Errorf("answer %v", a)
	}
	if _, ok := def["kind"]; ok {
		t.Error("the default answer must stay without a kind (fixtures from before lecture kinds)")
	}
}
