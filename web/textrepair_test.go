// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// "修复断句" (textrepair-int): the asr stage's action rerun_text_only has a
// button and a lecture menu item, asks first with a dialog that states the
// four facts the lead asked for, and is sent as the asr rerun with
// text_only; every text exists in zh-CN, en and context.

var textRepairKeys = []string{"common.action.rerun_text_only", "lecture.text_repair.title", "lecture.text_repair.message",
	"lecture.text_repair.no_transcribe", "lecture.text_repair.keep_reviewed", "lecture.text_repair.downstream",
	"lecture.text_repair.patches", "lecture.text_repair.ok", "common.model_choice.what_text_repair"}

func TestTextRepairTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range textRepairKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
	}
	// the dialog says: no new transcription; reviewed sentences kept; the
	// later stages run again and what that costs; the review's accept /
	// reject records are cleared
	dialog := ""
	for _, k := range []string{"no_transcribe", "keep_reviewed", "downstream", "patches"} {
		dialog += text(t, zh, "lecture.text_repair."+k)
	}
	for _, want := range []string{"不重新转写", "人工审阅", "保留", "摘要", "翻译", "重跑", "0.2 元", "采纳", "拒绝", "清掉"} {
		if !strings.Contains(dialog, want) {
			t.Errorf("the zh-CN dialog does not say %q: %s", want, dialog)
		}
	}
	if s := text(t, en, "lecture.text_repair.downstream"); !strings.Contains(s, "¥0.2") {
		t.Errorf("en cost: %s", s)
	}
}

func TestTextRepairAction(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	a := "'" + model.ActionRerunTextOnly + "'"
	pipeline, actions, api := read("pipeline.js"), read("actions.js"), read("api.js")
	// the stage card's button: its own confirmation and icon
	for _, want := range []string{"if (action === " + a + ") return confirmTextRepair();",
		"['no_transcribe', 'keep_reviewed', 'downstream', 'patches']", "a === " + a + " ? 'wand-sparkles'"} {
		if !strings.Contains(pipeline, want) {
			t.Errorf("pipeline.js lacks %q", want)
		}
	}
	// sent as the asr rerun with text_only, answering needs_model_choice
	if !strings.Contains(actions, "if (action === "+a+") return withModelChoice((choice) => api.lectureTextRepair(lec.id, choice), t('common.model_choice.what_text_repair'));") {
		t.Error("actions.js does not send rerun_text_only as the text_only rerun")
	}
	if !strings.Contains(api, "lectureTextRepair: (id, choice) => post(`/api/lectures/${enc(id)}/stages/asr/rerun`, { text_only: true, ...choiceBody(choice) })") {
		t.Error("api.js has no text_only rerun")
	}
	// the lecture menu item, when the back end offers it on the asr stage
	if !strings.Contains(actions, "s.stage === 'asr' && (s.allowed_actions || []).includes("+a+")") ||
		!strings.Contains(actions, "stageAction(lec, 'asr', "+a+")") {
		t.Error("actions.js has no lecture menu item for rerun_text_only")
	}
}
