// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// The v2 startup steps run between the migrations and the scheduler
// (lead rulings 12, 13): an Anthropic provider added since the last start
// becomes the glossary_janitor default on the next start. No network: the
// provider is never contacted (no probe result → the default model).
func TestBuildAppAppliesV2RoleDefaults(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	llmtest.New(t).Configure(cfg) // never the real API

	app, err := buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := state.JanitorConfigured(ctx, app.Store); err != nil || ok {
		t.Fatalf("first start without an Anthropic provider: configured=%v %v", ok, err)
	}
	name, typ, url, key := "Anthropic", model.ProviderAnthropic, "https://api.anthropic.example.test", "sk-test-not-a-real-key"
	p, err := app.Settings.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	app.Close()

	app, err = buildApp(ctx, cfg, AppOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	tgt, err := app.Settings.RoleTarget(model.RoleGlossaryJanitor)
	if err != nil {
		t.Fatalf("glossary_janitor after the second start: %v", err)
	}
	if tgt.ProviderID != p.ID || tgt.Model != settings.DefaultJanitorModel || !app.Settings.RoleDefault(model.RoleGlossaryJanitor) {
		t.Fatalf("glossary_janitor = %+v (default mark %v)", tgt, app.Settings.RoleDefault(model.RoleGlossaryJanitor))
	}
	if ok, err := state.JanitorConfigured(ctx, app.Store); err != nil || !ok {
		t.Fatalf("JanitorConfigured = %v %v", ok, err)
	}
}
