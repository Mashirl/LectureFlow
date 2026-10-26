// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

// English subtitles and transcript on demand (3.1.0 "只转写", SPEC-v3 V4.2
// step 4: "只做转写、播放与字幕"; pipeline-builder, NOTES.md): the lecture
// page offers an English .vtt and .txt as soon as asr is done, generated
// from the sentences when downloaded, without the export stage (which still
// runs only after translate). The subtitle cues are the player's
// (subs.SegmentLecture over CueSentences, like GET /cues and the export
// VTTs, P4.6); the text file is the export's .en.txt format.

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/subs"
)

// Endings of the on-demand English files ("{number} {English title}.en.vtt").
const (
	EnglishVTTSuffix = ".en.vtt"
	EnglishTxtSuffix = ".en.txt"
)

// EnglishTitle is the title part of the English files' names (SPEC-v2
// P13.6 "{编号} {英文标题}"): the lecture title, or without one the
// uploaded file's name without its extension (the stored media's name for
// lectures from before v2).
func EnglishTitle(l model.Lecture) string {
	if t := strings.TrimSpace(l.Title); t != "" {
		return t
	}
	name := l.OriginalFilename
	if name == "" {
		name = filepath.Base(l.MediaPath)
	}
	return strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
}

// EnglishFileName is the download name of an on-demand English file:
// ExportBase(label, EnglishTitle(l)) + suffix; label is the lecture's
// citation label (ExportLabel), "" when it cannot be computed.
func EnglishFileName(label string, l model.Lecture, suffix string) string {
	return ExportBase(label, EnglishTitle(l)) + suffix
}

// WriteEnglishVTT writes the English subtitles of sentences (idx order): a
// WebVTT file with the player's cues, English only; a cue without English
// (a sentence that has only a translation) is left out.
func WriteEnglishVTT(w io.Writer, ss []model.Sentence) error {
	b := bufio.NewWriter(w)
	b.WriteString("WEBVTT\n\n")
	n := 0
	for _, c := range subs.SegmentLecture(CueSentences(ss)) {
		en := cueText(c.En)
		if en == "" {
			continue
		}
		n++
		fmt.Fprintf(b, "%d\n%s --> %s\n%s\n\n", n, vttTime(c.StartMS), vttTime(c.EndMS), en)
	}
	return b.Flush()
}

// WriteEnglishText writes the English transcript of sentences (idx order):
// one sentence per line, the format of the export's .en.txt (SPEC §6.6).
func WriteEnglishText(w io.Writer, ss []model.Sentence) error {
	b := bufio.NewWriter(w)
	for _, s := range ss {
		fmt.Fprintf(b, "%s\n", oneLine(s.Text))
	}
	return b.Flush()
}
