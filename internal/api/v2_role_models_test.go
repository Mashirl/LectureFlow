// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

func TestModelListed(t *testing.T) {
	ds := []string{"deepseek-flash", "deepseek-v4-pro"}
	claude := []string{"claude-sonnet-4-5-20250929", "claude-opus-4-1-20250805", "claude-haiku-4-5"}
	for _, c := range []struct {
		name string
		list []string
		want bool
	}{
		{"deepseek-flash", ds, true},
		{" DeepSeek-Flash ", ds, true},
		{"deepseek-chat", ds, false}, // the retired name (NOTES, DeepSeek check)
		{"deepseek", ds, false},
		{"claude-sonnet-4-5", claude, true},        // alias of the dated id
		{"claude-sonnet-4-5-latest", claude, true}, // "-latest" alias
		{"claude-sonnet-4-5-20250929", claude, true},
		{"claude-sonnet-4", claude, false}, // "-5-20250929" is not a date
		{"claude-haiku-4-5", claude, true},
		{"claude-haiku-4", claude, false},
		{"claude-opus-4", claude, false},
		{"anything", nil, false},
	} {
		if got := modelListed(c.name, c.list); got != c.want {
			t.Errorf("modelListed(%q, %v) = %v, want %v", c.name, c.list, got, c.want)
		}
	}
}

func TestRoleModelChecksPure(t *testing.T) {
	s := func(v string) *string { return &v }
	providers := []ProviderView{
		{ID: "ds", Name: "DeepSeek", Models: []string{"deepseek-flash", "deepseek-v4-pro"}},
		{ID: "or", Name: "OpenRouter", Models: []string{}}, // never probed: no judgement
		{ID: "an", Name: "Anthropic", Models: []string{"claude-sonnet-4-5-20250929"}},
	}
	roles := []RoleView{
		{Role: model.RoleSyllabusFormat, ProviderID: s("ds"), Model: s("deepseek-chat")},
		{Role: model.RoleGlossary, ProviderID: s("ds"), Model: s("deepseek-chat")},
		{Role: model.RoleGlossaryJanitor, ProviderID: s("an"), Model: s("claude-opus-5-5")},
		{Role: model.RoleSummary, ProviderID: s("ds"), Model: s("deepseek-flash")},
		{Role: model.RoleRepair, ProviderID: s("or"), Model: s("x/old-model")},
		{Role: model.RoleTranslate, ProviderID: s("an"), Model: s("claude-sonnet-4-5")},
		{Role: model.RoleChapters, ProviderID: s("gone"), Model: s("deepseek-chat")}, // deleted provider: its own error
		{Role: model.RoleOrganizer, ProviderID: s("ds"), Model: s("deepseek-chat")},
		{Role: "unset"},
	}
	got := RoleModelChecks(roles, providers)
	if len(got) != 2 {
		t.Fatalf("checks %+v", got)
	}
	c := got[0]
	if c.Name != "role_model:DeepSeek/deepseek-chat" || c.Status != statusWarn || c.DetailInfo == nil || c.HintInfo == nil ||
		c.DetailInfo.Code != "check.role_model_unlisted" || c.HintInfo.Code != "check.hint_role_model_unlisted" {
		t.Fatalf("first check %+v", c)
	}
	roleIDs, _ := c.DetailInfo.Params["roles"].([]string)
	if strings.Join(roleIDs, ",") != "syllabus_format,glossary,organizer" || c.DetailInfo.Params["model"] != "deepseek-chat" ||
		c.DetailInfo.Params["provider"] != "DeepSeek" || c.HintInfo.Params["provider"] != "DeepSeek" {
		t.Errorf("params %+v / %+v", c.DetailInfo.Params, c.HintInfo.Params)
	}
	if c.Detail != "大纲整理、术语提取、标题与简介使用的 deepseek-chat 不在 DeepSeek 的模型列表里，可能已停用" || c.Hint == "" {
		t.Errorf("transitional texts %q / %q", c.Detail, c.Hint)
	}
	if got[1].Name != "role_model:Anthropic/claude-opus-5-5" {
		t.Errorf("second check %+v", got[1])
	}
	wantCataloged(t, "detail", got[0].DetailInfo)
	wantCataloged(t, "hint", got[0].HintInfo)
	if got := RoleModelChecks(roles[3:4], providers); len(got) != 0 {
		t.Errorf("a listed model is not reported: %+v", got)
	}
}

func TestWithRoleModelChecksBeforeDisk(t *testing.T) {
	extra := []SystemCheck{{Name: "role_model:A/x"}}
	got := WithRoleModelChecks([]SystemCheck{{Name: "api:A"}, {Name: "disk"}}, extra)
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "api:A,role_model:A/x,disk" {
		t.Errorf("order %v", names)
	}
	if got := WithRoleModelChecks([]SystemCheck{{Name: "api:A"}}, nil); len(got) != 1 {
		t.Errorf("no extra: %v", got)
	}
}

// GET /api/system: the check follows the roles and the probed list live
// (no recheck), and is only given once the provider has a list.
func TestSystemRoleModelCheck(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	roleModel := func() []SystemCheck {
		t.Helper()
		var sys System
		e.call("GET", "/api/system", nil, http.StatusOK, &sys)
		var out []SystemCheck
		for i, c := range sys.Checks {
			if strings.HasPrefix(c.Name, "role_model:") {
				out = append(out, c)
				if i == len(sys.Checks)-1 || sys.Checks[len(sys.Checks)-1].Name != "disk" {
					t.Errorf("role-model rows go before the disk check: %v", sys.Checks)
				}
			}
		}
		return out
	}
	for _, role := range []string{model.RoleSummary, model.RoleRepair} {
		e.call("PUT", "/api/settings/roles/"+role, RoleInput{ProviderID: "deepseek", Model: "deepseek-chat"}, http.StatusOK, nil)
	}
	if got := roleModel(); len(got) != 0 {
		t.Fatalf("without a probed list nothing is judged: %+v", got)
	}
	e.llm.SetModels([]string{"deepseek-flash", "deepseek-v4-pro"})
	e.call("POST", "/api/providers/deepseek/models", nil, http.StatusOK, nil)
	got := roleModel()
	if len(got) != 1 || got[0].Status != statusWarn || got[0].DetailInfo == nil || got[0].DetailInfo.Code != "check.role_model_unlisted" {
		t.Fatalf("after the probe %+v", got)
	}
	roles, _ := got[0].DetailInfo.Params["roles"].([]any)
	var ids []string
	for _, r := range roles {
		ids = append(ids, r.(string))
	}
	if strings.Join(ids, ",") != "summary,repair" { // the other roles use the listed deepseek-flash (config default)
		t.Errorf("roles %v", ids)
	}
	if got[0].DetailInfo.Params["model"] != "deepseek-chat" || got[0].DetailInfo.Params["provider"] != "deepseek" {
		t.Errorf("params %+v", got[0].DetailInfo.Params)
	}
	// both roles moved to a listed model: the row is gone without a recheck
	for _, role := range []string{model.RoleSummary, model.RoleRepair} {
		e.call("PUT", "/api/settings/roles/"+role, RoleInput{ProviderID: "deepseek", Model: "deepseek-flash"}, http.StatusOK, nil)
	}
	if got := roleModel(); len(got) != 0 {
		t.Fatalf("after the change %+v", got)
	}
}
