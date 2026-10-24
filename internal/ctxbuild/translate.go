// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"strconv"
	"strings"
)

// ASRNote is the first line of the [ASR] section: SPEC §7.1 labels the raw
// recognition as "可能有误，仅供参考"; in English like the G5.3 labels (lead
// ruling, Gate round 1).
const ASRNote = "(Raw speech recognition output; may contain errors. For reference only.)"

// TranslateRequest is one translate request (SPEC §7.1, §7.2).
type TranslateRequest struct {
	System string // fixed prefix, byte-identical for every sentence of a lecture
	User   string
	Idx    int
	Hash   string // TranslateHash(lec, idx, x), stored as sentence.zh_hash
}

// TranslateSystem is the translate prefix (SPEC §7.1): system prompt +
// enabled glossary in the two sections of SPEC-v2 G5.1 + summary. Always
// the current (v2) prompt, whatever prompt version the job's snapshot
// records (lead ruling 10).
func TranslateSystem(lec LectureSnapshot) string {
	return fill(TranslateSystemTemplate,
		"glossary_lines", GlossaryLines(lec.Terms),
		"summary", summaryOrNone(lec.Summary))
}

// translateHashPrefix is the prefix part of the translate hashes (lead
// ruling 9): the FROZEN v1 template filled with GlossaryHashText and the
// summary. It does not depend on the prompt actually sent, so prompt
// changes never make translate stale, and for an all-enforce glossary it is
// byte-identical to the v1 prefix (the v1 hashes stay valid).
func translateHashPrefix(lec LectureSnapshot) string {
	return fill(translateSystemTemplateV1,
		"glossary_lines", GlossaryHashText(lec.Terms),
		"summary", summaryOrNone(lec.Summary))
}

// BuildTranslateRequest builds the request for sentence idx with context
// window x (SPEC §7.1, §7.2 with the labels of SPEC-v2 G5.3). The user
// message is
//
//	[BEFORE]
//	text of idx-x .. idx-1, one per line, or (none)
//	[TARGET #idx]
//	text of idx
//	[ASR]
//	(Raw speech recognition output; may contain errors. For reference only.)
//	asr_text of idx
//	[AFTER]
//	text of idx+1 .. idx+x, one per line, or (none)
//
// An empty [BEFORE] / [AFTER] section (idx = 0, idx = n-1, x = 0) holds
// "(none)"; [ASR] (ASRNote, then asr_text) is omitted when asr_text equals
// text. No Chinese text of any sentence is included. idx must be a valid
// sentence index (panics otherwise).
func BuildTranslateRequest(lec LectureSnapshot, idx int, x int) TranslateRequest {
	ss := lec.Sentences
	if idx < 0 || idx >= len(ss) {
		panic(fmt.Sprintf("ctxbuild: sentence idx %d out of range [0,%d)", idx, len(ss)))
	}
	x = max(x, 0)
	lo, hi := max(0, idx-x), min(len(ss)-1, idx+x)
	t := ss[idx]

	var b strings.Builder
	contextSection(&b, LabelBefore, ss[lo:idx])
	fmt.Fprintf(&b, LabelTarget+"\n%s\n", t.Idx, oneLine(t.Text))
	if !sameText(t.ASRText, t.Text) {
		b.WriteString(LabelASR + "\n" + ASRNote + "\n")
		b.WriteString(oneLine(t.ASRText))
		b.WriteByte('\n')
	}
	contextSection(&b, LabelAfter, ss[idx+1:hi+1])
	return TranslateRequest{
		System: TranslateSystem(lec),
		User:   strings.TrimSuffix(b.String(), "\n"),
		Idx:    t.Idx,
		Hash:   translateHash(translateHashPrefix(lec), ss, idx, x),
	}
}

// contextSection writes a [BEFORE] / [AFTER] section: the label, then the
// text of each sentence on its own line, or "(none)" (SPEC-v2 G5.3).
func contextSection(b *strings.Builder, label string, ss []Sentence) {
	b.WriteString(label)
	b.WriteByte('\n')
	if len(ss) == 0 {
		b.WriteString(None)
		b.WriteByte('\n')
		return
	}
	for _, s := range ss {
		b.WriteString(oneLine(s.Text))
		b.WriteByte('\n')
	}
}

// TranslateRetryUser appends the QA rejection to a translate user message
// (SPEC §7.4, §13.5).
func TranslateRetryUser(user, qaError string) string {
	return user + fill(TranslateRetrySuffix, "qa_error", qaError)
}

// TranslateHash is the per-sentence input hash of SPEC §7.3:
// SHA-256(prefix + text of every sentence in [idx-x, idx+x] + asr_text of idx + x),
// where the prefix is translateHashPrefix (the frozen v1 template with
// GlossaryHashText, lead ruling 9), not the prompt sent.
// Changing the text of sentence i changes exactly the hashes of [i-x, i+x];
// changing the glossary (including a term's tier) or summary changes every hash.
func TranslateHash(lec LectureSnapshot, idx int, x int) string {
	ss := lec.Sentences
	if idx < 0 || idx >= len(ss) {
		panic(fmt.Sprintf("ctxbuild: sentence idx %d out of range [0,%d)", idx, len(ss)))
	}
	return translateHash(translateHashPrefix(lec), ss, idx, max(x, 0))
}

// TranslateStageHash is the stage-level input hash of translate: SHA-256 of
// the translate hash prefix (translateHashPrefix) and x. It changes exactly
// when every per-sentence hash changes because of the glossary or summary
// (SPEC §7.3 third rule → the whole stage becomes stale, §4.4);
// sentence-level edits do not touch it. It is also the base hash of an
// incremental run (state.IncrementalBaseHash), so a prompt change never
// stops a queued incremental run either.
func TranslateStageHash(lec LectureSnapshot, x int) string {
	return hashParts("translate-stage", translateHashPrefix(lec), strconv.Itoa(max(x, 0)))
}

// TranslateHashes returns TranslateHash for every sentence (prefix computed once).
func TranslateHashes(lec LectureSnapshot, x int) []string {
	prefix := translateHashPrefix(lec)
	out := make([]string, len(lec.Sentences))
	for i := range lec.Sentences {
		out[i] = translateHash(prefix, lec.Sentences, i, max(x, 0))
	}
	return out
}

func translateHash(prefix string, ss []Sentence, idx, x int) string {
	lo, hi := max(0, idx-x), min(len(ss)-1, idx+x)
	parts := make([]string, 0, hi-lo+5)
	parts = append(parts, "translate", prefix)
	for _, s := range ss[lo : hi+1] {
		parts = append(parts, s.Text)
	}
	parts = append(parts, ss[idx].ASRText, strconv.Itoa(x))
	return hashParts(parts...)
}

func sameText(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }
