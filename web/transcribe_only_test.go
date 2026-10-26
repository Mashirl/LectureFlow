// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/sched"
)

// "Transcribe only" (SPEC-v3 V4.2 step 4, 3.1.0; pipeline-builder): the
// front end and the back end agree on the waiting_for code, util.js names
// it, and every new text exists in zh-CN, en and context — in the catalogs,
// or (until the lead merges it) in web/i18n/_parts/v31-transcribe-only.

var transcribeOnlyKeys = []string{"waiting.no_provider", "lecture.notice.no_provider", "lecture.notice.no_provider_settings", "menu.export.vtt_en"}

// catalogOrPart is the catalog of locale with the not yet merged
// v31-transcribe-only part laid over it (absent part: the catalog alone).
func catalogOrPart(t *testing.T, locale string) catalog {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, locale+".json"))
	part := filepath.Join(catalogDir, "_parts", "v31-transcribe-only."+locale+".json")
	if _, err := os.Stat(part); err == nil {
		for k, v := range loadCatalog(t, part) {
			c[k] = v
		}
	}
	return c
}

func TestTranscribeOnlyTexts(t *testing.T) {
	zh, en, ctx := catalogOrPart(t, "zh-CN"), catalogOrPart(t, "en"), catalogOrPart(t, "context")
	for _, k := range transcribeOnlyKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		if strings.ContainsAny(text(t, zh, k)+text(t, en, k), "{}") {
			t.Errorf("%s: takes no params", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
	}
	// the setup's name, as the onboarding calls it
	if s := text(t, zh, "lecture.notice.no_provider"); !strings.HasPrefix(s, text(t, zh, "onboarding.tier.transcribe_only.name")+"：") {
		t.Errorf("zh-CN notice does not start with the setup's name: %q", s)
	}
	if s := text(t, en, "lecture.notice.no_provider"); !strings.HasPrefix(s, text(t, en, "onboarding.tier.transcribe_only.name")+":") {
		t.Errorf("en notice does not start with the setup's name: %q", s)
	}
}

func TestTranscribeOnlyWaitingCode(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := regexp.MustCompile(`export const WAIT_NO_PROVIDER = '([a-z_]+)';`).FindStringSubmatch(read("lecture_english.js"))
	if m == nil || m[1] != sched.WaitNoProvider {
		t.Fatalf("lecture_english.js WAIT_NO_PROVIDER %v, the scheduler's is %q", m, sched.WaitNoProvider)
	}
	if !strings.Contains(read("util.js"), "if (w === '"+sched.WaitNoProvider+"') return t('waiting.no_provider');") {
		t.Error("util.js waitingForText does not name the no_provider wait")
	}
	// the English-only views use the shared rule
	for _, f := range []string{"view_lecture.js", "view_transcript.js", "view_player.js"} {
		if !strings.Contains(read(f), "from './lecture_english.js'") {
			t.Errorf("%s does not use lecture_english.js", f)
		}
	}
}
