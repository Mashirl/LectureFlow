// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
	"lectureflow/internal/fetch"
)

// The browser extension's recording downloads on the course page, the
// workbench and the settings page (SPEC-ext, ext-brief §6.2; web-builder
// ext-step6): every download state of the frozen interface has its label,
// the settings use the queue's limits, the section is on the settings page,
// and every text the new files use exists in zh-CN, en and context — in
// the catalogs, or (until the lead merges it) in
// web/i18n/_parts/ext-web-dl.

var downloadFiles = []string{"downloads.js", "settings_downloads.js"}

// catalogWithDL is the catalog of locale with the not yet merged
// ext-web-dl part laid over it (absent part: the catalog alone).
func catalogWithDL(t *testing.T, locale string) catalog {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, locale+".json"))
	part := filepath.Join(catalogDir, "_parts", "ext-web-dl."+locale+".json")
	if _, err := os.Stat(part); err == nil {
		for k, v := range loadCatalog(t, part) {
			c[k] = v
		}
	}
	return c
}

func readWeb(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// literalKeys are the keys passed to t('…') in a file.
func literalKeys(t *testing.T, name string) []string {
	t.Helper()
	stripped, _ := lexJS(readWeb(t, name))
	seen := map[string]bool{}
	var out []string
	for _, m := range tCallRe.FindAllStringSubmatch(stripped, -1) {
		if k := m[1] + m[2] + m[3]; !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestDownloadsTexts(t *testing.T) {
	zh, en, ctx := catalogWithDL(t, "zh-CN"), catalogWithDL(t, "en"), catalogWithDL(t, "context")
	keys := []string{"errors.downloads.not_found", "errors.downloads.active", "errors.downloads.needs_refresh"}
	for _, f := range downloadFiles {
		keys = append(keys, literalKeys(t, f)...)
	}
	if len(keys) < 30 {
		t.Fatalf("only %d keys found in %v", len(keys), downloadFiles)
	}
	for _, k := range keys {
		zv, zok := zh[k]
		ev, eok := en[k]
		if !zok || !eok {
			t.Errorf("%s: missing text (zh-CN %v, en %v)", k, zok, eok)
			continue
		}
		zp, err1 := paramSets(zv)
		ep, err2 := paramSets(ev)
		if err1 != nil || err2 != nil {
			t.Errorf("%s: %v %v", k, err1, err2)
			continue
		}
		want := strings.Join(zp[""], ",")
		for form, ps := range ep {
			if got := strings.Join(ps, ","); got != want {
				t.Errorf("%s: en%s has {%s}, zh-CN {%s}", k, map[bool]string{true: "", false: "." + form}[form == ""], got, want)
			}
		}
		// new keys explain themselves to the translators; the shared ones
		// (common.*) only need to exist
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || (c["notes"] == nil && !strings.HasPrefix(k, "common.")) {
			t.Errorf("%s: no context entry with where and notes", k)
		}
	}
	// the wording the spec gives (SPEC-ext, ext-brief §6.2)
	if s := text(t, zh, "downloads.needs_refresh.hint"); s != "在浏览器里重新打开这节课的录播页，点一下 LectureFlow 扩展即可续传" {
		t.Errorf("needs_refresh hint: %q", s)
	}
	if s := text(t, zh, "settings.downloads.connections_cap"); s != "实际不会超过录播平台建议的上限" {
		t.Errorf("connections cap note: %q", s)
	}
	// the error codes the HTTP layer answers take no parameters in the text
	for _, k := range keys[:3] {
		if strings.ContainsAny(text(t, zh, k)+text(t, en, k), "{}") {
			t.Errorf("%s: the text must not use params", k)
		}
	}
}

func TestDownloadsStates(t *testing.T) {
	src := readWeb(t, "downloads.js")
	for _, s := range []string{bridge.StateQueued, bridge.StateDownloading, bridge.StateMerging, bridge.StateImporting, bridge.StateDone, bridge.StateFailed, bridge.StateNeedsRefresh, bridge.StateCancelled} {
		if !strings.Contains(src, "case '"+s+"':\n      return t('downloads.state."+s+"');") {
			t.Errorf("stateLabel does not name the state %q", s)
		}
	}
	// the main program's status tags (SPEC-ext 界面设计)
	if !strings.Contains(src, "const CHIP = { ok: 'chip-ok', run: 'chip-run', warn: 'chip-warn', bad: 'chip-err', idle: 'chip-quiet' };") {
		t.Error("the tones ok / run / warn / bad / idle are not mapped to the chips")
	}
	// the buttons are exactly the allowed actions of internal/fetch
	for _, a := range []string{fetch.ActionCancel, fetch.ActionRetry, fetch.ActionRemove} {
		if !strings.Contains(src, "acts.includes('"+a+"')") {
			t.Errorf("no button for the allowed action %q", a)
		}
	}
	// done is not shown on the course page
	if !strings.Contains(src, ".filter((d) => d.state !== 'done')") {
		t.Error("the course rows do not leave out finished downloads")
	}
}

func TestDownloadsSettings(t *testing.T) {
	src := readWeb(t, "settings_downloads.js")
	limit := func(name string, min, max, def int) {
		t.Helper()
		re := regexp.MustCompile(`export const ` + name + ` = \{ min: (\d+), max: (\d+), def: (\d+) \};`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("settings_downloads.js has no %s", name)
		}
		got := []int{}
		for _, s := range m[1:] {
			n, _ := strconv.Atoi(s)
			got = append(got, n)
		}
		if got[0] != min || got[1] != max || got[2] != def {
			t.Errorf("%s = %v, the queue's is [%d %d %d]", name, got, min, max, def)
		}
	}
	limit("CONCURRENCY", fetch.MinConcurrency, fetch.MaxConcurrency, fetch.DefaultConcurrency)
	limit("CONNECTIONS", fetch.MinConnections, fetch.MaxConnections, fetch.DefaultConnections)
	for _, f := range []string{"download_concurrency", "download_connections"} {
		if !strings.Contains(src, "patch('"+f+"', Number(") {
			t.Errorf("the section does not save %s", f)
		}
	}
	page := readWeb(t, "view_settings.js")
	for _, want := range []string{"['downloads', 'download', 'settings.downloads.title'],", "'downloads']);", "downloads: dlUI", "dlUI.load();", "dlUI.onSettingsChanged();", "dlUI.destroy();"} {
		if !strings.Contains(page, want) {
			t.Errorf("view_settings.js does not mount the downloads section: %q missing", want)
		}
	}
}
