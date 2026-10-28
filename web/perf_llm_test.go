// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/settings"
)

// perf-llm (pipeline-builder, 2026-10-27): provider_rules.js holds the
// limit ranges of internal/settings and the DeepSeek preset's limits of the
// shipped manifest; the settings page offers them to a DeepSeek provider on
// the old limits and warns when a base_url edit will clear the key
// (sec2-app L2); every new text exists in zh-CN, en and context.

func jsConst(t *testing.T, src, re string) int {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("provider_rules.js: no match for %s", re)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProviderRulesInStep(t *testing.T) {
	src := readWeb(t, "provider_rules.js")
	if got := jsConst(t, src, `export const MAX_CONCURRENCY = (\d+);`); got != settings.MaxConcurrencyLimit {
		t.Errorf("MAX_CONCURRENCY %d, settings %d", got, settings.MaxConcurrencyLimit)
	}
	if got := jsConst(t, src, `export const MAX_RPM = (\d+);`); got != settings.MaxRPM {
		t.Errorf("MAX_RPM %d, settings %d", got, settings.MaxRPM)
	}
	conc := jsConst(t, src, `DEEPSEEK_LIMITS = Object\.freeze\(\{ max_concurrency: (\d+), rpm: \d+ \}\)`)
	rpm := jsConst(t, src, `DEEPSEEK_LIMITS = Object\.freeze\(\{ max_concurrency: \d+, rpm: (\d+) \}\)`)
	m, err := components.LoadManifest(filepath.Join("..", "packaging", "components", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range m.Presets.Providers {
		if p.ID == "deepseek" {
			found = true
			if p.MaxConcurrency != conc || p.RPM != rpm {
				t.Errorf("DEEPSEEK_LIMITS %d / %d, manifest preset %d / %d", conc, rpm, p.MaxConcurrency, p.RPM)
			}
		}
	}
	if !found {
		t.Fatal("no deepseek preset")
	}
	if conc != config.DeepSeekMaxConcurrency {
		t.Errorf("DEEPSEEK_LIMITS %d, config default %d", conc, config.DeepSeekMaxConcurrency)
	}
}

func TestProviderLimitsUI(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"from './provider_rules.js'",
		"limitsHint(p)",
		"if (!hasOldLimits(p)) return null;",
		"api.patchProvider(p.id, { max_concurrency: DEEPSEEK_LIMITS.max_concurrency, rpm: DEEPSEEK_LIMITS.rpm })",
		"rpm > 0 ? t('settings.provider.rpm_value', { n: rpm }) : t('settings.provider.rpm_unlimited')",
		"min: '0', max: String(MAX_RPM)",
		"max: String(MAX_CONCURRENCY)",
		"r < 0 || r > MAX_RPM",
		"!sameOrigin(p.base_url, base.value.trim())",
		"t('settings.provider.key_moved_hint')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	if s := readWeb(t, "view_system.js"); !strings.Contains(s, "a.rpm_limit > 0 ? t('system.api.limits'") {
		t.Error("view_system.js: rpm 0 must read as no per-minute limit")
	}
	if s := readWeb(t, "onboarding_providers.js"); !strings.Contains(s, "input.rpm = pp.rpm || 0;") {
		t.Error("onboarding_providers.js: a preset without rpm must create a provider without per-minute limit")
	}
	keys := []string{
		"settings.provider.rpm_unlimited", "settings.provider.rpm_range_hint", "settings.provider.limits_old",
		"settings.provider.limits_apply", "settings.provider.limits_applying", "settings.provider.limits_applied",
		"settings.provider.key_moved_hint", "system.api.limits_unlimited",
	}
	for _, loc := range []string{"zh-CN", "en", "context"} {
		c := loadCatalogOrPart(t, loc)
		for _, k := range keys {
			if _, ok := c[k]; !ok {
				t.Errorf("%s: no %s", loc, k)
			}
		}
	}
}

// loadCatalogOrPart reads web/i18n/{loc}.json, falling back to the
// perf-llm part for keys the lead has not merged yet.
func loadCatalogOrPart(t *testing.T, loc string) catalog {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
	part := filepath.Join(catalogDir, "_parts", "perf-llm."+loc+".json")
	if fileExists(part) {
		for k, v := range loadCatalog(t, part) {
			if _, ok := c[k]; !ok {
				c[k] = v
			}
		}
	}
	return c
}
