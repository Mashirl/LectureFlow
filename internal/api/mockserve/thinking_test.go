// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Per-role thinking in the mock (.claude/thinking-brief.md): DeepSeek and
// Anthropic support it, the other OpenAI-compatible provider (OpenRouter)
// does not; roles carry their level ("off" when unset; the fixture's
// 术语提取 is "low"); SetRole's checks and codes; the level shows in the
// model-choice diff (label and params).
func TestMockThinking(t *testing.T) {
	srv := testServer(t)
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	want := map[string]bool{pidDeepSeek: true, pidAnthropic: true, pidOpenRouter: false}
	for id, w := range want {
		if p := findProvider(s.Providers, id); p == nil || p.SupportsThinking != w {
			t.Errorf("provider %s: %+v, want supports_thinking %v", id, p, w)
		}
	}
	for _, r := range s.Roles {
		w := model.ThinkingOff
		if r.Role == model.RoleGlossary {
			w = model.ThinkingLow
		}
		if r.Thinking != w {
			t.Errorf("role %s: thinking %q, want %q", r.Role, r.Thinking, w)
		}
	}

	u := srv.URL + "/api/settings/roles/"
	codeOf := func(body any, role string) model.ErrorInfo {
		t.Helper()
		status, b := rawCall(t, "PUT", u+role, body)
		var e api.ErrorBody
		if status != http.StatusBadRequest || json.Unmarshal(b, &e) != nil {
			t.Fatalf("PUT %s: %d %s", role, status, b)
		}
		return e.Error
	}
	bogus, low, high, off := "extreme", model.ThinkingLow, model.ThinkingHigh, model.ThinkingOff
	if e := codeOf(api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-chat", Params: model.RoleParams{Thinking: &bogus}}, model.RoleRepair); e.Code != settings.CodeThinkingInvalid || e.Params["value"] != bogus {
		t.Errorf("invalid level: %+v", e)
	}
	if e := codeOf(api.RoleInput{ProviderID: pidOpenRouter, Model: "deepseek/deepseek-chat", Params: model.RoleParams{Thinking: &low}}, model.RoleSummary); e.Code != settings.CodeThinkingUnsupported || e.Params["provider"] != "OpenRouter" {
		t.Errorf("unsupported: %+v", e)
	}
	// off is fine on any provider
	doJSON(t, "PUT", u+"summary", api.RoleInput{ProviderID: pidOpenRouter, Model: "deepseek/deepseek-chat", Params: model.RoleParams{Thinking: &off}}, http.StatusOK, &s)
	if sm := findRole(s.Roles, model.RoleSummary); sm.Thinking != model.ThinkingOff || *sm.ProviderID != pidOpenRouter {
		t.Errorf("summary off on OpenRouter: %+v", sm)
	}

	// repair: the same model and temperature, thinking high → a new label
	// (the lecture's snapshot differs only in thinking)
	doJSON(t, "PUT", u+"repair", api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-chat", Params: model.RoleParams{Temperature: fp(0.1), Thinking: &high}}, http.StatusOK, &s)
	if rp := findRole(s.Roles, model.RoleRepair); rp.Thinking != model.ThinkingHigh || rp.Temperature != 0.1 {
		t.Fatalf("repair after PUT: %+v", rp)
	}
	var nm api.NeedsModelChoice
	code, b := rawCall(t, "POST", srv.URL+"/api/lectures/"+lecDone+"/stages/repair/rerun", nil)
	if code != http.StatusConflict || json.Unmarshal(b, &nm) != nil || len(nm.Diff) == 0 {
		t.Fatalf("rerun repair: %d %s", code, b)
	}
	d := nm.Diff[0]
	if d.Stage != model.StageRepair || d.Current.Label != "DeepSeek / deepseek-chat · 深度思考 高" || d.Snapshot.Label != "DeepSeek / deepseek-chat · 温度 0.1" {
		t.Errorf("diff labels %q → %q", d.Snapshot.Label, d.Current.Label)
	}
	if d.Current.Params == nil || d.Current.Params.Thinking == nil || *d.Current.Params.Thinking != high ||
		d.Snapshot.Params == nil || d.Snapshot.Params.Thinking != nil || d.Snapshot.Params.Temperature == nil || *d.Snapshot.Params.Temperature != 0.1 {
		t.Errorf("diff params %+v → %+v", d.Snapshot.Params, d.Current.Params)
	}
	// stage models carry their params (the front end's localized label)
	var l api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecDone, nil, http.StatusOK, &l)
	for _, st := range l.Stages {
		switch st.Stage {
		case model.StageSummary, model.StageRepair, model.StageTranslate:
			if st.Model == nil || st.Model.Params == nil || st.Model.Params.Temperature == nil {
				t.Errorf("%s: stage model without params %+v", st.Stage, st.Model)
			}
		case model.StageASR:
			if st.Model == nil || st.Model.Params != nil || !strings.Contains(st.Model.Label, "large-v2") {
				t.Errorf("asr stage model %+v", st.Model)
			}
		}
	}
	// back to off: repair is no longer in the diff (translate still is:
	// its snapshot is DeepSeek, the current setting Anthropic)
	doJSON(t, "PUT", u+"repair", api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-chat", Params: model.RoleParams{Temperature: fp(0.1)}}, http.StatusOK, &s)
	if rp := findRole(s.Roles, model.RoleRepair); rp.Thinking != model.ThinkingOff {
		t.Fatalf("repair back to off: %+v", rp)
	}
	nm = api.NeedsModelChoice{}
	code, b = rawCall(t, "POST", srv.URL+"/api/lectures/"+lecDone+"/stages/repair/rerun", nil)
	if code != http.StatusConflict || json.Unmarshal(b, &nm) != nil {
		t.Fatalf("rerun repair again: %d %s", code, b)
	}
	for _, d := range nm.Diff {
		if d.Stage == model.StageRepair {
			t.Errorf("repair with thinking off again still differs: %+v", d)
		}
	}
}
