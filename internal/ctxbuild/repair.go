// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/model"
)

// RepairChunk is one repair request (SPEC §6.4, §13.4).
type RepairChunk struct {
	System string // fixed prefix, identical for every chunk of a lecture
	User   string
	// Start, End delimit the chunk's target ("main") sentences: idx in
	// [Start, End). Patches with any other idx must be dropped; a sentence is
	// the target of exactly one chunk.
	Start, End int
}

// Contains reports whether idx is a target sentence of this chunk.
func (c RepairChunk) Contains(idx int) bool { return idx >= c.Start && idx < c.End }

// RepairSystem is the repair prefix (SPEC §6.4): system prompt + enabled
// glossary in the two sections of SPEC-v2 G5.1 + summary + slides
// (truncated to SlidesMaxChars).
func RepairSystem(lec LectureSnapshot) string {
	return fill(RepairSystemTemplate,
		"glossary_lines", GlossaryLines(lec.Terms),
		"summary", summaryOrNone(lec.Summary),
		"slides_text", LectureSlides(lec))
}

// BuildRepairRequest splits the lecture into chunks of RepairChunkSize target
// sentences. Each chunk also carries up to RepairOverlap sentences of the
// previous chunk as CONTEXT (before) and up to RepairOverlap sentences of the
// next chunk as CONTEXT (after); overlap sentences only serve as context.
// Lines are "[idx] asr_text" with low-confidence words marked ⟦word⟧; an
// empty context section is "(none)" (SPEC §13.4).
func BuildRepairRequest(lec LectureSnapshot) []RepairChunk {
	ss := lec.Sentences
	if len(ss) == 0 {
		return nil
	}
	system := RepairSystem(lec)
	var out []RepairChunk
	for start := 0; start < len(ss); start += RepairChunkSize {
		end := min(start+RepairChunkSize, len(ss))
		before := repairLines(ss[max(0, start-RepairOverlap):start])
		target := repairLines(ss[start:end])
		after := repairLines(ss[end:min(len(ss), end+RepairOverlap)])
		out = append(out, RepairChunk{
			System: system,
			User: fill(RepairUserTemplate,
				"overlap_before_lines", before,
				"target_lines", target,
				"overlap_after_lines", after),
			Start: ss[start].Idx,
			End:   ss[end-1].Idx + 1,
		})
	}
	return out
}

func repairLines(ss []Sentence) string {
	if len(ss) == 0 {
		return None
	}
	lines := make([]string, len(ss))
	for i, s := range ss {
		lines[i] = fmt.Sprintf("[%d] %s", s.Idx, MarkLowConf(s.ASRText, s.LowConfWords))
	}
	return strings.Join(lines, "\n")
}

// MarkLowConf wraps the low-confidence words of an ASR sentence in ⟦ ⟧.
//
// Words are located by their index i among the whitespace-separated tokens
// of text (SPEC §6.2: i is the word's position in the sentence). The token
// at i is used only when it equals the word once leading/trailing
// punctuation is removed from both (case-insensitive). Otherwise (whisper
// sometimes emits sub-words without a leading space, which shifts the count)
// the nearest token that equals the word in the same way is used, ties to
// the earlier one; with no such token the word stays unmarked. Matching is
// always on whole tokens, never inside another word. The token without its
// surrounding punctuation is marked ("⟦cash⟧,").
func MarkLowConf(text string, words []model.LowConfWord) string {
	if len(words) == 0 {
		return oneLine(text)
	}
	spans := tokenSpans(text)
	tok := func(j int) string { return text[spans[j][0]:spans[j][1]] }
	marks := map[int]bool{} // token indexes to mark
	for _, w := range words {
		bare := trimPunct(strings.TrimSpace(w.W))
		if bare == "" {
			continue
		}
		// whole-token match only: never inside another word ("c⟦a⟧sh")
		match := func(j int) bool { return strings.EqualFold(trimPunct(tok(j)), bare) }
		ti := -1
		if w.I >= 0 && w.I < len(spans) && match(w.I) {
			ti = w.I
		} else {
			for j := range spans {
				if match(j) && (ti < 0 || abs(j-w.I) < abs(ti-w.I)) {
					ti = j
				}
			}
		}
		if ti >= 0 {
			marks[ti] = true
		}
	}
	var b strings.Builder
	last := 0
	for j, sp := range spans {
		if !marks[j] {
			continue
		}
		t := tok(j)
		start := strings.IndexFunc(t, notPunct)
		end := strings.LastIndexFunc(t, notPunct)
		_, size := utf8.DecodeRuneInString(t[end:])
		end += size
		b.WriteString(text[last:sp[0]])
		b.WriteString(t[:start])
		b.WriteString("⟦")
		b.WriteString(t[start:end])
		b.WriteString("⟧")
		b.WriteString(t[end:])
		last = sp[1]
	}
	b.WriteString(text[last:])
	return oneLine(b.String())
}

// tokenSpans returns the byte ranges of the whitespace-separated tokens.
func tokenSpans(s string) [][2]int {
	var spans [][2]int
	start := -1
	for i, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				spans = append(spans, [2]int{start, i})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		spans = append(spans, [2]int{start, len(s)})
	}
	return spans
}

func trimPunct(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func notPunct(r rune) bool { return !unicode.IsPunct(r) && !unicode.IsSymbol(r) }
