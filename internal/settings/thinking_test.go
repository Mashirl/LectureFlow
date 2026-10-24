// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"errors"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// Per-role "深度思考" (.claude/thinking-brief.md, T1).

func TestSupportsThinking(t *testing.T) {
	for _, tc := range []struct {
		p    model.Provider
		want bool
	}{
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com"}, true},
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com/v1"}, true},
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://proxy.example.com/v1", BalanceAPI: model.BalanceDeepSeek}, true},
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}, false},
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://openrouter.ai/api/v1"}, false},
		{model.Provider{Type: model.ProviderOpenAI, BaseURL: "http://127.0.0.1:11434/v1"}, false},
		{model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com"}, true},
		{model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://api.deepseek.com/anthropic"}, true},
		{model.Provider{Type: "other", BaseURL: "https://api.deepseek.com"}, false},
	} {
		if got := SupportsThinking(tc.p); got != tc.want {
			t.Errorf("SupportsThinking(%s %s %s) = %v, want %v", tc.p.Type, tc.p.BaseURL, tc.p.BalanceAPI, got, tc.want)
		}
	}
}

func TestResolveParamsThinking(t *testing.T) {
	for _, tc := range []struct {
		in   *string
		want string // "" = absent
	}{
		{nil, ""}, {sp(""), ""}, {sp("off"), ""}, {sp("medium"), ""},
		{sp("low"), "low"}, {sp("high"), "high"}, {sp("max"), "max"},
	} {
		for _, role := range []string{model.RoleSummary, model.RoleTranslate} {
			got := ResolveParams(role, model.RoleParams{Thinking: tc.in})
			if (got.Thinking == nil) != (tc.want == "") || got.Thinking != nil && *got.Thinking != tc.want {
				t.Errorf("%s %v: thinking %v, want %q", role, tc.in, got.Thinking, tc.want)
			}
			if got.Temperature == nil || (role == model.RoleTranslate) != (got.Window != nil) {
				t.Errorf("%s: %+v", role, got)
			}
		}
	}
}

// SetRole: the four levels; off is stored as absent (the row is what it
// was before the option); other values are thinking_invalid; a level other
// than off for a provider without support is thinking_unsupported.
func TestSetRoleThinking(t *testing.T) {
	s, _ := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	other := create(t, s, "OpenRouter", model.ProviderOpenAI, "https://openrouter.ai/api/v1", fakeKey)

	for _, tc := range []struct {
		p      model.Provider
		role   string
		in     *string
		stored string
	}{
		{ds, model.RoleSummary, nil, `{}`},
		{ds, model.RoleSummary, sp("off"), `{}`},
		{ds, model.RoleSummary, sp(""), `{}`},
		{ds, model.RoleSummary, sp("low"), `{"thinking":"low"}`},
		{ds, model.RoleTranslate, sp("high"), `{"window":2,"thinking":"high"}`},
		{claude, model.RoleRepair, sp("max"), `{"thinking":"max"}`},
		{other, model.RoleRepair, sp("off"), `{}`}, // off is always allowed
		{other, model.RoleRepair, nil, `{}`},
	} {
		params := model.RoleParams{Thinking: tc.in}
		if tc.role == model.RoleTranslate {
			params.Window = ip(2)
		}
		if err := s.SetRole(bg(), tc.role, tc.p.ID, "m", params); err != nil {
			t.Fatalf("%s %s %v: %v", tc.p.Name, tc.role, tc.in, err)
		}
		if got := s.Roles()[tc.role].Params; got != tc.stored {
			t.Errorf("%s %s %v: stored %s, want %s", tc.p.Name, tc.role, tc.in, got, tc.stored)
		}
		target, err := s.RoleTarget(tc.role)
		if err != nil {
			t.Fatal(err)
		}
		want := model.ThinkingLevel(model.RoleParams{Thinking: tc.in})
		if got := model.ThinkingOf(target.Params); got != want || (want == model.ThinkingOff) != (target.Params.Thinking == nil) {
			t.Errorf("%s %s %v: target thinking %v", tc.p.Name, tc.role, tc.in, target.Params.Thinking)
		}
	}

	for _, tc := range []struct {
		p      model.Provider
		in     string
		code   string
		params map[string]any
	}{
		{ds, "medium", CodeThinkingInvalid, map[string]any{"value": "medium"}},
		{ds, "HIGH", CodeThinkingInvalid, map[string]any{"value": "HIGH"}},
		{ds, " low", CodeThinkingInvalid, map[string]any{"value": " low"}},
		{other, "low", CodeThinkingUnsupported, map[string]any{"provider": "OpenRouter"}},
		{other, "max", CodeThinkingUnsupported, map[string]any{"provider": "OpenRouter"}},
		{other, "nonsense", CodeThinkingInvalid, map[string]any{"value": "nonsense"}},
	} {
		before := s.Roles()[model.RoleRepair].Params
		err := s.SetRole(bg(), model.RoleRepair, tc.p.ID, "m2", model.RoleParams{Thinking: sp(tc.in)})
		if !errors.Is(err, ErrInvalid) || !hasCode(err, tc.code, tc.params) {
			t.Errorf("%s %q: %v, want %s %v", tc.p.Name, tc.in, err, tc.code, tc.params)
		}
		if strings.Contains(err.Error(), fakeKey) {
			t.Errorf("error leaks the key: %v", err)
		}
		if after := s.Roles()[model.RoleRepair]; after.Params != before || *after.Model == "m2" {
			t.Errorf("%s %q: refused SetRole changed the row: %+v", tc.p.Name, tc.in, after)
		}
	}
	// The Chinese texts (the detail) name the value / the provider.
	err := s.SetRole(bg(), model.RoleRepair, other.ID, "m", model.RoleParams{Thinking: sp("high")})
	if err.Error() != "提供商「OpenRouter」不支持深度思考，请选择“关闭”" {
		t.Errorf("unsupported text %q", err.Error())
	}
	err = s.SetRole(bg(), model.RoleRepair, ds.ID, "m", model.RoleParams{Thinking: sp("medium")})
	if err.Error() != "深度思考的强度「medium」无效，只能是 off、low、high 或 max" {
		t.Errorf("invalid text %q", err.Error())
	}
}

// Launch snapshots (A4.1) carry the level when it is on and are unchanged
// when it is off; changing the level makes the snapshot differ (the rerun
// dialog of A4.3 asks).
func TestCurrentSnapshotsThinking(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	s.SetRole(bg(), model.RoleSummary, p.ID, "deepseek-flash", model.RoleParams{Thinking: sp("off")})
	s.SetRole(bg(), model.RoleRepair, p.ID, "deepseek-flash", model.RoleParams{})
	s.SetRole(bg(), model.RoleTranslate, p.ID, "deepseek-flash", model.RoleParams{Window: ip(2)})
	off, err := s.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"provider_id":"` + p.ID + `","provider_name":"DeepSeek","model":"deepseek-flash","params":{"temperature":0.2},"prompt_version":2}`; off[model.StageSummary] != want {
		t.Errorf("summary off:\n got %s\nwant %s", off[model.StageSummary], want)
	}
	if err := s.SetRole(bg(), model.RoleTranslate, p.ID, "deepseek-flash", model.RoleParams{Window: ip(2), Thinking: sp("high")}); err != nil {
		t.Fatal(err)
	}
	on, _ := s.CurrentSnapshots()
	if want := `{"provider_id":"` + p.ID + `","provider_name":"DeepSeek","model":"deepseek-flash","params":{"temperature":0.3,"window":2,"thinking":"high"},"prompt_version":2}`; on[model.StageTranslate] != want {
		t.Errorf("translate high:\n got %s\nwant %s", on[model.StageTranslate], want)
	}
	if model.SameSnapshot(model.StageTranslate, off[model.StageTranslate], on[model.StageTranslate]) {
		t.Error("a thinking change must differ for the rerun decision")
	}
	if !model.SameSnapshot(model.StageSummary, off[model.StageSummary], on[model.StageSummary]) ||
		!model.SameSnapshot(model.StageRepair, off[model.StageRepair], on[model.StageRepair]) {
		t.Error("the other roles are unchanged")
	}
}
