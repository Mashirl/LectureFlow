// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Power management, front-end gate round 1 (web-builder): every text of the
// power-aware policy says so ("电源感知" / "power-aware", requirement 7),
// the tag of a computer without a detected battery does not claim it is a
// desktop (item 11), the laptop variants and the self-test wait exist with
// their wording (items 5, 12), lecture summaries of power-aware waits use
// the "{stage}：{wait}" form (item 4), and the keys the UI no longer uses
// are marked in context.json, not deleted (item 9).

// text is a catalog value as one string (the forms of a plural joined).
func text(t *testing.T, c catalog, key string) string {
	t.Helper()
	switch v := c[key].(type) {
	case string:
		return v
	case map[string]any:
		var forms []string
		for _, f := range v {
			if s, ok := f.(string); ok {
				forms = append(forms, s)
			}
		}
		return strings.Join(forms, "\n")
	}
	t.Errorf("no text for %s", key)
	return ""
}

func TestPowerTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	powerAware := []string{
		"waiting.power_wait_ac", "waiting.power_no_saver_device", "waiting.power_no_device", "waiting.power_queue",
		"waiting.power_switch_pending", "waiting.power_device_selftest",
		"resources.power.reason.battery", "resources.power.reason.switch_pending", "resources.power.next",
		"resources.power.wait_ac", "resources.power.wait_no_device", "resources.rail.asr_power",
		"resources.devices.battery_mode_tip", "devices.battery_hold", "devices.order_hint_battery",
		"settings.asr.multi_device_hint_battery",
		"notes.power.switched", "notes.power.suspended", "notes.power.resumed", "errors.selftest.power_battery", "errors.selftest.power_cpu",
	}
	for _, k := range powerAware {
		if s := text(t, zh, k); !strings.Contains(s, "电源感知") {
			t.Errorf("zh-CN %s does not say 电源感知: %q", k, s)
		}
		if s := text(t, en, k); !strings.Contains(strings.ToLower(s), "power-aware") {
			t.Errorf("en %s does not say power-aware: %q", k, s)
		}
	}
	exact := []struct {
		c               catalog
		name, key, want string
	}{
		{zh, "zh-CN", "resources.power.reason.desktop", "未检测到电池 · 自动"},
		{en, "en", "resources.power.reason.desktop", "No battery detected · automatic"},
		{zh, "zh-CN", "resources.power.reason.switch_pending", "电源感知 · 完成当前讲座后切换"},
		{zh, "zh-CN", "waiting.power_device_selftest", "排队：电源感知 - 等待 {device} 完成测试"},
		// the back end's catalog text (testdata/api/error_codes_backend.json)
		{zh, "zh-CN", "errors.selftest.power_cpu", "电源感知：笔记本不使用 CPU 转写，不需要测试「{device}」"},
		{zh, "zh-CN", "resources.devices.battery_mode_tip", "电源感知：使用电池时只用 NPU 转写，没有 NPU 时用 Intel 核显；独立显卡和 CPU 不接新的转写"},
		// the summary form of a power-aware wait (util.js lectureSummary)
		{zh, "zh-CN", "common.summary.pending", "{stage}：{wait}"},
		{en, "en", "common.summary.pending", "{stage}: {wait}"},
	}
	for _, x := range exact {
		if got := text(t, x.c, x.key); got != x.want {
			t.Errorf("%s %s = %q, want %q", x.name, x.key, got, x.want)
		}
	}
	// a laptop with an unknown power state is tagged "desktop" too: never say so
	if s := text(t, zh, "resources.power.reason.desktop"); strings.Contains(s, "台式机") {
		t.Errorf("zh-CN reason.desktop says 台式机: %q", s)
	}
	if s := text(t, en, "resources.power.reason.desktop"); strings.Contains(strings.ToLower(s), "desktop") {
		t.Errorf("en reason.desktop says desktop: %q", s)
	}
	// a power-aware wait already names its state ("挂起 / 排队：…"), so the
	// summary must not be the "queued" form; util.js picks the pending form
	src, err := os.ReadFile("util.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "if (w && isPowerWait(st.waiting_for, opt.res)) return { text: t('common.summary.pending'") {
		t.Error("util.js lectureSummary does not use common.summary.pending for power-aware waits")
	}
	if !strings.Contains(string(src), "singleActive(res) ? t('waiting.power_device_selftest'") {
		t.Error("util.js waitingForText does not name the power-aware self-test wait")
	}
}

// The keys the UI stopped using are kept and marked for the translators.
func TestDeprecatedKeysMarked(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	var js strings.Builder
	files, err := filepath.Glob("*.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		js.Write(b)
	}
	const mark = "已废弃，不需要翻译或修改"
	deprecated := []string{
		"settings.asr.device_mode_auto", "settings.asr.device_mode_hint", "settings.asr.device_mode_missing", "settings.asr.device_mode_disabled",
		"settings.asr.battery_aware", "settings.asr.battery_aware_hint", "settings.asr.battery_no_intel",
	}
	for _, k := range deprecated {
		if _, ok := zh[k]; !ok {
			t.Errorf("%s was deleted from zh-CN.json (keep it)", k)
		}
		if strings.Contains(js.String(), "'"+k+"'") {
			t.Errorf("%s is still used by the UI", k)
		}
		e, _ := ctx[k].(map[string]any)
		if notes, _ := e["notes"].(string); !strings.HasPrefix(notes, mark) {
			t.Errorf("context.json %s is not marked deprecated: %q", k, notes)
		}
	}
	// the back end's code with a similar name stays in use
	e, _ := ctx["errors.settings.device_mode_invalid"].(map[string]any)
	if notes, _ := e["notes"].(string); strings.HasPrefix(notes, mark) {
		t.Error("errors.settings.device_mode_invalid is marked deprecated")
	}
}
