// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"

	"lectureflow/internal/components"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Keys already in the environment (.claude/v3-brief.md §7.3): onboarding
// lists, for the services of the manifest's presets, whether the variable
// the service's own tools use is set (masked only), and "使用环境变量中的
// key" creates or updates that provider on the server, so the key never
// passes through the browser. No response and no log line carries more of
// the key than model.KeyHint's last four characters.

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/onboarding/env-keys", s.getEnvKeys)
		h("POST /api/onboarding/use-env-key", s.useEnvKey)
	})
}

// providerEnvVars are the environment variables the preset services
// document for their API keys, by preset provider id.
var providerEnvVars = map[string]string{
	"deepseek": "DEEPSEEK_API_KEY",
	"gemini":   "GEMINI_API_KEY",
	"zhipu":    "ZHIPUAI_API_KEY",
}

// fallbackPresetProviders stand in for the presets without a component
// manager: DeepSeek only, with the limits of config.example.toml and of the
// shipped manifest's preset (8 concurrent requests, 300 per minute; V12.2
// acceptance issue 2).
var fallbackPresetProviders = []components.PresetProvider{{
	ID: "deepseek", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
	DefaultModel: "deepseek-flash", BalanceAPI: model.BalanceDeepSeek, MaxConcurrency: 8, RPM: 300,
}}

// EnvKeyView is one row of GET /api/onboarding/env-keys.
type EnvKeyView struct {
	ProviderType string `json:"provider_type"`
	Env          string `json:"env"`
	Present      bool   `json:"present"`
	Masked       string `json:"masked,omitempty"` // model.KeyHint ("sk-…abcd")
}

// UseEnvKeyBody is the body of POST /api/onboarding/use-env-key.
type UseEnvKeyBody struct {
	ProviderType string `json:"provider_type"`
}

// presetProviders returns the preset services that have a known variable.
func (s *Server) presetProviders() []components.PresetProvider {
	list := fallbackPresetProviders
	if s.comps != nil {
		list = s.comps.Presets().Providers
	}
	var out []components.PresetProvider
	for _, p := range list {
		if providerEnvVars[p.ID] != "" {
			out = append(out, p)
		}
	}
	return out
}

// envKey reads the key of a variable (surrounding spaces removed).
func (s *Server) envKey(name string) string {
	return strings.TrimSpace(s.compState().getenv(name))
}

// GET /api/onboarding/env-keys.
func (s *Server) getEnvKeys(w http.ResponseWriter, r *http.Request) {
	out := []EnvKeyView{}
	for _, p := range s.presetProviders() {
		env := providerEnvVars[p.ID]
		v := EnvKeyView{ProviderType: p.ID, Env: env}
		if k := s.envKey(env); k != "" {
			v.Present, v.Masked = true, model.KeyHint(k)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/onboarding/use-env-key {"provider_type"}: updates the key of
// the provider with the preset's base_url (or else its name), or creates
// the provider from the preset (201) with the preset's max_concurrency and
// rpm (the settings defaults 4 / 60 only where the preset gives none),
// through the settings service like the v1.1 provider endpoints. An
// existing provider keeps its limits: they may be the user's own. 404
// onboarding.env_key_missing when the variable is not set.
func (s *Server) useEnvKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in UseEnvKeyBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	var pp *components.PresetProvider
	for _, p := range s.presetProviders() {
		if p.ID == in.ProviderType {
			p := p
			pp = &p
			break
		}
	}
	if pp == nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": "provider_type"}))
		return
	}
	env := providerEnvVars[pp.ID]
	key := s.envKey(env)
	if key == "" {
		s.fail(w, r, apiErr(http.StatusNotFound, "onboarding.env_key_missing", map[string]any{"provider_type": pp.ID, "env": env}))
		return
	}
	norm := func(u string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/")) }
	var match *model.Provider
	ps := s.set.Providers()
	for i := range ps {
		if norm(ps[i].BaseURL) == norm(pp.BaseURL) {
			match = &ps[i]
			break
		}
	}
	if match == nil {
		for i := range ps {
			if strings.EqualFold(strings.TrimSpace(ps[i].Name), pp.Name) {
				match = &ps[i]
				break
			}
		}
	}
	if match != nil {
		p, err := s.set.UpdateProvider(r.Context(), match.ID, settings.ProviderInput{APIKey: &key})
		if err != nil {
			s.fail(w, r, notFoundAs(err, "provider.not_found"))
			return
		}
		s.respondProvider(w, r, p, http.StatusOK)
		return
	}
	name, typ, base := pp.Name, pp.Type, pp.BaseURL
	pin := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key}
	if pp.BalanceAPI != "" {
		b := pp.BalanceAPI
		pin.BalanceAPI = &b
	}
	if pp.MaxConcurrency > 0 {
		c := pp.MaxConcurrency
		pin.MaxConcurrency = &c
	}
	if pp.RPM > 0 {
		rpm := pp.RPM
		pin.RPM = &rpm
	}
	p, err := s.set.CreateProvider(r.Context(), pin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondProvider(w, r, p, http.StatusCreated)
}
