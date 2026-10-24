// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// zhStops are the Chinese punctuation marks a translation is cut after (P4.4).
const zhStops = "，。；：？！、"

// noLineStart may not begin a Chinese chunk: a cut is moved past them.
const noLineStart = "，。；：？！、）》」』】〉”’…%,.;:?!)]}"

// zhOpeners may not end a Chinese chunk: a cut is moved before them.
const zhOpeners = "（《「『【〈“‘"

// assignZh distributes the translation over the cues (P4.4). Each boundary
// between two cues is placed in zh at the same proportion as in the English
// (by character count), at the nearest punctuation mark of zhStops within
// ±30% of the Chinese length; else right after a function character such as
// 的 or 和 near that position (lead ruling, see zhCut); else at the
// proportional position itself, moved forward so that it never cuts a run of
// Latin letters, digits or a formula (`IND-CCA`, `AES-256`, `O(n log n)`). A cue whose chunk is empty
// shows the previous chunk again, so there may be fewer Chinese chunks than
// cues. An empty zh (not translated) leaves every Zh empty.
//
// A boundary at a pause (pauses[k]: cue k starts after one) is not cut at the
// proportional position: the cue on one side is often a single word ("Is" of
// "Is … it clear?"), and cutting the Chinese there splits a word (大|家).
// Without punctuation or a function character nearby that leaves each side
// some Chinese, the two cues share one chunk instead: both show the Chinese
// of both (lead ruling on P4.3).
func assignZh(cues []Cue, pauses []bool, zh string) {
	if zh == "" || len(cues) == 0 {
		return
	}
	lens := make([]int, len(cues))
	for i, c := range cues {
		lens[i] = utf8.RuneCountInString(c.En)
	}
	r := []rune(zh)
	cuts, joined := zhCuts(r, lens, pauses)
	prev := ""
	for i := 0; i < len(cues); {
		j := i + 1
		for j < len(cues) && joined[j] {
			j++
		}
		chunk := strings.TrimSpace(string(r[cuts[i]:cuts[j]]))
		if chunk == "" {
			chunk = prev
		}
		for k := i; k < j; k++ {
			cues[k].Zh = chunk
		}
		prev = chunk
		i = j
	}
}

// zhCuts returns len(lens)+1 non-decreasing rune positions in r, from 0 to
// len(r): chunk i is r[cuts[i]:cuts[i+1]] and goes with the English cue of
// lens[i] characters. joined[b] reports that boundary b (between cues b-1
// and b) is a pause without a place to cut the Chinese: cuts[b] equals
// cuts[b-1], and cues b-1 and b share the chunk r[cuts[b-1]:cuts[b+1]]
// (longer runs of joined boundaries likewise). pauses may be nil.
func zhCuts(r []rune, lens []int, pauses []bool) (cuts []int, joined []bool) {
	n := len(r)
	cuts = make([]int, len(lens)+1)
	joined = make([]bool, len(lens))
	cuts[len(lens)] = n
	total := 0
	for _, l := range lens {
		total += l
	}
	if total == 0 {
		total = 1
	}
	stops, softs := zhCutPoints(r), zhSoftPoints(r)
	cum := 0
	for b := 1; b < len(lens); b++ {
		cum += lens[b-1]
		prev := cuts[b-1]
		c := zhCut(r, stops, softs, cum, total, prev, pauses != nil && pauses[b])
		if c < 0 {
			c, joined[b] = prev, true
		}
		cuts[b] = max(c, prev)
	}
	return cuts, joined
}

// zhCutPoints lists the positions right after a mark of zhStops (and after
// any closing marks that follow it), strictly inside r, in ascending order.
func zhCutPoints(r []rune) []int {
	var out []int
	for i := 0; i < len(r); i++ {
		if !strings.ContainsRune(zhStops, r[i]) {
			continue
		}
		p := i + 1
		for p < len(r) && strings.ContainsRune(noLineStart, r[p]) {
			p++
		}
		if p < len(r) && (len(out) == 0 || out[len(out)-1] != p) {
			out = append(out, p)
		}
	}
	return out
}

// zhSoftBreaks are function characters after which a cut rarely splits a
// word (lead ruling on P4.4): used when there is no punctuation nearby.
const zhSoftBreaks = "的了和与或并而在是把被对从向就也都还"

// The soft-break window: ±10% of the Chinese length, but at least 3
// characters so that short translations get one too.
const (
	zhSoftWindowPct = 10
	zhSoftWindowMin = 3
)

// zhSoftWords are common words that begin with a character of zhSoftBreaks;
// a cut is never placed inside them (lead ruling). 向量, 对称 and 对应 are
// frequent in cryptography and mathematics lectures.
var zhSoftWords = map[string]bool{
	"并且": true, "而且": true, "是否": true, "对称": true, "对于": true, "对应": true, "对比": true,
	"从而": true, "从来": true, "还是": true, "还有": true, "就是": true, "也是": true, "都是": true,
	"在于": true, "和平": true, "或者": true, "而言": true, "被动": true, "把握": true, "向量": true,
	"的确": true, "了解": true,
}

// zhSoftPoints lists the positions right after a character of zhSoftBreaks,
// strictly inside r, not followed by a mark that may not begin a line and
// not inside a word of zhSoftWords. The character before such a position is
// Chinese, so it never cuts a Latin run.
func zhSoftPoints(r []rune) []int {
	var out []int
	for i := 0; i+1 < len(r); i++ {
		if strings.ContainsRune(zhSoftBreaks, r[i]) && !strings.ContainsRune(noLineStart, r[i+1]) &&
			!zhSoftWords[string(r[i:i+2])] {
			out = append(out, i+1)
		}
	}
	return out
}

// zhCut places the boundary that follows cum of total English characters:
// at the nearest punctuation within ±30% of the Chinese length; else right
// after the nearest function character of zhSoftBreaks within the soft
// window; else at the proportional position, adjusted by adjustZhCut — or,
// at a pause, nowhere (-1: the cues on both sides share a chunk). The
// proportional position is cum*n/total; distances are compared scaled by
// total to stay in integers. Candidates before prevCut are ignored; at a
// pause so is prevCut itself, which would leave the cue before the pause
// without Chinese of its own: showing the previous chunk again there would
// put the translation of the last clause under the word that begins the
// next one ("Is" of "Is … it clear?").
func zhCut(r []rune, stops, softs []int, cum, total, prevCut int, pause bool) int {
	n := len(r)
	target := int64(cum) * int64(n)
	nearest := func(cands []int, window int64) int { // window: 100 × characters
		best, bestDist := -1, int64(0)
		for _, c := range cands {
			if c < prevCut || (pause && c == prevCut) {
				continue
			}
			d := int64(c)*int64(total) - target
			if d < 0 {
				d = -d
			}
			if 100*d > window*int64(total) {
				continue
			}
			if best < 0 || d < bestDist {
				best, bestDist = c, d
			}
		}
		return best
	}
	if c := nearest(stops, int64(ZhWindowPct)*int64(n)); c >= 0 {
		return c
	}
	if c := nearest(softs, max(int64(zhSoftWindowPct)*int64(n), 100*zhSoftWindowMin)); c >= 0 {
		return c
	}
	if pause {
		return -1
	}
	p := int((2*target + int64(total)) / (2 * int64(total))) // rounded proportional position
	return adjustZhCut(r, max(p, prevCut, 1))
}

// adjustZhCut moves a cut that is not at punctuation forward out of a Latin
// run or past marks that may not begin a line, and back before an opening
// mark that may not end one.
func adjustZhCut(r []rune, p int) int {
	n := len(r)
	for p < n {
		if isFormulaRune(r[p-1]) && isFormulaRune(r[p]) {
			p++
			continue
		}
		if strings.ContainsRune(noLineStart, r[p]) {
			p++
			continue
		}
		break
	}
	if p < n && p > 1 && strings.ContainsRune(zhOpeners, r[p-1]) {
		p--
	}
	return p
}

// isFormulaRune reports whether r belongs to a run of Latin text, digits or
// a formula inside Chinese: anything below the CJK blocks except the
// typographic marks Chinese text uses. Spaces count, so `O(n log n)` and
// English phrases stay whole.
func isFormulaRune(r rune) bool {
	if r >= 0x2E80 {
		return false
	}
	switch r {
	case '‘', '’', '“', '”', '…', '—', '–', '·':
		return false
	}
	return unicode.IsPrint(r) || r == ' '
}
