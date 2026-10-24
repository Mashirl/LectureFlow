// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/settings"
)

// syncBuffer collects log output from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends the default slog and log output to a buffer (debug
// level) until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev, prevW, prevF := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevW)
		log.SetFlags(prevF)
	})
	return buf
}

func TestEnvKeysAndUseEnvKey(t *testing.T) {
	const secret = "sk-test-ENVSECRET-0123456789-wxyz"
	const secret2 = "sk-test-ENVSECRET-second-key-abcd"
	logs := captureLogs(t)
	env := map[string]string{"DEEPSEEK_API_KEY": "  " + secret + "\n"}
	var envMu sync.Mutex
	ce := newCompEnv(t, func(_ *Server, cs *compState) {
		cs.getenv = func(k string) string {
			envMu.Lock()
			defer envMu.Unlock()
			return env[k]
		}
	})
	var bodies []string
	do := func(method, path string, body any, want int, out any) {
		t.Helper()
		res, b := ce.do(method, path, body)
		bodies = append(bodies, string(b))
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}

	var keys []EnvKeyView
	do("GET", "/api/onboarding/env-keys", nil, http.StatusOK, &keys)
	if len(keys) != 2 || keys[0] != (EnvKeyView{ProviderType: "deepseek", Env: "DEEPSEEK_API_KEY", Present: true, Masked: "sk-…wxyz"}) ||
		keys[1] != (EnvKeyView{ProviderType: "gemini", Env: "GEMINI_API_KEY"}) {
		t.Fatalf("env keys: %+v", keys)
	}

	// No provider with the preset's base_url or name yet: created (201).
	// (the fixture's provider named "deepseek" points at the fake LLM
	// server; renamed so that neither base_url nor name matches)
	for _, p := range ce.set.Providers() {
		if strings.EqualFold(p.Name, "DeepSeek") {
			name := "fake " + p.Name
			if _, err := ce.set.UpdateProvider(context.Background(), p.ID, settings.ProviderInput{Name: &name}); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := len(ce.set.Providers())
	var pv ProviderView
	do("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusCreated, &pv)
	if pv.KeyHint != "sk-…wxyz" || pv.Name != "DeepSeek" || pv.BaseURL != "https://api.deepseek.com" || pv.BalanceAPI != "deepseek" {
		t.Fatalf("created: %+v", pv)
	}
	if len(ce.set.Providers()) != before+1 {
		t.Fatalf("providers: %d, want %d", len(ce.set.Providers()), before+1)
	}
	if p, ok := ce.set.Provider(pv.ID); !ok || p.APIKey != secret {
		t.Fatal("the provider does not hold the trimmed key from the environment")
	}

	// Again with another key: the same provider is updated (200).
	envMu.Lock()
	env["DEEPSEEK_API_KEY"] = secret2
	envMu.Unlock()
	var pv2 ProviderView
	do("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusOK, &pv2)
	if pv2.ID != pv.ID || pv2.KeyHint != "sk-…abcd" || len(ce.set.Providers()) != before+1 {
		t.Fatalf("updated: %+v", pv2)
	}
	if p, _ := ce.set.Provider(pv.ID); p.APIKey != secret2 {
		t.Fatal("key not updated")
	}

	// Missing variable: 404 with the code; unknown service: 400.
	do("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "gemini"}, http.StatusNotFound, nil)
	if info := ce.callErrInfo("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "gemini"}, http.StatusNotFound); info.Code != "onboarding.env_key_missing" ||
		info.Params["env"] != "GEMINI_API_KEY" {
		t.Errorf("missing: %+v", info)
	}
	if c := ce.callErrInfo("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "openai"}, http.StatusBadRequest).Code; c != "request.bad_param" {
		t.Errorf("unknown: %s", c)
	}

	// the provider list and settings never carry the key either
	do("GET", "/api/providers", nil, http.StatusOK, nil)
	do("GET", "/api/settings", nil, http.StatusOK, nil)

	for _, b := range bodies {
		for _, s := range []string{secret, secret2, "ENVSECRET"} {
			if strings.Contains(b, s) {
				t.Fatalf("a response contains the key: %s", b)
			}
		}
	}
	if l := logs.String(); strings.Contains(l, "ENVSECRET") {
		t.Fatalf("the log contains the key:\n%s", l)
	}
}

func TestEnvKeysWithoutComponents(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.compState().getenv = func(k string) string {
			if k == "GEMINI_API_KEY" {
				return "sk-test-gem-1234"
			}
			return ""
		}
	})
	var keys []EnvKeyView
	e.call("GET", "/api/onboarding/env-keys", nil, http.StatusOK, &keys)
	if len(keys) != 1 || keys[0].ProviderType != "deepseek" || keys[0].Present {
		t.Fatalf("fallback presets: %+v", keys)
	}
	if c := e.callErrInfo("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusNotFound).Code; c != "onboarding.env_key_missing" {
		t.Errorf("missing: %s", c)
	}
}

// renameDeepSeekFixture renames the fixture's provider named "deepseek"
// (it points at the fake LLM server), so use-env-key creates a new one.
func renameDeepSeekFixture(t *testing.T, set *settings.Service) {
	t.Helper()
	for _, p := range set.Providers() {
		if strings.EqualFold(p.Name, "DeepSeek") {
			name := "fake " + p.Name
			if _, err := set.UpdateProvider(context.Background(), p.ID, settings.ProviderInput{Name: &name}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// V12.2 acceptance issue 2: the provider made from an environment key gets
// the preset's limits (DeepSeek 8 concurrent, 300 per minute), with the
// shipped manifest and without a component manager.
func TestUseEnvKeyUsesPresetLimits(t *testing.T) {
	const secret = "sk-test-ENVSECRET-limits-0000"
	getenv := func(k string) string {
		if k == "DEEPSEEK_API_KEY" {
			return secret
		}
		return ""
	}
	manifest, err := filepath.Abs(filepath.Join("..", "..", "packaging", "components", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := components.New(components.Options{
		DataDir: t.TempDir(), AppVersion: "3.0.0", Platform: components.PlatformWindowsX64, BuiltinManifest: manifest,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	var preset components.PresetProvider
	for _, p := range m.Presets().Providers {
		if p.ID == "deepseek" {
			preset = p
		}
	}
	if preset.MaxConcurrency != 8 || preset.RPM != 300 {
		t.Fatalf("shipped DeepSeek preset: %d / %d, want 8 / 300", preset.MaxConcurrency, preset.RPM)
	}
	for name, comps := range map[string]*components.Manager{"manifest": m, "fallback": nil} {
		e := newEnv(t, func(s *Server) {
			s.comps = comps
			s.compState().getenv = getenv
		})
		renameDeepSeekFixture(t, e.set)
		var pv ProviderView
		e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusCreated, &pv)
		if pv.MaxConcurrency != 8 || pv.RPM != 300 || pv.BalanceAPI != "deepseek" {
			t.Errorf("%s: created %+v", name, pv)
		}
		// an existing provider keeps its own limits; only the key changes
		four, sixty := 4, 60
		if _, err := e.set.UpdateProvider(context.Background(), pv.ID, settings.ProviderInput{MaxConcurrency: &four, RPM: &sixty}); err != nil {
			t.Fatal(err)
		}
		var pv2 ProviderView
		e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusOK, &pv2)
		if pv2.ID != pv.ID || pv2.MaxConcurrency != 4 || pv2.RPM != 60 {
			t.Errorf("%s: updated %+v", name, pv2)
		}
	}
}
