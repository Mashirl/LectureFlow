// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Per-role thinking (.claude/thinking-brief.md): ProviderView.supports_thinking
// is settings.SupportsThinking (DeepSeek and Anthropic, not the other
// OpenAI-compatible providers); RoleView.thinking is the saved level ("off"
// when unset); PUT /api/settings/roles/{role} passes params.thinking through
// to settings.SetRole and its coded errors.
func TestProviderSupportsThinking(t *testing.T) {
	e := newEnv(t)
	create := func(name, typ, baseURL string) ProviderView {
		t.Helper()
		var pv ProviderView
		e.call("POST", "/api/providers", ProviderInput{Name: ptr(name), Type: ptr(typ), BaseURL: ptr(baseURL), APIKey: ptr("sk-test-0000")}, http.StatusCreated, &pv)
		return pv
	}
	ds := create("DeepSeek 2", model.ProviderOpenAI, "https://api.deepseek.com")
	an := create("Claude", model.ProviderAnthropic, "https://api.anthropic.com")
	or := create("OpenRouter", model.ProviderOpenAI, "https://openrouter.ai/api/v1")
	if !ds.SupportsThinking || !an.SupportsThinking || or.SupportsThinking {
		t.Fatalf("POST answers: deepseek %v, anthropic %v, openrouter %v", ds.SupportsThinking, an.SupportsThinking, or.SupportsThinking)
	}
	// a provider pointed at DeepSeek later supports it from then on
	var pv ProviderView
	e.call("PATCH", "/api/providers/"+or.ID, ProviderInput{BaseURL: ptr("https://api.deepseek.com/v1")}, http.StatusOK, &pv)
	if !pv.SupportsThinking {
		t.Fatalf("patched to DeepSeek: %+v", pv)
	}
	e.call("PATCH", "/api/providers/"+or.ID, ProviderInput{BaseURL: ptr("https://openrouter.ai/api/v1")}, http.StatusOK, &pv)
	if pv.SupportsThinking {
		t.Fatalf("patched back: %+v", pv)
	}

	// GET /api/settings and GET /api/providers agree with settings.SupportsThinking
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	var list []ProviderView
	e.call("GET", "/api/providers", nil, http.StatusOK, &list)
	if len(sv.Providers) != 4 || len(list) != 4 {
		t.Fatalf("providers %d / %d", len(sv.Providers), len(list))
	}
	for i, v := range sv.Providers {
		p, ok := e.set.Provider(v.ID)
		if !ok {
			t.Fatalf("provider %s", v.ID)
		}
		if v.SupportsThinking != settings.SupportsThinking(p) || list[i].SupportsThinking != v.SupportsThinking {
			t.Errorf("%s: supports_thinking %v / %v, settings says %v", v.Name, v.SupportsThinking, list[i].SupportsThinking, settings.SupportsThinking(p))
		}
	}
	want := map[string]bool{ds.ID: true, an.ID: true, or.ID: false}
	for _, v := range sv.Providers {
		if w, ok := want[v.ID]; ok && v.SupportsThinking != w {
			t.Errorf("%s: supports_thinking %v, want %v", v.Name, v.SupportsThinking, w)
		}
	}
	// the JSON field is always present (false is not omitted)
	_, b := e.do("GET", "/api/settings", nil)
	if n := strings.Count(string(b), `"supports_thinking":`); n != 4 {
		t.Errorf("supports_thinking appears %d times in %s", n, b)
	}
}

func TestRoleThinking(t *testing.T) {
	e := newEnv(t)
	var ds, or ProviderView
	e.call("POST", "/api/providers", ProviderInput{Name: ptr("DeepSeek 2"), Type: ptr(model.ProviderOpenAI), BaseURL: ptr("https://api.deepseek.com"), APIKey: ptr("sk-test-0000")}, http.StatusCreated, &ds)
	e.call("POST", "/api/providers", ProviderInput{Name: ptr("OpenRouter"), Type: ptr(model.ProviderOpenAI), BaseURL: ptr("https://openrouter.ai/api/v1"), APIKey: ptr("sk-test-0000")}, http.StatusCreated, &or)
	roleOf := func(sv Settings, role string) RoleView {
		t.Helper()
		for _, rv := range sv.Roles {
			if rv.Role == role {
				return rv
			}
		}
		t.Fatalf("no role %s", role)
		return RoleView{}
	}

	// every role starts off (appendix D2), also the unconfigured ones
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	for _, rv := range sv.Roles {
		if rv.Thinking != model.ThinkingOff {
			t.Errorf("%s: thinking %q at start", rv.Role, rv.Thinking)
		}
	}

	temp, win := 0.3, 3
	for _, lv := range []string{model.ThinkingLow, model.ThinkingHigh, model.ThinkingMax} {
		th := lv
		e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: ds.ID, Model: "deepseek-v4-pro", Params: model.RoleParams{Temperature: &temp, Window: &win, Thinking: &th}}, http.StatusOK, &sv)
		if tr := roleOf(sv, model.RoleTranslate); tr.Thinking != lv || tr.Temperature != 0.3 || tr.Window == nil || *tr.Window != 3 {
			t.Fatalf("translate after %s: %+v", lv, tr)
		}
		if rp := roleOf(sv, model.RoleRepair); rp.Thinking != model.ThinkingOff {
			t.Fatalf("repair changed with translate: %+v", rp)
		}
	}
	// GET reads the same back
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if tr := roleOf(sv, model.RoleTranslate); tr.Thinking != model.ThinkingMax {
		t.Fatalf("GET translate %+v", tr)
	}

	// invalid level, and a level other than off on a provider without thinking
	bogus := "extreme"
	info := e.callErrInfo("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: ds.ID, Model: "deepseek-v4-pro", Params: model.RoleParams{Thinking: &bogus}}, http.StatusBadRequest)
	if info.Code != settings.CodeThinkingInvalid || info.Params["value"] != bogus {
		t.Fatalf("invalid level: %+v", info)
	}
	low := model.ThinkingLow
	info = e.callErrInfo("PUT", "/api/settings/roles/summary", RoleInput{ProviderID: or.ID, Model: "deepseek/deepseek-chat", Params: model.RoleParams{Thinking: &low}}, http.StatusBadRequest)
	if info.Code != settings.CodeThinkingUnsupported || info.Params["provider"] != "OpenRouter" {
		t.Fatalf("unsupported: %+v", info)
	}
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if tr, su := roleOf(sv, model.RoleTranslate), roleOf(sv, model.RoleSummary); tr.Thinking != model.ThinkingMax || su.Thinking != model.ThinkingOff {
		t.Fatalf("a refused save changed a role: %+v / %+v", tr, su)
	}
	// "off" is accepted everywhere (the settings page sends it for a
	// provider without thinking), and so is leaving it out
	off := model.ThinkingOff
	e.call("PUT", "/api/settings/roles/summary", RoleInput{ProviderID: or.ID, Model: "deepseek/deepseek-chat", Params: model.RoleParams{Thinking: &off}}, http.StatusOK, &sv)
	if su := roleOf(sv, model.RoleSummary); su.Thinking != model.ThinkingOff || su.ProviderID == nil || *su.ProviderID != or.ID {
		t.Fatalf("summary off on OpenRouter: %+v", su)
	}
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: ds.ID, Model: "deepseek-v4-pro", Params: model.RoleParams{Temperature: &temp, Window: &win}}, http.StatusOK, &sv)
	if tr := roleOf(sv, model.RoleTranslate); tr.Thinking != model.ThinkingOff {
		t.Fatalf("translate without thinking: %+v", tr)
	}
}

// The snapshot labels (stage hover card, model-choice dialog) name the
// thinking level when it is on, and carry the params for the front end's
// own label in the interface language.
func TestSnapshotLabelThinking(t *testing.T) {
	e := newEnv(t)
	temp, win := 0.3, 3
	high := model.ThinkingHigh
	raw := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "gone", ProviderName: "DeepSeek", Model: "deepseek-v4-pro",
		Params: model.RoleParams{Temperature: &temp, Window: &win, Thinking: &high}})
	sm := e.srv.stageModel(model.StageTranslate, raw)
	if sm == nil || sm.Label != "DeepSeek / deepseek-v4-pro · 窗口 3 · 深度思考 高" {
		t.Fatalf("stage model %+v", sm)
	}
	if sm.Params == nil || sm.Params.Thinking == nil || *sm.Params.Thinking != high || sm.Params.Window == nil || *sm.Params.Window != 3 {
		t.Fatalf("stage model params %+v", sm.Params)
	}
	sv := e.srv.snapshotView(model.StageTranslate, raw)
	if sv.Label != sm.Label || sv.Params == nil || sv.Params.Thinking == nil || *sv.Params.Thinking != high {
		t.Fatalf("snapshot view %+v", sv)
	}
	for lv, name := range map[string]string{model.ThinkingLow: "低", model.ThinkingMax: "最高"} {
		th := lv
		if got := llmLabel("Claude", "claude-sonnet-4-5", model.RoleParams{Temperature: &temp, Thinking: &th}); got != "Claude / claude-sonnet-4-5 · 深度思考 "+name {
			t.Errorf("%s: %q", lv, got)
		}
	}
	// off, unset and an invalid stored value are not shown (model.ThinkingLevel)
	for _, th := range []*string{nil, ptr(""), ptr(model.ThinkingOff), ptr("extreme")} {
		if got := llmLabel("DeepSeek", "deepseek-flash", model.RoleParams{Temperature: &temp, Thinking: th}); got != "DeepSeek / deepseek-flash · 温度 0.3" {
			t.Errorf("thinking %v: %q", th, got)
		}
	}
	// asr has no params
	if am := e.srv.stageModel(model.StageASR, model.MarshalSnapshot(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"})); am == nil || am.Params != nil {
		t.Fatalf("asr stage model %+v", am)
	}
}
