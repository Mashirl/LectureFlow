// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/model"
)

// TestUseEnvKeyMatchesByBaseURLOnly (sec2-app L2): a provider that only has
// the preset's name (here the fixture's "deepseek", pointing at the fake
// LLM server) does not receive the environment's key; a new provider with
// the preset's base_url and a free name is created instead.
func TestUseEnvKeyMatchesByBaseURLOnly(t *testing.T) {
	const secret = "sk-envkey-l2-test-0042"
	e := newEnv(t, func(s *Server) {
		s.compState().getenv = func(k string) string {
			if k == "DEEPSEEK_API_KEY" {
				return secret
			}
			return ""
		}
	})
	var fixture, fixtureKey string
	for _, p := range e.set.Providers() {
		if strings.EqualFold(p.Name, "DeepSeek") && !strings.Contains(strings.ToLower(p.BaseURL), "api.deepseek.com") {
			fixture, fixtureKey = p.ID, p.APIKey
		}
	}
	if fixture == "" {
		t.Fatalf("no fixture provider named deepseek on another host: %+v", e.set.Providers())
	}
	var pv ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusCreated, &pv)
	if pv.ID == fixture || pv.Name != "DeepSeek (deepseek)" || pv.BaseURL != "https://api.deepseek.com" || pv.KeyHint != "sk-…0042" {
		t.Fatalf("created %+v", pv)
	}
	if p, _ := e.set.Provider(fixture); p.APIKey != fixtureKey {
		t.Fatal("the provider with the preset's name got the environment's key")
	}
	// Once more: the provider with the base_url is found (200), no third one.
	n := len(e.set.Providers())
	var pv2 ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusOK, &pv2)
	if pv2.ID != pv.ID || len(e.set.Providers()) != n {
		t.Fatalf("second call: %+v (%d providers, was %d)", pv2, len(e.set.Providers()), n)
	}
}

func TestFreeProviderName(t *testing.T) {
	pp := &components.PresetProvider{ID: "deepseek", Name: "DeepSeek"}
	ps := providersNamed("deepseek", "DeepSeek (deepseek)", "DeepSeek (DEEPSEEK) 2")
	if got := freeProviderName(ps, pp); got != "DeepSeek (deepseek) 3" {
		t.Errorf("name %q", got)
	}
	if got := freeProviderName(providersNamed("Other"), pp); got != "DeepSeek" {
		t.Errorf("name %q", got)
	}
}

func providersNamed(names ...string) []model.Provider {
	out := make([]model.Provider, len(names))
	for i, n := range names {
		out[i] = model.Provider{ID: n, Name: n}
	}
	return out
}
