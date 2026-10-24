// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cost model of the splitter. A segmentation that meets every limit of P4.3
// costs 0. When none exists (a single word longer than 84 characters, very
// fast speech), the one with the smallest total violation is used, measuring
// an excess character like 70 ms of excess time (about 15 characters per
// second of reading). A cue of zero duration is effectively forbidden.
const (
	charWeight   = 70
	zeroPenalty  = int64(1) << 40
	inf          = int64(1) << 62
	maxCueTokens = 128 // a cue never spans more words (≥ 255 characters); bounds the search
)

type span struct{ i, j int } // tokens [i, j)

type splitter struct {
	toks   []token
	end    int64 // when the last cue ends
	cum    []int // cum[k]: characters before token k, counting one space after each token
	minDur bool  // the 1 s minimum applies (the tokens last at least 1 s)
}

// splitSentence splits timed tokens into cues (P4.3). A cue [i, j) starts at
// token i's start and ends at token j's start (at end for the last cue), so
// its duration does not depend on the other cues. The tokens are one piece
// of a sentence between pauses (see SegmentCues): end is the last token's
// end, or a little later when a pause follows.
//
// A span that meets the limits is kept whole. Otherwise it is cut in two at
// the break with the highest priority among those that still allow the best
// segmentation of both halves (with every limit met, when possible); ties go
// to the break nearest the even division of the span into the fewest cues
// its length allows. Both halves are then split the same way.
func splitSentence(toks []token, end int64) []span {
	n := len(toks)
	sp := &splitter{toks: toks, end: end, cum: make([]int, n+1)}
	for k, t := range toks {
		sp.cum[k+1] = sp.cum[k] + t.runes + 1
	}
	total := sp.at(n) - sp.at(0)
	if total <= 0 {
		return []span{{0, n}}
	}
	sp.minDur = total >= MinCueMS
	var out []span
	sp.split(0, n, &out)
	return out
}

// at is the start of token k, or the end of the last cue for k == n.
func (sp *splitter) at(k int) int64 {
	if k < len(sp.toks) {
		return sp.toks[k].s
	}
	return sp.end
}

func (sp *splitter) chars(i, j int) int { return sp.cum[j] - sp.cum[i] - 1 }

// cost is the violation of tokens [i, j) shown as one cue.
func (sp *splitter) cost(i, j int) int64 {
	if j-i > maxCueTokens {
		return inf
	}
	var v int64
	if c := sp.chars(i, j); c > MaxCueChars {
		v += int64(c-MaxCueChars) * charWeight
	}
	d := sp.at(j) - sp.at(i)
	switch {
	case d <= 0:
		v += zeroPenalty
	case d > MaxCueMS:
		v += d - MaxCueMS
	case sp.minDur && d < MinCueMS:
		v += MinCueMS - d
	}
	return v
}

func addCost(a, b int64) int64 {
	if a >= inf || b >= inf {
		return inf
	}
	return a + b
}

// forward returns f with f[k-i] = the least cost of segmenting [i, k).
func (sp *splitter) forward(i, j int) []int64 {
	f := make([]int64, j-i+1)
	for k := i + 1; k <= j; k++ {
		best := inf
		for l := max(i, k-maxCueTokens); l < k; l++ {
			best = min(best, addCost(f[l-i], sp.cost(l, k)))
		}
		f[k-i] = best
	}
	return f
}

// backward returns b with b[k-i] = the least cost of segmenting [k, j).
func (sp *splitter) backward(i, j int) []int64 {
	b := make([]int64, j-i+1)
	for k := j - 1; k >= i; k-- {
		best := inf
		for m := k + 1; m <= min(j, k+maxCueTokens); m++ {
			best = min(best, addCost(sp.cost(k, m), b[m-i]))
		}
		b[k-i] = best
	}
	return b
}

func (sp *splitter) split(i, j int, out *[]span) {
	self := sp.cost(i, j)
	if self == 0 || j-i == 1 {
		*out = append(*out, span{i, j})
		return
	}
	f, b := sp.forward(i, j), sp.backward(i, j)
	best := f[j-i]
	if self <= best {
		*out = append(*out, span{i, j})
		return
	}
	parts, byTime := sp.pieces(i, j)
	cut, cutPri, cutDist := -1, -1, int64(0)
	for k := i + 1; k < j; k++ {
		if addCost(f[k-i], b[k-i]) != best {
			continue
		}
		p := breakPriority(sp.toks[k-1].text, sp.toks[k].text)
		d := sp.distance(i, j, k, parts, byTime)
		if p > cutPri || (p == cutPri && d < cutDist) {
			cut, cutPri, cutDist = k, p, d
		}
	}
	sp.split(i, cut, out)
	sp.split(cut, j, out)
}

// pieces is the fewest cues the length or the duration of [i, j) needs (at
// least 2), and whether the duration is the binding limit; the even division
// is measured in that dimension.
func (sp *splitter) pieces(i, j int) (int, bool) {
	c, d := int64(sp.chars(i, j)), sp.at(j)-sp.at(i)
	n := max(2, ceilDiv(c, MaxCueChars), ceilDiv(d, MaxCueMS))
	return int(n), d*MaxCueChars > c*MaxCueMS
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// distance measures how far cutting before token k is from the ideal cut of
// [i, j) into parts even pieces: after floor(parts/2) of them.
func (sp *splitter) distance(i, j, k, parts int, byTime bool) int64 {
	var pos, total int64
	if byTime {
		pos, total = sp.at(k)-sp.at(i), sp.at(j)-sp.at(i)
	} else {
		pos, total = int64(sp.chars(i, k)), int64(sp.chars(i, j))
	}
	d := pos*int64(parts) - int64(parts/2)*total
	if d < 0 {
		d = -d
	}
	return d
}

// Break priorities between two words (P4.3).
const (
	priWord        = 0 // any word boundary
	priConjunction = 1 // before and, but, so, because, which, where, when
	priComma       = 2 // after a comma or a dash
	priColon       = 3 // after a semicolon or a colon
	priStop        = 4 // after a full stop, question mark or exclamation mark
)

var conjunctions = map[string]bool{
	"and": true, "but": true, "so": true, "because": true, "which": true, "where": true, "when": true,
}

// abbreviations of SPEC §6.2: a full stop after them does not end a clause,
// so it is not a stop-priority break.
var abbreviations = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "dr": true, "mr": true, "mrs": true,
	"ms": true, "prof": true, "fig": true, "eq": true, "no": true, "approx": true,
}

const closers = `"'”’)]}»`

func breakPriority(prev, next string) int {
	body := strings.TrimRight(prev, closers)
	last, _ := utf8.DecodeLastRuneInString(body)
	switch last {
	case '.', '?', '!', '…':
		if last != '.' || !isAbbreviation(body) {
			return priStop
		}
	case ';', ':':
		return priColon
	case ',', '—', '–':
		return priComma
	case '-':
		if strings.HasSuffix(body, "--") || strings.Trim(body, "-") == "" {
			return priComma
		}
	}
	if conjunctions[strings.ToLower(strings.TrimFunc(next, func(r rune) bool { return !unicode.IsLetter(r) }))] {
		return priConjunction
	}
	return priWord
}

// isAbbreviation reports whether w (ending with ".") is an abbreviation of
// §6.2 or a single capital letter with a dot (an initial).
func isAbbreviation(w string) bool {
	w = strings.TrimLeft(strings.TrimRight(w, "."), `"'“‘([{`)
	if abbreviations[strings.ToLower(w)] {
		return true
	}
	r, size := utf8.DecodeRuneInString(w)
	return size == len(w) && unicode.IsUpper(r)
}
