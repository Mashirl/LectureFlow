// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/subs"
)

// TestCueSentences (lead ruling 8 of phase 2): the English is text, never
// asr_text; words are carried over; zh only when not blank and zh_status is
// not none (qa_failed and stale translations are shown).
func TestCueSentences(t *testing.T) {
	zh := func(s string) *string { return &s }
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 900, ASRText: "the cash", Text: "The cache.", Zh: zh(" 缓存。 "), ZhStatus: model.ZhOK,
			Words: []model.Word{{W: "the", S: 0, E: 300}, {W: "cash", S: 350, E: 900}}},
		{Idx: 1, Text: "Not translated.", Zh: zh("旧的"), ZhStatus: model.ZhNone},
		{Idx: 2, Text: "QA failed.", Zh: zh("质检未通过的译文"), ZhStatus: model.ZhQAFailed},
		{Idx: 3, Text: "Stale.", Zh: zh("过期译文"), ZhStatus: model.ZhStale},
		{Idx: 4, Text: "Blank.", Zh: zh("  "), ZhStatus: model.ZhOK},
		{Idx: 5, Text: "Nil.", ZhStatus: model.ZhOK},
	}
	got := CueSentences(ss)
	if len(got) != len(ss) {
		t.Fatalf("%d sentences", len(got))
	}
	if g := got[0]; g.Text != "The cache." || g.ASRText != "the cash" || g.Zh != "缓存。" || g.Idx != 0 || g.EndMS != 900 ||
		len(g.Words) != 2 || g.Words[1] != (subs.Word{W: "cash", S: 350, E: 900}) {
		t.Errorf("sentence 0: %+v", g)
	}
	for i, want := range []string{"缓存。", "", "质检未通过的译文", "过期译文", "", ""} {
		if got[i].Zh != want {
			t.Errorf("sentence %d: zh %q, want %q", i, got[i].Zh, want)
		}
	}
	if got[1].Words != nil {
		t.Error("no words → nil (character interpolation, P4.2)")
	}
}

// vttCue is one parsed WebVTT cue.
type vttCue struct {
	id         string
	start, end string
	lines      []string
}

func parseVTT(t *testing.T, path string) []vttCue {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(strings.TrimSuffix(string(b), "\n\n"), "\n\n")
	if blocks[0] != "WEBVTT" {
		t.Fatalf("%s: header %q", path, blocks[0])
	}
	var out []vttCue
	for _, bl := range blocks[1:] {
		ls := strings.Split(bl, "\n")
		start, end, ok := strings.Cut(ls[1], " --> ")
		if len(ls) < 3 || !ok {
			t.Fatalf("%s: bad cue %q", path, bl)
		}
		out = append(out, vttCue{id: ls[0], start: start, end: end, lines: ls[2:]})
	}
	return out
}

// TestExportVTTIsTheCues (SPEC-v2 P4.6): bilingual.vtt has exactly the cues
// of subs.SegmentLecture (English first, then Chinese); zh.vtt joins the
// cues of one sentence that show the same Chinese, so an untranslated
// sentence is one MissingZh cue.
func TestExportVTTIsTheCues(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("Cues", "m.mp4")
	words := []model.Word{}
	for i, w := range strings.Fields(longEN) {
		words = append(words, model.Word{W: w, S: int64(1000 + i*600), E: int64(1000 + i*600 + 500)})
	}
	ss := []model.Sentence{
		{Idx: 0, StartMS: 1000, EndMS: words[len(words)-1].E, ASRText: longEN, RepairedText: longEN, Words: words},
		{Idx: 1, StartMS: 23000, EndMS: 45000, ASRText: longEN, RepairedText: longEN}, // untranslated, no words
		{Idx: 2, StartMS: 45500, EndMS: 47000, ASRText: "Short one.", RepairedText: "Short one."},
	}
	for i := range ss {
		ss[i].LectureID, ss[i].ZhStatus = l.ID, model.ZhNone
	}
	e.insertSentences(l.ID, ss)
	for idx, zh := range map[int]string{0: longZh, 2: "短句。"} {
		z := zh
		e.must(e.st.SetTranslation(e.ctx, l.ID, idx, db.SentenceTr{Zh: &z, ZhStatus: model.ZhOK}))
	}
	if r := e.run(e.stage(e.deps(nil), model.StageExport), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	cues := subs.SegmentLecture(CueSentences(e.sentences(l.ID)))
	if len(cues) < 6 {
		t.Fatalf("only %d cues: the long sentences must be split", len(cues))
	}
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	names := e.exportNames(l)

	bi := parseVTT(t, filepath.Join(dir, names[2]))
	if len(bi) != len(cues) {
		t.Fatalf("bilingual.vtt has %d cues, SegmentLecture %d", len(bi), len(cues))
	}
	for i, c := range cues {
		zh := c.Zh
		if zh == "" {
			zh = MissingZh
		}
		want := vttCue{id: strconv.Itoa(i + 1), start: vttTime(c.StartMS), end: vttTime(c.EndMS), lines: []string{c.En, zh}}
		if g := bi[i]; g.id != want.id || g.start != want.start || g.end != want.end || strings.Join(g.lines, "|") != strings.Join(want.lines, "|") {
			t.Errorf("bilingual cue %d: %+v, want %+v", i, g, want)
		}
	}

	zh := parseVTT(t, filepath.Join(dir, names[1]))
	// expected: consecutive cues of one sentence with the same Chinese merged
	var want []vttCue
	for i := 0; i < len(cues); {
		j := i + 1
		for j < len(cues) && cues[j].SentenceIdx == cues[i].SentenceIdx && cues[j].Zh == cues[i].Zh && cues[j].StartMS == cues[j-1].EndMS {
			j++
		}
		text := cues[i].Zh
		if text == "" {
			text = MissingZh
		}
		want = append(want, vttCue{id: strconv.Itoa(len(want) + 1), start: vttTime(cues[i].StartMS), end: vttTime(cues[j-1].EndMS), lines: []string{text}})
		i = j
	}
	if len(zh) != len(want) {
		t.Fatalf("zh.vtt has %d cues, want %d", len(zh), len(want))
	}
	missing := 0
	for i := range want {
		if g := zh[i]; g.id != want[i].id || g.start != want[i].start || g.end != want[i].end || strings.Join(g.lines, "|") != want[i].lines[0] {
			t.Errorf("zh cue %d: %+v, want %+v", i, g, want[i])
		}
		if zh[i].lines[0] == MissingZh {
			missing++
		}
	}
	if missing != 1 {
		t.Errorf("%d MissingZh cues in zh.vtt, want 1 (one untranslated sentence)", missing)
	}
	// the translated long sentence is split in zh.vtt as well, without
	// losing or repeating any of its Chinese
	var s0 []string
	for i := range want {
		if strings.Contains(longZh, zh[i].lines[0]) {
			s0 = append(s0, zh[i].lines[0])
		}
	}
	if len(s0) < 2 || strings.Join(s0, "") != longZh {
		t.Errorf("sentence 0 in zh.vtt: %q", s0)
	}
}
