// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"slices"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// v2 (SPEC-v2 G3.1, appendix D1/D2): role label and hint, DeepSeek
// detection, the newest Opus model and the default role assignment.

func TestJanitorLabelAndHint(t *testing.T) {
	if got := RoleLabel(model.RoleGlossaryJanitor); got != "术语表清道夫" {
		t.Errorf("label %q", got)
	}
	if got := RoleHint(model.RoleGlossaryJanitor); got != "建议选择与术语提取不同的模型，例如 Claude Opus，以便互相纠错。" {
		t.Errorf("hint %q", got)
	}
	for _, role := range model.Roles {
		if role != model.RoleGlossaryJanitor && RoleHint(role) != "" {
			t.Errorf("%s has a hint %q", role, RoleHint(role))
		}
	}
}

func TestIsDeepSeek(t *testing.T) {
	for _, tc := range []struct {
		baseURL, balance string
		want             bool
	}{
		{"https://api.deepseek.com", model.BalanceNone, true},
		{"https://api.deepseek.com/v1", "", true},
		{"https://API.DeepSeek.com/", "", true},
		{"https://deepseek.com", "", true},
		{"https://api.deepseek.com/anthropic", "", true},
		{"http://127.0.0.1:9999", model.BalanceDeepSeek, true}, // the balance API marks it as DeepSeek
		{"http://127.0.0.1:9999", model.BalanceNone, false},
		{"https://notdeepseek.com", "", false},
		{"https://api.deepseek.com.example.org", "", false},
		{"https://api.openai.com/v1", "", false},
		{"https://api.anthropic.com", "", false},
		{"", "", false},
		{"::not a url", "", false},
	} {
		if got := IsDeepSeek(tc.baseURL, tc.balance); got != tc.want {
			t.Errorf("IsDeepSeek(%q, %q) = %v, want %v", tc.baseURL, tc.balance, got, tc.want)
		}
	}
}

func TestLatestOpus(t *testing.T) {
	for _, tc := range []struct {
		models []string
		want   string // "" = none
	}{
		{[]string{"claude-opus-4-1", "claude-opus-5-5", "claude-sonnet-5-5", "claude-opus-5"}, "claude-opus-5-5"},
		{[]string{"claude-opus-5", "claude-opus-4-8"}, "claude-opus-5"},
		{[]string{"claude-opus-4-1-20250805", "claude-opus-4-8"}, "claude-opus-4-8"},
		{[]string{"claude-3-opus-20240229", "claude-opus-4-1"}, "claude-opus-4-1"},
		{[]string{"claude-3-opus-20240229", "claude-3-5-sonnet-20241022"}, "claude-3-opus-20240229"},
		{[]string{"claude-opus-4-1-20250805", "claude-opus-4-1"}, "claude-opus-4-1"}, // the alias over the snapshot
		{[]string{"claude-opus-4-10", "claude-opus-4-9"}, "claude-opus-4-10"},        // numbers, not strings
		{[]string{"claude-opus-4-1", "claude-opus-4-5@20251101"}, "claude-opus-4-5@20251101"},
		{[]string{" claude-opus-5-5 "}, "claude-opus-5-5"},
		{[]string{"claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}, ""},
		{[]string{"octopus-1", "opusculum"}, ""},
		{nil, ""},
	} {
		got, ok := LatestOpus(tc.models)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("LatestOpus(%q) = %q, %v; want %q", tc.models, got, ok, tc.want)
		}
	}
}

func roleRow(t *testing.T, st db.Store, role string) (model.RoleSetting, bool) {
	t.Helper()
	rows, err := st.ListRoleSettings(bg())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Role == role {
			return r, true
		}
	}
	return model.RoleSetting{}, false
}

func defaultMark(t *testing.T, st db.Store, role string) (string, bool) {
	t.Helper()
	v, ok, err := st.GetKV(bg(), model.KVRoleDefaultPrefix+role)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func applyDefaults(t *testing.T, s *Service) []string {
	t.Helper()
	filled, err := s.ApplyRoleDefaults(bg())
	if err != nil {
		t.Fatal(err)
	}
	return filled
}

func janitorTarget(t *testing.T, s *Service) model.LLMSnapshot {
	t.Helper()
	tg, err := s.RoleTarget(model.RoleGlossaryJanitor)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestApplyRoleDefaultsWithoutAnthropicProvider(t *testing.T) {
	s, st := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	create(t, s, "Claude without key", model.ProviderAnthropic, "https://api.anthropic.com", "")
	// DeepSeek's Anthropic-compatible endpoint maps claude-opus-* to its own model.
	create(t, s, "DeepSeek Anthropic", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", fakeKey)
	for _, role := range []string{model.RoleSummary, model.RoleGlossary} {
		if err := s.SetRole(bg(), role, ds.ID, "deepseek-flash", model.RoleParams{}); err != nil {
			t.Fatal(err)
		}
	}
	hooks := 0
	s.OnChange(func() { hooks++ })

	// Phase 2 (D1): chapters and organizer take the summary role's provider
	// and model; the janitor stays unconfigured without an Anthropic provider.
	if filled := applyDefaults(t, s); len(filled) != 2 || !contains(filled, model.RoleChapters) || !contains(filled, model.RoleOrganizer) {
		t.Fatalf("filled %v", filled)
	}
	if _, ok := roleRow(t, st, model.RoleGlossaryJanitor); ok {
		t.Fatal("janitor row written without an Anthropic provider")
	}
	if _, ok := defaultMark(t, st, model.RoleGlossaryJanitor); ok {
		t.Fatal("default mark recorded although nothing was written")
	}
	// One write, one OnChange call (chapters and organizer were filled).
	if s.RoleDefault(model.RoleGlossaryJanitor) || hooks != 1 {
		t.Fatalf("RoleDefault %v hooks %d", s.RoleDefault(model.RoleGlossaryJanitor), hooks)
	}
	if _, err := s.RoleTarget(model.RoleGlossaryJanitor); err == nil {
		t.Fatal("janitor must stay unconfigured")
	}
	// Phase 2 (lead ruling 11): chapters and organizer are in model.Roles and
	// get the summary role's provider and model, with a default mark.
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		r, ok := roleRow(t, st, role)
		if !ok || r.ProviderID == nil || *r.ProviderID != ds.ID || r.Model == nil || *r.Model != "deepseek-flash" || r.Params != "{}" {
			t.Errorf("%s row %+v %v", role, r, ok)
		}
		if v, ok := defaultMark(t, st, role); !ok || v != "1" {
			t.Errorf("%s mark %q %v", role, v, ok)
		}
		if !s.RoleDefault(role) {
			t.Errorf("%s: RoleDefault false", role)
		}
		if tg, err := s.RoleTarget(role); err != nil || tg.ProviderID != ds.ID || tg.Model != "deepseek-flash" {
			t.Errorf("%s target %+v %v", role, tg, err)
		}
	}

	// An Anthropic provider added later is picked up by the next start.
	c := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	hooks = 0
	if filled := applyDefaults(t, s); !slices.Equal(filled, []string{model.RoleGlossaryJanitor}) {
		t.Fatalf("filled %v", filled)
	}
	if tg := janitorTarget(t, s); tg.ProviderID != c.ID || tg.Model != DefaultJanitorModel {
		t.Fatalf("janitor %+v", tg)
	}
	if hooks != 1 {
		t.Errorf("OnChange called %d times", hooks)
	}
	// The defaults of chapters and organizer are kept (they are configured now).
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		if !s.RoleDefault(role) {
			t.Errorf("%s lost its default mark", role)
		}
	}
}

// Phase 2 (D1): without a configured summary role, chapters and organizer
// stay unconfigured; the next start after summary is set fills them, and a
// user's choice clears the mark of that role only.
func TestApplyRoleDefaultsChaptersOrganizerFollowSummary(t *testing.T) {
	s, st := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	hooks := 0
	s.OnChange(func() { hooks++ })
	if filled := applyDefaults(t, s); len(filled) != 0 || hooks != 0 {
		t.Fatalf("filled %v hooks %d without a summary role", filled, hooks)
	}
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		if _, ok := roleRow(t, st, role); ok {
			t.Errorf("%s written without a summary role", role)
		}
		if _, ok := defaultMark(t, st, role); ok {
			t.Errorf("%s mark recorded without a summary role", role)
		}
	}
	if err := s.SetRole(bg(), model.RoleSummary, ds.ID, "deepseek-v4-pro", model.RoleParams{Temperature: fp(0.5)}); err != nil {
		t.Fatal(err)
	}
	hooks = 0
	if filled := applyDefaults(t, s); !slices.Equal(filled, []string{model.RoleChapters, model.RoleOrganizer}) || hooks != 1 {
		t.Fatalf("filled %v hooks %d", filled, hooks)
	}
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		tg, err := s.RoleTarget(role)
		if err != nil || tg.ProviderID != ds.ID || tg.Model != "deepseek-v4-pro" {
			t.Fatalf("%s target %+v %v", role, tg, err)
		}
		// Only provider and model are copied; params keep the role's defaults.
		if *tg.Params.Temperature != model.DefaultTemperature(role) {
			t.Errorf("%s temperature %v", role, *tg.Params.Temperature)
		}
	}
	if err := s.SetRole(bg(), model.RoleChapters, ds.ID, "deepseek-flash", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if s.RoleDefault(model.RoleChapters) || !s.RoleDefault(model.RoleOrganizer) {
		t.Fatalf("marks: chapters %v organizer %v", s.RoleDefault(model.RoleChapters), s.RoleDefault(model.RoleOrganizer))
	}
	// Later starts never change them again, even when summary changes.
	if err := s.SetRole(bg(), model.RoleSummary, ds.ID, "deepseek-flash", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if filled := applyDefaults(t, s); len(filled) != 0 {
		t.Fatalf("filled %v", filled)
	}
	if tg, _ := s.RoleTarget(model.RoleOrganizer); tg.Model != "deepseek-v4-pro" {
		t.Fatalf("organizer changed to %q", tg.Model)
	}
}

func TestApplyRoleDefaultsUnprobedProvider(t *testing.T) {
	s, st := newService(t)
	c := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if filled := applyDefaults(t, s); !slices.Equal(filled, []string{model.RoleGlossaryJanitor}) {
		t.Fatalf("filled %v", filled)
	}
	if tg := janitorTarget(t, s); tg.ProviderID != c.ID || tg.Model != "claude-opus-5-5" || tg.ProviderName != "Claude" {
		t.Fatalf("janitor %+v", tg)
	}
	if v, ok := defaultMark(t, st, model.RoleGlossaryJanitor); !ok || v != "1" {
		t.Fatalf("mark %q %v", v, ok)
	}
	if !s.RoleDefault(model.RoleGlossaryJanitor) || s.RoleDefault(model.RoleSummary) {
		t.Fatal("RoleDefault")
	}
	if r, _ := roleRow(t, st, model.RoleGlossaryJanitor); r.Params != "{}" {
		t.Errorf("params %q", r.Params)
	}
	// The next start changes nothing (the role is configured now).
	if filled := applyDefaults(t, s); len(filled) != 0 {
		t.Fatalf("second run filled %v", filled)
	}
	// The mark survives a restart.
	s2, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatal("mark lost on restart")
	}
}

func TestApplyRoleDefaultsUsesNewestProbedOpus(t *testing.T) {
	s, _ := newService(t)
	// Created first, but never probed: loses against a probed Opus.
	create(t, s, "Claude unprobed", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	// Probed, but this key lists no Opus model: skipped.
	noOpus := create(t, s, "Claude Sonnet only", model.ProviderAnthropic, "https://api.anthropic.com/", fakeKey)
	if err := s.SetProbedModels(bg(), noOpus.ID, model.ProbedModels{Models: []string{"claude-sonnet-5-5", "claude-haiku-5-5"}}); err != nil {
		t.Fatal(err)
	}
	probed := create(t, s, "Claude probed", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if err := s.SetProbedModels(bg(), probed.ID, model.ProbedModels{
		Models: []string{"claude-haiku-5-5", "claude-opus-4-1-20250805", "claude-opus-5", "claude-opus-4-8", "claude-sonnet-5-5"},
	}); err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, s)
	if tg := janitorTarget(t, s); tg.ProviderID != probed.ID || tg.Model != "claude-opus-5" {
		t.Fatalf("janitor %+v", tg)
	}
}

func TestApplyRoleDefaultsSkipsProvidersWithoutOpus(t *testing.T) {
	s, _ := newService(t)
	noOpus := create(t, s, "Claude Sonnet only", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if err := s.SetProbedModels(bg(), noOpus.ID, model.ProbedModels{Models: []string{"claude-sonnet-5-5"}}); err != nil {
		t.Fatal(err)
	}
	if filled := applyDefaults(t, s); len(filled) != 0 {
		t.Fatalf("filled %v with a provider that lists no Opus", filled)
	}
	// An empty probe result or a broken cache counts as "no probe result".
	empty := create(t, s, "Claude empty probe", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if err := s.SetProbedModels(bg(), empty.ID, model.ProbedModels{}); err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, s)
	if tg := janitorTarget(t, s); tg.ProviderID != empty.ID || tg.Model != DefaultJanitorModel {
		t.Fatalf("janitor %+v", tg)
	}

	s2, st2 := newService(t)
	broken := create(t, s2, "Claude broken cache", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if err := st2.SetKV(bg(), model.KVProviderModelsPref+broken.ID, "{not json"); err != nil {
		t.Fatal(err)
	}
	applyDefaults(t, s2)
	if tg := janitorTarget(t, s2); tg.ProviderID != broken.ID || tg.Model != DefaultJanitorModel {
		t.Fatalf("janitor %+v", tg)
	}
}

func TestApplyRoleDefaultsNeverChangesAConfiguredRole(t *testing.T) {
	s, st := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	c := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if err := s.SetProbedModels(bg(), c.ID, model.ProbedModels{Models: []string{"claude-opus-5-5"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(bg(), model.RoleGlossaryJanitor, ds.ID, "deepseek-v4-pro", model.RoleParams{Temperature: fp(0.1)}); err != nil {
		t.Fatal(err)
	}
	if filled := applyDefaults(t, s); len(filled) != 0 {
		t.Fatalf("filled %v", filled)
	}
	if tg := janitorTarget(t, s); tg.ProviderID != ds.ID || tg.Model != "deepseek-v4-pro" || *tg.Params.Temperature != 0.1 {
		t.Fatalf("configured janitor changed: %+v", tg)
	}
	if _, ok := defaultMark(t, st, model.RoleGlossaryJanitor); ok || s.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatal("a configured role must not get a default mark")
	}
}

func TestSetRoleClearsDefaultMark(t *testing.T) {
	s, st := newService(t)
	c := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	applyDefaults(t, s)
	if !s.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatal("not marked")
	}
	// Even choosing the same provider and model is the user's choice now.
	if err := s.SetRole(bg(), model.RoleGlossaryJanitor, c.ID, DefaultJanitorModel, model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if s.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatal("SetRole must clear the mark")
	}
	if v, _ := defaultMark(t, st, model.RoleGlossaryJanitor); v != "" {
		t.Fatalf("kv mark %q", v)
	}
	s2, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatal("cleared mark came back after a restart")
	}
	if filled := applyDefaults(t, s2); len(filled) != 0 {
		t.Fatalf("filled %v", filled)
	}
	// SetRole of a role without a mark writes no kv row.
	ds := create(t, s2, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	if err := s2.SetRole(bg(), model.RoleTranslate, ds.ID, "deepseek-flash", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := defaultMark(t, st, model.RoleTranslate); ok {
		t.Fatal("unexpected kv mark for translate")
	}
}

// Phase 2 roles (chapters, organizer) take the summary role's provider and
// model (roleDefault, used by ApplyRoleDefaults now that model.Roles lists them).
func TestPhase2RoleDefaults(t *testing.T) {
	s, st := newService(t)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	provs := s.Providers()
	current := map[string]model.RoleSetting{}
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		if _, _, ok, err := roleDefault(bg(), st, role, current, provs); err != nil || ok {
			t.Fatalf("%s without summary: ok=%v err=%v", role, ok, err)
		}
	}
	pid, m := ds.ID, " deepseek-flash "
	current[model.RoleSummary] = model.RoleSetting{Role: model.RoleSummary, ProviderID: &pid, Model: &m}
	for _, role := range []string{model.RoleChapters, model.RoleOrganizer} {
		p, got, ok, err := roleDefault(bg(), st, role, current, provs)
		if err != nil || !ok || p.ID != ds.ID || got != "deepseek-flash" {
			t.Fatalf("%s: %v %q %v %v", role, p.ID, got, ok, err)
		}
	}
	gone := "deleted-provider"
	current[model.RoleSummary] = model.RoleSetting{Role: model.RoleSummary, ProviderID: &gone, Model: &m}
	if _, _, ok, _ := roleDefault(bg(), st, model.RoleChapters, current, provs); ok {
		t.Fatal("summary pointing at a missing provider is not configured")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
