// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"testing"
)

func strp(s string) *string { return &s }

func TestThinkingLevels(t *testing.T) {
	for _, tc := range []struct {
		v           *string
		of, level   string
		valid, isOn bool
	}{
		{nil, ThinkingOff, ThinkingOff, true, false},
		{strp(""), ThinkingOff, ThinkingOff, true, false},
		{strp("off"), ThinkingOff, ThinkingOff, true, false},
		{strp("low"), ThinkingLow, ThinkingLow, true, true},
		{strp("high"), ThinkingHigh, ThinkingHigh, true, true},
		{strp("max"), ThinkingMax, ThinkingMax, true, true},
		{strp("medium"), "medium", ThinkingOff, false, false},
		{strp("HIGH"), "HIGH", ThinkingOff, false, false},
	} {
		p := RoleParams{Thinking: tc.v}
		if got := ThinkingOf(p); got != tc.of {
			t.Errorf("ThinkingOf(%v) = %q, want %q", tc.v, got, tc.of)
		}
		if got := ThinkingLevel(p); got != tc.level {
			t.Errorf("ThinkingLevel(%v) = %q, want %q", tc.v, got, tc.level)
		}
		if got := ValidThinking(ThinkingOf(p)); got != tc.valid {
			t.Errorf("ValidThinking(%q) = %v", ThinkingOf(p), got)
		}
		if got := ThinkingOn(p); got != tc.isOn {
			t.Errorf("ThinkingOn(%v) = %v", tc.v, got)
		}
	}
	if len(ThinkingLevels) != 4 || ThinkingLevels[0] != ThinkingOff {
		t.Errorf("ThinkingLevels %v", ThinkingLevels)
	}
}

// Params without thinking encode exactly as before the option existed.
func TestRoleParamsJSONWithoutThinking(t *testing.T) {
	temp, w := 0.3, 3
	b, _ := json.Marshal(RoleParams{Temperature: &temp, Window: &w})
	if string(b) != `{"temperature":0.3,"window":3}` {
		t.Errorf("params %s", b)
	}
	snap := LLMSnapshot{ProviderID: "p1", ProviderName: "DeepSeek", Model: "deepseek-flash", Params: RoleParams{Temperature: &temp}, PromptVersion: 2}
	if got := MarshalSnapshot(snap); got != `{"provider_id":"p1","provider_name":"DeepSeek","model":"deepseek-flash","params":{"temperature":0.3},"prompt_version":2}` {
		t.Errorf("snapshot %s", got)
	}
	snap.Params.Thinking = strp(ThinkingHigh)
	if got := MarshalSnapshot(snap); got != `{"provider_id":"p1","provider_name":"DeepSeek","model":"deepseek-flash","params":{"temperature":0.3,"thinking":"high"},"prompt_version":2}` {
		t.Errorf("snapshot with thinking %s", got)
	}
	var back RoleParams
	if err := json.Unmarshal([]byte(`{"temperature":0.3,"thinking":"max"}`), &back); err != nil || ThinkingOf(back) != ThinkingMax {
		t.Errorf("decode %+v %v", back, err)
	}
}

// The rerun decision (A4.3) compares thinking by level: absent, "" and
// "off" are the same (a snapshot written before the option equals a
// current setting with thinking off); any change of level differs.
func TestSameSnapshotThinking(t *testing.T) {
	temp, w := 0.3, 3
	mk := func(th *string) string {
		return MarshalSnapshot(LLMSnapshot{ProviderID: "p1", ProviderName: "DeepSeek", Model: "deepseek-flash",
			Params: RoleParams{Temperature: &temp, Window: &w, Thinking: th}, PromptVersion: 2})
	}
	old := `{"provider_id":"p1","provider_name":"DeepSeek","model":"deepseek-flash","params":{"temperature":0.3,"window":3},"prompt_version":2}`
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{old, mk(nil), true},
		{old, mk(strp("off")), true},
		{old, mk(strp("")), true},
		{old, mk(strp("medium")), true}, // not a level: runs as off
		{old, mk(strp("low")), false},
		{mk(strp("low")), mk(strp("high")), false},
		{mk(strp("high")), mk(strp("max")), false},
		{mk(strp("high")), mk(strp("high")), true},
		{mk(strp("max")), old, false},
	} {
		for _, stage := range []string{StageSummary, StageRepair, StageTranslate} {
			if got := SameSnapshot(stage, tc.a, tc.b); got != tc.same {
				t.Errorf("%s: SameSnapshot(%s, %s) = %v, want %v", stage, tc.a, tc.b, got, tc.same)
			}
		}
	}
}

// Thinking is not an input of any stage: the only snapshot parameter that
// feeds an input hash (translate's window, SnapshotWindow) ignores it.
func TestSnapshotWindowIgnoresThinking(t *testing.T) {
	w := 2
	for _, th := range []*string{nil, strp("off"), strp("low"), strp("high"), strp("max")} {
		raw := MarshalSnapshot(LLMSnapshot{ProviderID: "p", Model: "m", Params: RoleParams{Window: &w, Thinking: th}})
		if got := SnapshotWindow(raw); got != 2 {
			t.Errorf("thinking %v: window %d", th, got)
		}
	}
}
