// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/nmhost"
)

// ext web3 (web-builder, 2026-10-27): the player's second angle (picture
// in picture), "添加第二视角" on the lecture page, chapters in the transcript
// stream and on the timeline, and the diagnostics rows aria2c and
// browser_ext. Every text the files use exists in zh-CN, en and context —
// in the catalogs or (until the lead merges it) in
// web/i18n/_parts/ext-web3 —, the states and browsers of internal/nmhost
// have their texts, the requests are the frozen routes, and the pieces are
// wired into the pages.

var web3Files = []string{"player_angles.js", "lecture_angles.js", "system_ext.js", "player_transcript.js", "view_player.js", "player_keys.js", "view_system.js"}

const web3Part = "ext-web3"

// nmStates are the browser states of nmhost.Status (blocked and not_ready
// are macOS's, mac-fix).
var nmStates = []string{nmhost.StateRegistered, nmhost.StateNotRegistered, nmhost.StateHostMissing, nmhost.StateNotInstalled,
	nmhost.StateBlocked, nmhost.StateNotReady}

// catalogWithWeb3 is the catalog of locale with the not yet merged ext-*
// parts laid over it (core3's has the rows' names and texts), and this
// task's part alone (empty once merged).
func catalogWithWeb3(t *testing.T, locale string) (catalog, catalog) {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, locale+".json"))
	part := catalog{}
	files, _ := filepath.Glob(filepath.Join(catalogDir, "_parts", "ext-*."+locale+".json"))
	for _, f := range files {
		p := loadCatalog(t, f)
		for k, v := range p {
			c[k] = v
		}
		if filepath.Base(f) == web3Part+"."+locale+".json" {
			part = p
		}
	}
	return c, part
}

func TestWeb3Texts(t *testing.T) {
	zh, zhPart := catalogWithWeb3(t, "zh-CN")
	en, enPart := catalogWithWeb3(t, "en")
	ctx, ctxPart := catalogWithWeb3(t, "context")
	var keys []string
	for _, f := range web3Files {
		keys = append(keys, literalKeys(t, f)...)
	}
	// keys built from tables (system_ext.js) and the group title
	for _, s := range nmStates {
		keys = append(keys, "system.ext.state."+s)
	}
	for _, b := range nmhost.Browsers {
		keys = append(keys, "system.ext.browser."+b)
	}
	keys = append(keys, "system.group.ext")
	if len(keys) < 80 {
		t.Fatalf("only %d keys found in %v", len(keys), web3Files)
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
		// a plural text has its forms ("other" in zh-CN): all say the same params
		var want string
		for _, ps := range zp {
			want = strings.Join(ps, ",")
		}
		for form, ps := range ep {
			if got := strings.Join(ps, ","); got != want {
				t.Errorf("%s: en %q has {%s}, zh-CN {%s}", k, form, got, want)
			}
		}
		if _, ok := ctx[k]; !ok {
			t.Errorf("%s: no context entry", k)
		}
	}
	if len(zhPart) == 0 && len(ctxPart) == 0 {
		return // merged into the catalogs
	}
	for k := range zhPart {
		if _, ok := enPart[k]; !ok {
			t.Errorf("%s: in the zh-CN part, not in the en part", k)
		}
		c, ok := ctxPart[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes in the part", k)
		}
	}
	for k := range enPart {
		if _, ok := zhPart[k]; !ok {
			t.Errorf("%s: in the en part, not in the zh-CN part", k)
		}
	}
	for k := range ctxPart {
		if _, ok := zhPart[k]; !ok {
			t.Errorf("%s: in the context part, not in the zh-CN part", k)
		}
	}
	// the user's words (NOTES 2026-10-27): the second video is a "视角"
	for _, k := range []string{"lecture.angle.add", "player.angle.name_2"} {
		if !strings.Contains(text(t, zh, k), "第二视角") {
			t.Errorf("%s: %q does not say 第二视角", k, text(t, zh, k))
		}
	}
	// recordings are never "downloaded" in these texts (NOTES ext 文案 1)
	for k := range zhPart {
		if strings.Contains(text(t, zh, k), "下载") {
			t.Errorf("%s: %q says 下载", k, text(t, zh, k))
		}
	}
}

// system_ext.js shows every state, browser and listen error of
// internal/nmhost; the diagnostics page groups the two rows.
func TestWeb3Diagnostics(t *testing.T) {
	src := readWeb(t, "system_ext.js")
	for _, s := range nmStates {
		if !strings.Contains(src, "  "+s+": ['chip-") {
			t.Errorf("system_ext.js has no chip for the state %q", s)
		}
	}
	for _, b := range nmhost.Browsers {
		if !strings.Contains(src, "  "+b+": 'system.ext.browser."+b+"'") {
			t.Errorf("system_ext.js has no name for the browser %q", b)
		}
	}
	// the fields of nmhost.Status it reads
	for _, f := range []string{"managed", "browsers", "last_connection", "last_register", "dev_extension"} {
		if !strings.Contains(src, "st."+f) {
			t.Errorf("system_ext.js does not read %q", f)
		}
	}
	if !strings.Contains(src, "c.extension_version") || !strings.Contains(src, "r.error") || !strings.Contains(src, "b.manifest") {
		t.Error("system_ext.js does not show the extension version, the registration's error or the manifest")
	}
	// nm-harden: the development extension's reasons (nmhost.DevExtension*)
	for _, why := range []string{nmhost.DevExtensionBuild, nmhost.DevExtensionEnv} {
		if !strings.Contains(src, "  "+why+": 'system.ext.dev_extension_"+why+"'") {
			t.Errorf("system_ext.js has no text for the development extension's reason %q", why)
		}
	}
	// macOS (mac-fix): what a registration could not do has its own lines
	for _, want := range []string{"r.blocked", "r.not_ready", "'system.ext.register_blocked'", "'system.ext.register_not_ready'"} {
		if !strings.Contains(src, want) {
			t.Errorf("system_ext.js lacks %s", want)
		}
	}
	// the row sentences that already name the connection are the server's
	api, err := os.ReadFile(filepath.Join("..", "internal", "api", "system_ext.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range regexp.MustCompile(`'(check\.browser_ext_[a-z_]+)'`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(string(api), `"`+code[1]+`"`) {
			t.Errorf("system_ext.js names %s, which internal/api/system_ext.go does not send", code[1])
		}
	}
	sys := readWeb(t, "view_system.js")
	for _, want := range []string{
		"['env', 'layers', (n) => ['data_dir', 'credentials', 'data_dir_location', 'ffmpeg', 'aria2c',",
		"['ext', 'plug', (n) => n === 'browser_ext'],",
		"aria2c: () => t('common.check.aria2c'),",
		"browser_ext: () => t('common.check.browser_ext'),",
		"c.name === 'browser_ext' ? extBlock(c) : null,",
		"ext.setRow(c);",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("view_system.js lacks %q", want)
		}
	}
}

// The requests are the frozen routes: the angle file and adding one (the
// lead's interface; the mock serves them), the nmhost routes of
// handlers_nmhost.go.
func TestWeb3Routes(t *testing.T) {
	src := readWeb(t, "api_ext.js")
	b, err := os.ReadFile(filepath.Join("..", "internal", "api", "handlers_nmhost.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`h\("([A-Z]+) (/api/nmhost[^"]*)"`).FindAllStringSubmatch(string(b), -1) {
		if !strings.Contains(src, "request('"+m[1]+"', '"+m[2]+"')") {
			t.Errorf("api_ext.js does not call %s %s", m[1], m[2])
		}
	}
	for _, want := range []string{
		"`/api/lectures/${enc(id)}/angles/${enc(name)}`",
		"request('POST', `/api/lectures/${enc(id)}/angles`, { path })",
		"request('POST', `/api/lectures/${enc(id)}/angles`, form)",
		"form.append('file', file, file.name);",
		"request('DELETE', `/api/lectures/${enc(id)}/angles/${enc(name)}`)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("api_ext.js lacks %s", want)
		}
	}
	mock, err := os.ReadFile(filepath.Join("..", "internal", "api", "mockserve", "ext_web3.go"))
	if err != nil {
		t.Fatal(err)
	}
	angles, err := os.ReadFile(filepath.Join("..", "internal", "api", "handlers_angles.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"GET /api/lectures/{id}/angles/{name}", "POST /api/lectures/{id}/angles", "DELETE /api/lectures/{id}/angles/{name}", "GET /api/nmhost", "POST /api/nmhost/register"} {
		if !strings.Contains(string(mock), `"`+r+`"`) {
			t.Errorf("the mock does not serve %s", r)
		}
		if strings.Contains(r, "angles") && !strings.Contains(string(angles), `"`+r+`"`) {
			t.Errorf("the server does not serve %s (handlers_angles.go)", r)
		}
	}
}

// The player is wired: the second angle's controller, its bar button and
// box, V, the click on the picture; the transcript gets the chapters and
// the current chapter's name opens the list. The lecture page offers the
// second angle.
func TestWeb3Wiring(t *testing.T) {
	pl := readWeb(t, "view_player.js")
	for _, want := range []string{
		"import { createAngles, lectureAngles } from './player_angles.js';",
		"frame.after(angles.box);",
		"sideBtn, angles.button, theaterBtn",
		"angles.attach(media, frame);",
		"angles.detach();",
		"angles.setAngles(lectureAngles(l));",
		"angles.setAngles(lectureAngles(lec));",
		"angles.layout(fr, { W, H, overlay });",
		"e.target === angles.pictureEl()",
		"angle: () => angles.swap(),",
		"!angles.mainHidden()",
		"angles.destroy();",
		"transcript.setChapters(chapters.chapters(),",
		"onclick: () => chapters.openList(chapterNow)",
	} {
		if !strings.Contains(pl, want) {
			t.Errorf("view_player.js lacks %q", want)
		}
	}
	if n := strings.Count(pl, "syncTranscriptChapters();"); n < 3 {
		t.Errorf("the transcript's chapters are synced %d times (chapters, display, English only)", n)
	}
	keys := readWeb(t, "player_keys.js")
	if !strings.Contains(keys, "case 'v':\n        done = !!(hd.angle && hd.angle());") || !strings.Contains(keys, "opts.angles ? [['V'], 'player.angle.help'] : null,") {
		t.Error("player_keys.js: V does not swap the angles, or the help does not list it")
	}
	lec := readWeb(t, "view_lecture.js")
	if strings.Count(lec, "menuItems(lec") < 1 || !strings.Contains(lec, "angleMenuItems(lec, (l) => handlers.onLectures([l]))") || !strings.Contains(lec, "bindContextMenu(headEl, () => (lecture ? menuItems(lecture) : null));") {
		t.Error("view_lecture.js does not offer the second angle in its menus")
	}
	la := readWeb(t, "lecture_angles.js")
	for _, want := range []string{
		"if (!angles.length) return ['sep', { id: 'add_angle'",
		"{ id: 'replace_angle'",
		"{ id: 'remove_angle'",
		"const ok = await confirmDialog(",
		"if (DESKTOP) {",
		"send = () => addAngle(lec.id, path);",
		"send = () => uploadAngle(lec.id, file);",
	} {
		if !strings.Contains(la, want) {
			t.Errorf("lecture_angles.js lacks %q", want)
		}
	}
	// the second angle is muted, paused in the box and refreshed at most
	// about twice a second; drift beyond 0.3 s is corrected
	an := readWeb(t, "player_angles.js")
	for _, want := range []string{"v.muted = true;", "const REFRESH_MS = 600;", "const DRIFT_SEEK = 0.3;", "if (!a2.paused) a2.pause();"} {
		if !strings.Contains(an, want) {
			t.Errorf("player_angles.js lacks %q", want)
		}
	}
	// the classes the files draw are styled
	css := readWeb(t, "style.css")
	for _, cls := range []string{".pl-pip ", ".pl-pip-swap ", ".pl-pip-close ", ".pl-pip-label ", ".pl-chap ", ".pl-chap-fold ", ".pl-chap-go ", ".pl-chap-count ", ".pl-fold-all.is-on", ".pl-tl-gap ", ".pl-tl-seg ", ".pl-chapter-now-text ", ".sys-ext ", ".sys-ext-browser ", ".sys-ext-line ", ".sys-ext-actions "} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css has no rule for %s", strings.TrimSpace(cls))
		}
	}
	if strings.Contains(css, ".pl-tl-chapter ") {
		t.Error("style.css still styles the old chapter ticks")
	}
}
