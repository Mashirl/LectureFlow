// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// token is one whitespace-separated word of a text, with its time.
type token struct {
	text   string
	b0, b1 int // byte range in the text
	runes  int
	key    string // alignment key: lower case, letters and digits only
	s, e   int64
}

func tokenize(text string) []token {
	var toks []token
	start := -1
	add := func(end int) {
		t := text[start:end]
		toks = append(toks, token{text: t, b0: start, b1: end, runes: utf8.RuneCountInString(t), key: alignKey(t)})
	}
	for i, r := range text {
		if unicode.IsSpace(r) {
			if start >= 0 {
				add(i)
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		add(len(text))
	}
	return toks
}

func joinTokens(toks []token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}

// alignKey is the case- and punctuation-insensitive form used by the LCS
// alignment (P4.3). A word made only of punctuation has an empty key and is
// never aligned.
func alignKey(w string) string {
	var b strings.Builder
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// WordTimes returns the whitespace-separated words of s.Text with their
// estimated times (P4.3), in order. Starts are monotone and every end is at
// or after its start.
//
//   - text == asr_text: the times of Words. faster-whisper may split one
//     written word into several (`IND` + `-CCA`), so words are mapped onto the
//     text by their characters: a word spanning several timestamps starts
//     with the first and ends with the last.
//   - text != asr_text: the words of text are aligned with the words of
//     asr_text (longest common subsequence, ignoring case and punctuation);
//     aligned words keep their times, the others are interpolated by
//     character count between the neighbouring known times.
//   - no Words: the sentence duration is spread linearly over the character
//     positions of text.
func WordTimes(s Sentence) []Word {
	toks := tokenize(s.Text)
	timeTokens(toks, s)
	out := make([]Word, len(toks))
	for i, t := range toks {
		out[i] = Word{W: t.text, S: t.s, E: t.e}
	}
	return out
}

func timeTokens(toks []token, s Sentence) {
	if len(toks) == 0 {
		return
	}
	asr := asrTokens(s)
	if asr != nil && s.Text == s.ASRText {
		// Identical text: asr holds the times of toks already.
		for i := range toks {
			toks[i].s, toks[i].e = asr[i].s, asr[i].e
		}
	} else {
		if asr == nil {
			asr = rawWordTokens(s.Words)
		}
		known := align(toks, asr)
		interpolate(toks, known, s.StartMS, s.EndMS)
	}
	for i := range toks {
		if i > 0 && toks[i].s < toks[i-1].s {
			toks[i].s = toks[i-1].s
		}
		if toks[i].e < toks[i].s {
			toks[i].e = toks[i].s
		}
	}
}

// asrTokens returns the words of asr_text timed by Words, mapping Words onto
// asr_text character by character (whitespace ignored on both sides). It
// returns nil when there are no Words or they do not spell asr_text.
func asrTokens(s Sentence) []token {
	if len(s.Words) == 0 {
		return nil
	}
	toks := tokenize(s.ASRText)
	if len(toks) == 0 || !mapWords(s.ASRText, toks, s.Words) {
		return nil
	}
	return toks
}

// mapWords sets the times of toks (tokens of text) from words whose
// characters, whitespace ignored, spell text. It reports false when they do
// not.
func mapWords(text string, toks []token, words []Word) bool {
	set := make([]bool, len(toks))
	pos, ti := 0, 0
	for _, w := range words {
		first := -1 // byte offset in text of the word's first character
		for _, r := range w.W {
			if unicode.IsSpace(r) {
				continue
			}
			pos = skipSpace(text, pos)
			if pos >= len(text) {
				return false
			}
			tr, size := utf8.DecodeRuneInString(text[pos:])
			if tr != r {
				return false
			}
			if first < 0 {
				first = pos
			}
			pos += size
		}
		if first < 0 {
			continue // a word of whitespace only
		}
		for ti < len(toks) && toks[ti].b1 <= first {
			ti++
		}
		for k := ti; k < len(toks) && toks[k].b0 < pos; k++ {
			if !set[k] {
				toks[k].s, toks[k].e, set[k] = w.S, w.E, true
				continue
			}
			if w.E > toks[k].e {
				toks[k].e = w.E
			}
		}
	}
	if skipSpace(text, pos) != len(text) {
		return false
	}
	for _, ok := range set {
		if !ok {
			return false
		}
	}
	return true
}

func skipSpace(text string, pos int) int {
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !unicode.IsSpace(r) {
			break
		}
		pos += size
	}
	return pos
}

// rawWordTokens turns Words into tokens one to one; used only when Words do
// not spell asr_text, so they cannot be grouped into its words.
func rawWordTokens(words []Word) []token {
	toks := make([]token, len(words))
	for i, w := range words {
		toks[i] = token{text: w.W, key: alignKey(w.W), s: w.S, e: w.E}
	}
	return toks
}

// align copies the times of the longest common subsequence of asr (by
// alignment key) onto toks and reports which tokens got one. Among equally
// long alignments the latest matches win, so in a stutter ("the the cipher"
// repaired to "the cipher") the word is aligned with its last occurrence.
func align(toks, asr []token) []bool {
	known := make([]bool, len(toks))
	n, m := len(toks), len(asr)
	if m == 0 {
		return known
	}
	// l[i*(m+1)+j] = LCS length of toks[:i] and asr[:j].
	l := make([]int32, (n+1)*(m+1))
	at := func(i, j int) int { return i*(m+1) + j }
	match := func(i, j int) bool { return toks[i].key != "" && toks[i].key == asr[j].key }
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if match(i-1, j-1) {
				l[at(i, j)] = l[at(i-1, j-1)] + 1
			} else {
				l[at(i, j)] = max(l[at(i-1, j)], l[at(i, j-1)])
			}
		}
	}
	for i, j := n, m; i > 0 && j > 0; {
		switch {
		case match(i-1, j-1):
			toks[i-1].s, toks[i-1].e, known[i-1] = asr[j-1].s, asr[j-1].e, true
			i--
			j--
		case l[at(i-1, j)] >= l[at(i, j-1)]:
			i--
		default:
			j--
		}
	}
	return known
}

// interpolate times every run of tokens without a known time: the run's
// characters (counting one space between its tokens) are spread linearly
// over the time between the end of the known token before it (or startMS)
// and the start of the known token after it (or endMS). Without any known
// token this is the no-Words rule: the whole sentence by character position.
func interpolate(toks []token, known []bool, startMS, endMS int64) {
	n := len(toks)
	off := make([]int, n) // character offset of each token in the space-joined text
	for k := 1; k < n; k++ {
		off[k] = off[k-1] + toks[k-1].runes + 1
	}
	endOff := func(k int) int { return off[k] + toks[k].runes }
	for a := 0; a < n; {
		if known[a] {
			a++
			continue
		}
		b := a
		for b < n && !known[b] {
			b++
		}
		lo, hi := startMS, endMS
		c0, c1 := off[a], endOff(b-1)
		if a > 0 {
			lo = toks[a-1].e
		}
		if b < n {
			hi = toks[b].s
			if a == 0 && lo > hi {
				lo = hi // the sentence start lies after its first known word
			}
		}
		if hi < lo {
			hi = lo
		}
		span := int64(c1 - c0)
		for k := a; k < b; k++ {
			toks[k].s = lo + (hi-lo)*int64(off[k]-c0)/span
			toks[k].e = lo + (hi-lo)*int64(endOff(k)-c0)/span
		}
		a = b
	}
}
