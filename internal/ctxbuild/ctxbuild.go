// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package ctxbuild builds every LLM request and input hash from in-memory
// snapshots (SPEC §6, §7, §12, §13).
//
// It is pure: no database, files, network, clock or randomness (SPEC §1.1).
// The same inputs always produce byte-identical outputs, so the fixed
// prefixes (system messages) hit the provider's context cache.
package ctxbuild

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"lectureflow/internal/model"
)

// Size limits in characters (runes), SPEC §6.0.1, §6.3, §13.
const (
	SyllabusMaxChars   = 40000 // glossary_seed input
	SlidesMaxChars     = 30000 // {slides_text}
	MaterialChunkChars = 20000 // material_terms chunk size
	RepairChunkSize    = 40    // target sentences per repair request
	RepairOverlap      = 5     // context sentences taken from each neighbour chunk
)

// Term limits stated in the §13.1 / §13.2 prompts ("Return at most N terms,
// most subject-specific first"). The prompt is a request, so the stages also
// keep only the first N of an answer (lead decision, NOTES.md).
const (
	GlossarySeedMaxTerms  = 150
	MaterialTermsMaxTerms = 100
)

// Sentence is the part of a sentence row that prompts and hashes depend on.
type Sentence struct {
	Idx          int
	Text         string // sentence.text (human_text or repaired_text)
	ASRText      string
	LowConfWords []model.LowConfWord
}

// LectureSnapshot is everything lecture-level prompts depend on. Callers load
// it from the database; ctxbuild never does.
type LectureSnapshot struct {
	LectureID string
	// Sentences ordered by idx; idx is 0-based and contiguous (SPEC §3.2),
	// so Sentences[i].Idx == i.
	Sentences []Sentence
	// Terms of the course. Disabled terms are ignored everywhere.
	Terms []model.Term
	// Summary text (summary.text); "" before the summary stage.
	Summary string
	// Materials of the course (any order). Only those bound to LectureID or
	// course-wide (LectureID == nil) are used, in upload order (SPEC §13).
	Materials []model.Material
}

// Request is one system + user message pair (SPEC §12).
type Request struct {
	System string
	User   string
}

// FromModel converts sentence rows (ordered by idx) to snapshot sentences.
func FromModel(ss []model.Sentence) []Sentence {
	out := make([]Sentence, len(ss))
	for i, s := range ss {
		out[i] = Sentence{Idx: s.Idx, Text: s.Text, ASRText: s.ASRText, LowConfWords: s.LowConfWords}
	}
	return out
}

// SelectMaterials returns the materials used for a lecture's {slides_text}:
// those bound to lectureID plus those not bound to a lecture, sorted by
// upload time (created_at, then id for a stable order). Since slides can be
// bound to a teaching week (lecture kinds), callers pass the lecture's
// material set (state.LoadSnapshot: bound to it ∪ bound to its week ∪
// course-wide), never all materials of the course: a material bound to
// another week has no lecture id either.
func SelectMaterials(all []model.Material, lectureID string) []model.Material {
	var out []model.Material
	for _, m := range all {
		if m.LectureID == nil || *m.LectureID == lectureID {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// BoundMaterialText is the text of the materials bound to lectureID (not the
// course-wide ones), in upload order, every page followed by "\n" — the
// same joining as the course-wide freq text of MergeTerms, so a term found
// only in these materials has a lecture count equal to its freq. Used for
// the whisper prompt order (SPEC-v2 G4). "" without bound material.
// BoundMaterialTextWeek also takes the materials bound to the lecture's
// teaching week.
func BoundMaterialText(all []model.Material, lectureID string) string {
	return BoundMaterialTextWeek(all, lectureID, 0)
}

// BoundMaterialTextWeek is BoundMaterialText over the materials bound to
// lectureID or to its teaching week `week` (lecture kinds, lead freeze
// 1.5: the lecture's material set without the course-wide part; week 0 =
// the week cannot be computed, so no week-bound material), in upload order.
// The asr stage orders the whisper prompt by it (lead ruling for K2: the
// slides of the lecture's week count as its own).
func BoundMaterialTextWeek(all []model.Material, lectureID string, week int) string {
	var bound []model.Material
	for _, m := range all {
		switch {
		case m.LectureID != nil:
			if *m.LectureID == lectureID {
				bound = append(bound, m)
			}
		case m.BindWeek != nil && week > 0 && *m.BindWeek == week:
			bound = append(bound, m)
		}
	}
	var b strings.Builder
	for _, m := range SelectMaterials(bound, lectureID) {
		for _, p := range m.PagesMD {
			b.WriteString(p)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// SlidesText formats {slides_text} (SPEC §13) from materials in the given
// order: each page is preceded by "--- page {n} ---", where n is the 1-based
// page number inside its own PDF; empty pages keep their header; the whole
// text is truncated to SlidesMaxChars characters. Materials not yet extracted
// (PagesMD == nil) contribute nothing. Returns "(none)" when there is no page.
func SlidesText(materials []model.Material) string {
	var b strings.Builder
	for _, m := range materials {
		for i, p := range m.PagesMD {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "--- page %d ---\n%s", i+1, strings.TrimSpace(p))
		}
	}
	if b.Len() == 0 {
		return None
	}
	return truncateRunes(b.String(), SlidesMaxChars)
}

// LectureSlides is SlidesText(SelectMaterials(lec.Materials, lec.LectureID)).
func LectureSlides(lec LectureSnapshot) string {
	return SlidesText(SelectMaterials(lec.Materials, lec.LectureID))
}

func summaryOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return None
	}
	return s
}

// GlossarySeedRequest builds the glossary_seed request (SPEC §6.0.1, §13.1);
// the syllabus is truncated to SyllabusMaxChars characters.
func GlossarySeedRequest(syllabus string) Request {
	return Request{
		System: GlossarySeedSystem,
		User:   fill(GlossarySeedUserTemplate, "syllabus_text", truncateRunes(syllabus, SyllabusMaxChars)),
	}
}

// BuildSyllabusFormatRequest builds the syllabus_format request of
// SPEC-v1.1 A8.3: the verbatim system prompt and "RAW SYLLABUS:\n{raw_text}".
// The raw text is passed whole (A8.3 sets no limit and the formatted
// syllabus must keep every factual detail); the glossary_seed that reads
// the result still truncates to SyllabusMaxChars.
func BuildSyllabusFormatRequest(raw string) (system, user string) {
	return SyllabusFormatSystem, fill(SyllabusFormatUserTemplate, "raw_text", raw)
}

// MaterialChunks joins the pages of one material with "\n\n" and splits the
// result into chunks of at most MaterialChunkChars characters, only at page
// boundaries (SPEC §6.0.1). Blank pages are skipped. A single page longer
// than the limit forms its own chunk and is not split.
func MaterialChunks(pages []string) []string {
	var chunks []string
	var cur []string
	curLen := 0
	for _, p := range pages {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n := runeLen(p)
		add := n
		if len(cur) > 0 {
			add += 2 // "\n\n"
		}
		if len(cur) > 0 && curLen+add > MaterialChunkChars {
			chunks = append(chunks, strings.Join(cur, "\n\n"))
			cur, curLen, add = nil, 0, n
		}
		cur = append(cur, p)
		curLen += add
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "\n\n"))
	}
	return chunks
}

// MaterialTermsRequests builds one material_terms request per chunk
// (SPEC §6.0.1, §13.2). No request when every page is blank.
func MaterialTermsRequests(pages []string) []Request {
	chunks := MaterialChunks(pages)
	out := make([]Request, len(chunks))
	for i, c := range chunks {
		out[i] = Request{System: MaterialTermsSystem, User: fill(MaterialTermsUserTemplate, "chunk_text", c)}
	}
	return out
}

// SummarySystem is the summary prefix (SPEC §13.3): glossary + slides.
func SummarySystem(lec LectureSnapshot) string {
	return fill(SummarySystemTemplate,
		"glossary_lines", GlossaryLines(lec.Terms),
		"slides_text", LectureSlides(lec))
}

// SummaryRequest builds the summary request (SPEC §6.3, §13.3): the user
// message is every asr_text joined by "\n".
func SummaryRequest(lec LectureSnapshot) Request {
	lines := make([]string, len(lec.Sentences))
	for i, s := range lec.Sentences {
		lines[i] = oneLine(s.ASRText)
	}
	return Request{
		System: SummarySystem(lec),
		User:   fill(SummaryUserTemplate, "asr_sentences_joined_by_newline", strings.Join(lines, "\n")),
	}
}

// truncateRunes keeps the first n characters of s.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// oneLine replaces line breaks inside a sentence by spaces so that every
// sentence occupies exactly one line in the line-oriented user messages.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Join(strings.Fields(s), " ")
}
