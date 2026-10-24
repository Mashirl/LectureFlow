// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"lectureflow/internal/model"
)

// JanitorSystem is the glossary janitor prompt of SPEC-v2 G3.4, verbatim.
const JanitorSystem = `You are the glossary curator for a university course. The glossary is used in two ways:
1. Terms in the "enforce" tier MUST be translated exactly as given, and translations are automatically checked for them.
2. Terms in the "reference" tier are shown to the translator as the preferred translation when the word is used in its technical sense; they are not enforced.
A wrong "enforce" entry actively damages translations: for example, enforcing "public => 公钥" forces "public knowledge" to be mistranslated. Your job is to prevent that, and to keep the glossary consistent.

For each entry, decide:
- "enforce": proper nouns, named algorithms, standards and attacks, acronyms, multi-word technical terms, and single words that in this course are used almost exclusively in their technical sense.
- "reference": words that have a technical meaning in this course but are also common in everyday or general academic English (the lecturer will use them both ways).
- "disable": generic words with no specific technical meaning in this course (e.g. "world", "analysis", "practical", "data" when used generically), and entries that are not terms at all.
- "merge": duplicates and plural or inflected forms of another entry; give the id to merge into.
- "fix_zh": the Chinese translation is wrong, non-standard, or inconsistent with a related entry (for example an acronym and its expansion must use the same translation). Use the standard term from Chinese university textbooks. Use "" for terms that Chinese speakers normally keep in English.
Leave an entry out of your answer if it is already correct.

Use the contexts to see how each word is actually used in this course. Never act on entries marked "locked"; use them only as reference for consistency.
Give a short reason in English (under 15 words) for every action.

Respond with JSON only: {"actions": [{"id": "...", "action": "...", "into": "...", "zh": "...", "reason": "..."}]}`

// Limits of the janitor input (G3.2).
const (
	JanitorSyllabusMaxChars = 8000 // SYLLABUS.md in the fixed prefix
	JanitorBatchThreshold   = 400  // more unlocked entries than this: batches
	JanitorBatchSize        = 200  // entries per batch, in en_key order
	JanitorMaxContexts      = 2    // context snippets per entry
	JanitorContextChars     = 60   // characters kept on each side of a match
)

// JanitorInput is everything the janitor request depends on. Callers load
// it from the database; ctxbuild never does.
type JanitorInput struct {
	CourseName string
	CourseCode string // "" = none
	// Syllabus is SYLLABUS.md (syllabus.text); "" when the course has none.
	Syllabus string
	// Terms of the course, any order.
	Terms []model.Term
	// Materials of the course, any order (bound or not): the contexts are
	// cut from their pages_md and from Syllabus only (G6 rule 7).
	Materials []model.Material
}

// janitorEntry is one user line of an unlocked term (G3.2).
type janitorEntry struct {
	ID         string   `json:"id"`
	En         string   `json:"en"`
	Zh         string   `json:"zh"`
	Tier       string   `json:"tier"`
	CommonWord bool     `json:"common_word"`
	Freq       int      `json:"freq"`
	Source     string   `json:"source"`
	Contexts   []string `json:"contexts"`
}

// janitorLocked is one user line of a locked term, listed after the others
// for reference only (no contexts).
type janitorLocked struct {
	ID         string `json:"id"`
	En         string `json:"en"`
	Zh         string `json:"zh"`
	Tier       string `json:"tier"`
	CommonWord bool   `json:"common_word"`
	Freq       int    `json:"freq"`
	Source     string `json:"source"`
	Locked     bool   `json:"locked"`
}

// JanitorSystemPrompt is the fixed prefix of every janitor request (G3.2):
// the G3.4 prompt, the course name and code, and SYLLABUS.md truncated to
// JanitorSyllabusMaxChars characters ("(none)" without a syllabus).
func JanitorSystemPrompt(in JanitorInput) string {
	course := oneLine(strings.TrimSpace(in.CourseName))
	if code := oneLine(strings.TrimSpace(in.CourseCode)); code != "" {
		course += " (" + code + ")"
	}
	if course == "" {
		course = None
	}
	syl := strings.TrimSpace(in.Syllabus)
	if syl == "" {
		syl = None
	} else {
		syl = truncateRunes(syl, JanitorSyllabusMaxChars)
	}
	return JanitorSystem + "\n\nCOURSE: " + course + "\n\nSYLLABUS.md:\n" + syl
}

// JanitorRequests builds the glossary janitor requests (SPEC-v2 G3.2).
//
// Entries are the enabled, unlocked terms in en_key order, one JSON line
// each (id, en, zh, tier, common_word, freq, source, contexts); the enabled
// locked terms follow after an empty line, each with "locked": true. With
// more than JanitorBatchThreshold entries they are split, in en_key order,
// into batches of JanitorBatchSize; every batch carries all locked lines.
// No request when there is no entry. Disabled terms are not sent: the
// janitor has no action that enables a term (NOTES.md, pipeline-A).
//
// contexts are up to JanitorMaxContexts snippets of about
// JanitorContextChars characters on each side of a whole-word,
// case-insensitive match, searched in the material pages (upload order,
// page order) and then in SYLLABUS.md — never in lecture transcripts,
// repairs, summaries or translations (G6 rule 7).
func JanitorRequests(in JanitorInput) []Request {
	b := NewJanitorBuilder(in)
	var out []Request
	for _, batch := range b.Batches() {
		out = append(out, b.Request(batch))
	}
	return out
}

// JanitorBuilder builds janitor requests for any run of entries, so that a
// batch whose answer was cut off can be sent again in halves (lead ruling
// after the real acceptance, NOTES.md pipeline-A). The entry lines are
// prepared once; the builder is read-only and safe for concurrent use.
type JanitorBuilder struct {
	system string
	open   []model.Term      // the entries (enabled, unlocked) in en_key order
	lines  map[string][]byte // entry id → its JSON line (ending in a newline)
	locked []byte            // the locked lines (each ending in a newline)
}

// NewJanitorBuilder prepares the requests of JanitorRequests.
func NewJanitorBuilder(in JanitorInput) *JanitorBuilder {
	b := &JanitorBuilder{lines: map[string][]byte{}}
	var locked []model.Term
	seen := map[string]bool{}
	ts := append([]model.Term(nil), in.Terms...)
	sort.SliceStable(ts, func(i, j int) bool {
		if a, b := termKey(ts[i]), termKey(ts[j]); a != b {
			return a < b
		}
		return ts[i].ID < ts[j].ID
	})
	for _, t := range ts {
		if !t.Enabled || strings.TrimSpace(t.En) == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		if t.Locked {
			locked = append(locked, t)
		} else {
			b.open = append(b.open, t)
		}
	}
	if len(b.open) == 0 {
		return b
	}
	b.system = JanitorSystemPrompt(in)
	corpus := janitorCorpus(in)
	var lockedLines bytes.Buffer
	for _, t := range locked {
		writeJSONLine(&lockedLines, janitorLocked{
			ID: t.ID, En: oneLine(strings.TrimSpace(t.En)), Zh: oneLine(strings.TrimSpace(t.Zh)), Tier: janitorTier(t),
			CommonWord: t.CommonWord, Freq: t.Freq, Source: t.Source, Locked: true,
		})
	}
	b.locked = lockedLines.Bytes()
	for _, t := range b.open {
		var line bytes.Buffer
		writeJSONLine(&line, janitorEntry{
			ID: t.ID, En: oneLine(strings.TrimSpace(t.En)), Zh: oneLine(strings.TrimSpace(t.Zh)), Tier: janitorTier(t),
			CommonWord: t.CommonWord, Freq: t.Freq, Source: t.Source,
			Contexts: corpus.contexts(t.En),
		})
		b.lines[t.ID] = line.Bytes()
	}
	return b
}

// Batches returns the entries of the G3.2 requests: all of them in one
// batch, or batches of JanitorBatchSize when there are more than
// JanitorBatchThreshold. Nil when there is no entry.
func (b *JanitorBuilder) Batches() [][]model.Term {
	size := len(b.open)
	if size > JanitorBatchThreshold {
		size = JanitorBatchSize
	}
	var out [][]model.Term
	for start := 0; start < len(b.open); start += size {
		out = append(out, b.open[start:min(start+size, len(b.open))])
	}
	return out
}

// Request is the janitor request of entries (a run of Batches' terms, in
// en_key order): one line each, then the locked lines after an empty line.
func (b *JanitorBuilder) Request(entries []model.Term) Request {
	var u bytes.Buffer
	for _, t := range entries {
		u.Write(b.lines[t.ID])
	}
	if len(b.locked) > 0 {
		u.WriteByte('\n')
		u.Write(b.locked)
	}
	return Request{System: b.system, User: strings.TrimSuffix(u.String(), "\n")}
}

func janitorTier(t model.Term) string {
	if t.Tier == "" {
		return model.TierEnforce
	}
	return t.Tier
}

// writeJSONLine appends v as one line of JSON (HTML characters unescaped).
func writeJSONLine(b *bytes.Buffer, v any) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // strings, ints and bools only: cannot fail
}

// janitorSegment is one material page or the syllabus with whitespace runs
// collapsed: orig keeps the case for the snippet, low is matched against.
type janitorSegment struct {
	orig []rune
	low  []rune
}

type janitorText []janitorSegment

// janitorCorpus collects the human-written texts of G6 rule 7: the pages of
// every material of the course (upload order, page order), then SYLLABUS.md.
func janitorCorpus(in JanitorInput) janitorText {
	ms := append([]model.Material(nil), in.Materials...)
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].CreatedAt != ms[j].CreatedAt {
			return ms[i].CreatedAt < ms[j].CreatedAt
		}
		return ms[i].ID < ms[j].ID
	})
	var c janitorText
	add := func(s string) {
		flat := strings.Join(strings.Fields(s), " ")
		if flat == "" {
			return
		}
		orig := []rune(flat)
		low := make([]rune, len(orig))
		for i, r := range orig {
			low[i] = unicode.ToLower(r)
		}
		c = append(c, janitorSegment{orig: orig, low: low})
	}
	for _, m := range ms {
		for _, p := range m.PagesMD {
			add(p)
		}
	}
	add(in.Syllabus)
	return c
}

// contexts returns up to JanitorMaxContexts distinct snippets around
// whole-word matches of en. A snippet starts and ends at a word boundary
// when one lies within its outer third; "…" marks a cut.
func (c janitorText) contexts(en string) []string {
	p := []rune(strings.Join(strings.Fields(en), " "))
	for i, r := range p {
		p[i] = unicode.ToLower(r)
	}
	out := []string{}
	if len(p) == 0 {
		return out
	}
	for _, seg := range c {
		for from := 0; from+len(p) <= len(seg.low); {
			i := janitorIndex(seg.low[from:], p)
			if i < 0 {
				break
			}
			s, e := from+i, from+i+len(p)
			if !janitorBoundary(seg.low, s-1) || !janitorBoundary(seg.low, e) {
				from = s + 1
				continue
			}
			snip := janitorSnippet(seg.orig, s, e)
			if !janitorContains(out, snip) {
				out = append(out, snip)
				if len(out) == JanitorMaxContexts {
					return out
				}
			}
			from = min(e+JanitorContextChars, len(seg.low)) // the next snippet does not overlap this one
			if from <= s {
				from = s + 1
			}
		}
	}
	return out
}

// janitorSnippet cuts about JanitorContextChars characters on each side of [s, e).
func janitorSnippet(r []rune, s, e int) string {
	a := max(0, s-JanitorContextChars)
	b := min(len(r), e+JanitorContextChars)
	if a > 0 { // start after a space within the first third
		for k := a; k < a+JanitorContextChars/3 && k < s; k++ {
			if r[k] == ' ' {
				a = k + 1
				break
			}
		}
	}
	if b < len(r) { // end before a space within the last third
		for k := b - 1; k >= b-JanitorContextChars/3 && k >= e; k-- {
			if r[k] == ' ' {
				b = k
				break
			}
		}
	}
	out := strings.TrimSpace(string(r[a:b]))
	if a > 0 {
		out = "…" + out
	}
	if b < len(r) {
		out += "…"
	}
	return out
}

// janitorBoundary: position i is outside the text or not a word character
// (isWordRune, the same class as freq counting).
func janitorBoundary(r []rune, i int) bool {
	return i < 0 || i >= len(r) || !isWordRune(r[i])
}

// janitorIndex returns the first index of p in s, or -1.
func janitorIndex(s, p []rune) int {
	n := len(p)
outer:
	for i := 0; i+n <= len(s); i++ {
		if s[i] != p[0] {
			continue
		}
		for k := 1; k < n; k++ {
			if s[i+k] != p[k] {
				continue outer
			}
		}
		return i
	}
	return -1
}

func janitorContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
