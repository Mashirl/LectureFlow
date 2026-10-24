// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package subs splits sentences into subtitle cues (SPEC-v2 P4). The player
// and the VTT export both use it, so what the user watches and what they
// download are cut in exactly the same way.
//
// It is pure: no database, files, network, clock or randomness (like
// ctxbuild). The same input always produces the same cues.
package subs

import "strings"

// Limits of SPEC-v2 P4.3 and P4.4.
const (
	MaxCueChars   = 84   // English characters per cue (two lines of 42)
	MaxCueMS      = 6000 // longest cue
	MinCueMS      = 1000 // shortest cue, unless the whole sentence is shorter
	JoinGapMS     = 1500 // a gap between two sentences shorter than this is closed
	ZhWindowPct   = 30   // Chinese punctuation is looked for within ±30% of the Chinese length
	ZhCueMaxChars = 40   // suggested Chinese characters per cue; longer chunks are not split again
	// A pause of PauseCutMS or more between two words of a sentence always
	// ends a cue, and that cue stays on screen at most PauseHoldMS after its
	// last word; the rest of the pause shows no subtitle (lead ruling on
	// P4.3 after the phase 2 acceptance: single words were shown for up to
	// 26 s while the lecturer was silent).
	PauseCutMS  = 1500
	PauseHoldMS = 1500
)

// Word is one word timestamp of the ASR result (P4.2), stored as JSON in
// sentence.words. faster-whisper may split one written word into several
// (`IND` + `-CCA`, `3` + `.14`); the only invariant is that the words,
// concatenated without whitespace, equal asr_text without whitespace.
type Word struct {
	W string `json:"w"`
	S int64  `json:"s"` // ms from lecture start
	E int64  `json:"e"`
}

// Sentence is the part of a sentence row that subtitles depend on.
type Sentence struct {
	Idx     int
	StartMS int64
	EndMS   int64
	Text    string // effective text (sentence.text) — the ONLY English used for cues (P4.3)
	ASRText string // sentence.asr_text, only for aligning Words
	Words   []Word // nil/empty = no word timestamps (old lectures)
	Zh      string // "" = not translated
}

// Cue is one subtitle block. Times are ms from lecture start; a cue is shown
// while StartMS <= t < EndMS (P3).
type Cue struct {
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	En          string `json:"en"`
	Zh          string `json:"zh"`
	SentenceIdx int    `json:"sentence_idx"`
}

// SegmentCues splits one sentence into cues (P4.3, P4.4).
//
// The English of the cues, joined with single spaces, is exactly the words of
// s.Text; times are monotone, cues do not overlap and every cue has
// StartMS < EndMS. A pause of PauseCutMS or more between two words cuts the
// sentence into pieces that are split independently: a cue ends where the
// next one starts, except before such a pause, where it ends PauseHoldMS
// after its last word and the screen stays empty until the next word. A
// sentence without any duration (corrupt timestamps) becomes one cue shown
// for MinCueMS. A sentence without English words yields one Chinese-only cue
// when it has a translation, else nothing.
func SegmentCues(s Sentence) []Cue {
	toks := tokenize(s.Text)
	zh := strings.TrimSpace(s.Zh)
	if len(toks) == 0 {
		if zh == "" {
			return nil
		}
		end := s.EndMS
		if end <= s.StartMS {
			end = s.StartMS + MinCueMS
		}
		return []Cue{{StartMS: s.StartMS, EndMS: end, Zh: zh, SentenceIdx: s.Idx}}
	}
	timeTokens(toks, s)
	n := len(toks)
	var cues []Cue
	var pauses []bool // pauses[k]: cue k starts after a pause
	for i := 0; i < n; {
		j := i + 1
		for j < n && toks[j].s-toks[j-1].e < PauseCutMS {
			j++
		}
		piece, end := toks[i:j], toks[j-1].e
		switch {
		case j < n:
			end = min(toks[j].s, end+PauseHoldMS)
		case i > 0 && end <= piece[0].s:
			end = piece[0].s + MinCueMS // a last piece without duration, like a sentence without one
		}
		for k, sp := range splitSentence(piece, end) {
			e := end
			if sp.j < len(piece) {
				e = piece[sp.j].s
			}
			cues = append(cues, Cue{
				StartMS:     piece[sp.i].s,
				EndMS:       e,
				En:          joinTokens(piece[sp.i:sp.j]),
				SentenceIdx: s.Idx,
			})
			pauses = append(pauses, k == 0 && i > 0)
		}
		i = j
	}
	cues, pauses = dropEmptyCues(cues, pauses)
	assignZh(cues, pauses, zh)
	return cues
}

// SegmentLecture segments every sentence (in order) and applies the
// inter-sentence rule of P4.3: when the gap to the next sentence is < 1.5 s,
// the last cue of the previous sentence ends at the next sentence's start.
//
// Overlapping sentences (the next one starts before the previous one ends)
// are handled by the same rule, which shortens the previous cue. Sentences
// out of order (the next one starts at or before the start of the previous
// cue) are delayed until the previous cue ends, keeping their durations, so
// the result never overlaps and no text is lost.
func SegmentLecture(ss []Sentence) []Cue {
	var out []Cue
	for _, s := range ss {
		cues := SegmentCues(s)
		if len(cues) == 0 {
			continue
		}
		if len(out) > 0 {
			last := &out[len(out)-1]
			if next := cues[0].StartMS; next > last.StartMS && next-last.EndMS < JoinGapMS {
				last.EndMS = next
			}
			prevEnd := last.EndMS
			for k := range cues {
				c := &cues[k]
				if c.StartMS >= prevEnd {
					break
				}
				d := c.EndMS - c.StartMS
				c.StartMS = prevEnd
				if c.EndMS < c.StartMS+d {
					c.EndMS = c.StartMS + d
				}
				prevEnd = c.EndMS
			}
		}
		out = append(out, cues...)
	}
	return out
}

// dropEmptyCues merges every cue without duration into its neighbour (the
// previous one, or the next one for the first cue); pauses follows the cues.
// The splitter avoids such cues whenever the sentence has any duration, so
// this only guards degenerate timestamps. A sentence without any duration
// keeps one cue of MinCueMS.
func dropEmptyCues(cues []Cue, pauses []bool) ([]Cue, []bool) {
	out, outP := cues[:0], pauses[:0]
	for k, c := range cues {
		switch {
		case c.EndMS > c.StartMS:
			out, outP = append(out, c), append(outP, pauses[k])
		case len(out) > 0:
			p := &out[len(out)-1]
			p.En += " " + c.En
			if c.EndMS > p.EndMS {
				p.EndMS = c.EndMS
			}
		default:
			out, outP = append(out, c), append(outP, pauses[k]) // first cue: merged with the next one below
		}
	}
	if len(out) > 1 && out[0].EndMS <= out[0].StartMS {
		out[1].En = out[0].En + " " + out[1].En
		out[1].StartMS = out[0].StartMS
		out, outP = out[1:], outP[1:]
		outP[0] = false
	}
	if len(out) == 1 && out[0].EndMS <= out[0].StartMS {
		out[0].EndMS = out[0].StartMS + MinCueMS
	}
	return out, outP
}
