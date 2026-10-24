// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"go/parser"
	gotoken "go/token"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The package is pure (P4.3): its non-test files may only import these.
var allowedImports = map[string]bool{"strings": true, "unicode": true, "unicode/utf8": true}

func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(gotoken.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); !allowedImports[p] {
				t.Errorf("%s imports %s (internal/subs must not do any I/O)", f, p)
			}
		}
	}
}

// --- random input ---

var vocab = strings.Fields(`the a key cipher block message we send encrypt decrypt and but so because which
where when of to in is it channel attacker secure random nonce AES-256 IND-CCA two-way 3.14 e.g. Dr. it's
don't authentication cryptographically pseudorandomness Diffie-Hellman O(n log n) — x RSA proof bound`)

var conjunctionList = []string{"and", "but", "so", "because", "which", "where", "when"}

// fragments are kept whole in the Chinese; they are checked after splitting.
var fragments = []string{"IND-CCA", "AES-256", "O(n log n)", "RSA", "3.14"}

const zhPool = "我们证明这个方案在标准模型下是安全的并且效率很高攻击者无法区分密文与随机串密钥长度决定了安全强度对称向量的内积就是零"

type gen struct{ r *rand.Rand }

func (g gen) pick(xs []string) string { return xs[g.r.Intn(len(xs))] }
func (g gen) chance(p float64) bool   { return g.r.Float64() < p }

// script returns 1..70 written words with random punctuation.
func (g gen) script() []string {
	n := 1 + g.r.Intn(70)
	if g.chance(0.2) {
		n = 1 + g.r.Intn(4)
	}
	out := make([]string, n)
	for i := range out {
		w := g.pick(vocab)
		if g.chance(0.1) {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		if g.chance(0.15) {
			w += g.pick([]string{",", ",", ".", "?", "!", ";", ":"})
		}
		out[i] = w
	}
	return out
}

// speakRandom times the written words like a speaker (about 60 ms per
// character, short gaps, occasional pauses) and splits some of them into
// several whisper timestamps the way faster-whisper does (`IND` + `-CCA`).
// It returns the words and, per written word, the index of its first and
// last timestamp.
func (g gen) speakRandom(start int64, written []string) ([]Word, [][2]int) {
	var words []Word
	var span [][2]int
	t := start
	for _, w := range written {
		d := 80 + int64(utf8.RuneCountInString(w))*int64(40+g.r.Intn(40))
		pieces := []string{w}
		if i := strings.IndexAny(w[1:], "-.'"); i >= 0 && g.chance(0.6) {
			pieces = []string{w[:i+1], w[i+1:]}
		}
		first := len(words)
		at := t
		for k, p := range pieces {
			e := t + d*int64(k+1)/int64(len(pieces))
			if k == 0 && g.chance(0.5) {
				p = " " + p
			}
			words = append(words, Word{W: p, S: at, E: e})
			at = e
		}
		span = append(span, [2]int{first, len(words) - 1})
		t += d + int64(20+g.r.Intn(180))
		if g.chance(0.05) {
			t += int64(600 + g.r.Intn(1400))
		}
	}
	return words, span
}

// edit returns a repaired version of the written words: inserted, deleted,
// replaced words, case and punctuation changes.
func (g gen) edit(ws []string) []string {
	var out []string
	for _, w := range ws {
		switch x := g.r.Float64(); {
		case x < 0.06:
			continue // deleted
		case x < 0.12:
			out = append(out, g.pick(vocab), w) // inserted
		case x < 0.17:
			out = append(out, g.pick(vocab)) // replaced
		case x < 0.27:
			out = append(out, strings.ToUpper(w))
		case x < 0.35:
			out = append(out, strings.TrimRight(w, ",.?!;:")+g.pick([]string{",", ".", ";", ""}))
		default:
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		out = append(out, g.pick(conjunctionList))
	}
	return out
}

func (g gen) zh() string {
	if g.chance(0.25) {
		return ""
	}
	pool := []rune(zhPool)
	var b strings.Builder
	for n := 1 + g.r.Intn(6); n > 0; n-- {
		i := g.r.Intn(len(pool))
		j := min(len(pool), i+1+g.r.Intn(14))
		b.WriteString(string(pool[i:j]))
		if g.chance(0.3) {
			f := g.pick(fragments)
			if g.chance(0.5) {
				f = " " + f + " "
			}
			b.WriteString(f)
			b.WriteString(string(pool[g.r.Intn(len(pool))]))
		}
		if g.chance(0.6) {
			b.WriteString(g.pick([]string{"，", "，", "。", "；", "：", "？", "！", "、"}))
		}
	}
	b.WriteString("。")
	return b.String()
}

// sentence returns a random sentence starting at start and, for word
// timestamps kept as they are, the mapping of written words to timestamps.
func (g gen) sentence(idx int, start int64) (Sentence, [][2]int) {
	written := g.script()
	words, span := g.speakRandom(start, written)
	asr := strings.Join(written, " ")
	s := Sentence{Idx: idx, StartMS: words[0].S, EndMS: words[len(words)-1].E, Text: asr, ASRText: asr, Words: words, Zh: g.zh()}
	if g.chance(0.5) {
		s.Text = strings.Join(g.edit(written), " ")
		span = nil
	}
	if g.chance(0.2) {
		s.Words = nil
		span = nil
	}
	return s, span
}

// --- properties ---

func TestPropertiesSentences(t *testing.T) {
	g := gen{rand.New(rand.NewSource(20261023))}
	for iter := 0; iter < 3000; iter++ {
		s, span := g.sentence(iter, int64(g.r.Intn(5_000_000)))
		cues := SegmentCues(s)
		if !reflect.DeepEqual(cues, SegmentCues(s)) {
			t.Fatalf("sentence %d: not deterministic", iter)
		}
		checkCues(t, []Sentence{s}, cues)
		checkWordTimes(t, s, span)
		checkSentenceLimits(t, s, cues)
		checkZh(t, s, cues)
		if t.Failed() {
			t.Fatalf("failing input: %#v", s)
		}
	}
	if softCuts < 100 {
		t.Errorf("only %d boundaries exercised the function-character rule", softCuts)
	}
	t.Logf("%d function-character cuts, %d pauses inside sentences, %d joined in the Chinese", softCuts, pauseCuts, joinedPauses)
	if pauseCuts < 100 || joinedPauses < 20 {
		t.Errorf("only %d pauses inside sentences, %d of them joined in the Chinese", pauseCuts, joinedPauses)
	}
}

func checkWordTimes(t *testing.T, s Sentence, span [][2]int) {
	t.Helper()
	wt := WordTimes(s)
	fields := strings.Fields(s.Text)
	if len(wt) != len(fields) {
		t.Fatalf("%d word times for %d words", len(wt), len(fields))
	}
	for i, w := range wt {
		if w.W != fields[i] || w.E < w.S || (i > 0 && w.S < wt[i-1].S) {
			t.Fatalf("word time %d = %+v (previous %+v)", i, w, wt[max(i-1, 0)])
		}
		// Unchanged text: a written word spans exactly its whisper pieces.
		if span != nil && (w.S != s.Words[span[i][0]].S || w.E != s.Words[span[i][1]].E) {
			t.Fatalf("word %d %q = %d-%d, want %d-%d", i, w.W, w.S, w.E, s.Words[span[i][0]].S, s.Words[span[i][1]].E)
		}
	}
}

// cueWords returns, for each cue, the index of its first word in
// WordTimes(s) and the index after its last word.
func cueWords(t *testing.T, cues []Cue) [][2]int {
	t.Helper()
	out := make([][2]int, len(cues))
	k := 0
	for i, c := range cues {
		n := len(strings.Fields(c.En))
		out[i] = [2]int{k, k + n}
		k += n
	}
	return out
}

// pauseBefore reports, for each word, whether a pause of PauseCutMS or more
// separates it from the previous word.
func pauseBefore(wt []Word) []bool {
	out := make([]bool, len(wt))
	for k := 1; k < len(wt); k++ {
		out[k] = wt[k].S-wt[k-1].E >= PauseCutMS
	}
	return out
}

// pauseCuts counts sentence-internal pauses exercised by the properties.
var pauseCuts int

// checkSentenceLimits: with the generator's speech rate every limit of P4.3
// can be met, so every cue must meet it. A pause of PauseCutMS or more
// between two words always ends a cue, which stays on screen PauseHoldMS
// after its last word (lead ruling after the phase 2 acceptance); elsewhere
// the cues of a sentence are contiguous. The 1 s minimum applies within each
// piece between such pauses that lasts at least 1 s (like a whole sentence).
func checkSentenceLimits(t *testing.T, s Sentence, cues []Cue) {
	t.Helper()
	wt := WordTimes(s)
	pause := pauseBefore(wt)
	cw := cueWords(t, cues)
	// pieceDur[k]: the displayed duration of the piece holding word k.
	pieceDur := make([]int64, len(wt))
	for i := 0; i < len(wt); {
		j := i + 1
		for j < len(wt) && !pause[j] {
			j++
		}
		end := wt[j-1].E
		if j < len(wt) {
			end += PauseHoldMS
			pauseCuts++
		}
		for k := i; k < j; k++ {
			pieceDur[k] = end - wt[i].S
		}
		i = j
	}
	starts := map[int]bool{}
	for i, c := range cues {
		starts[cw[i][0]] = true
		if n := utf8.RuneCountInString(c.En); n > MaxCueChars {
			t.Errorf("cue %d: %d characters", i, n)
		}
		d := c.EndMS - c.StartMS
		if d > MaxCueMS {
			t.Errorf("cue %d: %d ms", i, d)
		}
		if pieceDur[cw[i][0]] >= MinCueMS && d < MinCueMS {
			t.Errorf("cue %d: %d ms in a piece of %d ms", i, d, pieceDur[cw[i][0]])
		}
		first, next := cw[i][0], cw[i][1]
		if c.StartMS != wt[first].S {
			t.Errorf("cue %d starts at %d, its first word at %d", i, c.StartMS, wt[first].S)
		}
		var want int64
		switch {
		case next == len(wt):
			want = wt[next-1].E
		case pause[next]:
			want = wt[next-1].E + PauseHoldMS
		default:
			want = wt[next].S
		}
		if c.EndMS != want {
			t.Errorf("cue %d ends at %d, want %d (pause after it: %v)", i, c.EndMS, want, next < len(wt) && pause[next])
		}
		if c.EndMS-wt[next-1].E > PauseHoldMS {
			t.Errorf("cue %d stays %d ms after its last word", i, c.EndMS-wt[next-1].E)
		}
	}
	for k, p := range pause {
		if p && !starts[k] {
			t.Errorf("no cue starts after the pause before word %d %q", k, wt[k].W)
		}
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func checkZh(t *testing.T, s Sentence, cues []Cue) {
	t.Helper()
	if strings.TrimSpace(s.Zh) == "" {
		for i, c := range cues {
			if c.Zh != "" {
				t.Errorf("cue %d has Chinese %q for an untranslated sentence", i, c.Zh)
			}
		}
		return
	}
	for i, c := range cues {
		if c.Zh == "" {
			t.Errorf("cue %d has no Chinese", i)
		}
	}
	// The cuts partition the translation; each cue shows its trimmed chunk,
	// or the previous cue's Chinese when its chunk is empty. Cues around a
	// joined pause share the chunk of the run.
	r := []rune(strings.TrimSpace(s.Zh))
	lens := make([]int, len(cues))
	for i, c := range cues {
		lens[i] = utf8.RuneCountInString(c.En)
	}
	pauses := make([]bool, len(cues))
	if s.Text != "" {
		pw := pauseBefore(WordTimes(s))
		for i, w := range cueWords(t, cues) {
			pauses[i] = i > 0 && pw[w[0]]
		}
	}
	cuts, joined := zhCuts(r, lens, pauses)
	for i, c := range cues {
		j := i + 1
		for j < len(cues) && joined[j] {
			j++
		}
		g := i // first cue of the run
		for g > 0 && joined[g] {
			g--
		}
		want := strings.TrimSpace(string(r[cuts[g]:cuts[j]]))
		if want == "" && g > 0 {
			want = cues[g-1].Zh
		}
		if c.Zh != want {
			t.Errorf("cue %d Chinese %q, want %q (cuts %v, joined %v)", i, c.Zh, want, cuts, joined)
		}
		if joined[i] && (!pauses[i] || cuts[i] != cuts[i-1]) {
			t.Errorf("boundary %d joined without a pause, or with a cut (cuts %v, pauses %v)", i, cuts, pauses)
		}
	}
	// No cut falls inside a Latin fragment or right before punctuation.
	text := string(r)
	for _, f := range fragments {
		for off := 0; ; {
			i := strings.Index(text[off:], f)
			if i < 0 {
				break
			}
			lo := utf8.RuneCountInString(text[:off+i])
			hi := lo + utf8.RuneCountInString(f)
			for _, c := range cuts {
				if c > lo && c < hi {
					t.Errorf("cut %d inside %q at %d-%d of %q", c, f, lo, hi, text)
				}
			}
			off += i + len(f)
		}
	}
	for b, c := range cuts[1 : len(cuts)-1] {
		if !joined[b+1] && c < len(r) && strings.ContainsRune("，。；：？！、", r[c]) {
			t.Errorf("cut %d puts punctuation first: %q", c, string(r[c:]))
		}
	}
	checkSoftCuts(t, r, lens, cuts, joined, pauses)
}

// softWordList: the lead's list of words a cut may not split.
const softWordList = "并且 而且 是否 对称 对于 对应 对比 从而 从来 还是 还有 就是 也是 都是 在于 和平 或者 而言 被动 把握 向量 的确 了解"

// softCuts counts boundaries placed by the function-character rule;
// joinedPauses counts pauses whose two cues share a chunk.
var softCuts, joinedPauses int

// checkSoftCuts re-derives the lead ruling on P4.4 independently: when no
// punctuation lies within ±30% of the proportional position but a position
// right after 的、和、在… (not before punctuation, not before the previous
// cut) lies within max(±10%, 3 characters), the cut is at the nearest one.
// At a pause (the lead ruling after the phase 2 acceptance) the candidate
// must leave the cue before it some Chinese (be after the previous cut);
// when there is none the boundary is joined; elsewhere it never is.
func checkSoftCuts(t *testing.T, r []rune, lens, cuts []int, joined, pauses []bool) {
	t.Helper()
	n, total, cum := len(r), 0, 0
	for _, l := range lens {
		total += l
	}
	dist := func(c int) float64 {
		d := float64(c) - float64(cum)*float64(n)/float64(total)
		if d < 0 {
			d = -d
		}
		return d
	}
	for b := 1; b < len(lens); b++ {
		cum += lens[b-1]
		prev := cuts[b-1]
		stop := false
		for i := range r {
			if !strings.ContainsRune("，。；：？！、", r[i]) {
				continue
			}
			c := i + 1 // after the mark and the closing marks that follow it, strictly inside
			for c < n && strings.ContainsRune("，。；：？！、）》」』】〉”’…%,.;:?!)]}", r[c]) {
				c++
			}
			if c < n && c >= prev && (c > prev || !pauses[b]) && dist(c) <= 0.3*float64(n) {
				stop = true
			}
		}
		if stop {
			if joined[b] {
				t.Errorf("boundary %d joined although punctuation is near (cuts %v, of %q)", b, cuts, string(r))
			}
			continue
		}
		window := max(0.1*float64(n), 3)
		best := -1
		for c := max(prev, 1); c < n; c++ {
			if c == prev && pauses[b] {
				continue // at a pause, both cues keep some Chinese
			}
			if strings.ContainsRune("的了和与或并而在是把被对从向就也都还", r[c-1]) &&
				!strings.ContainsRune("，。；：？！、）》」』】〉”’…%,.;:?!)]}", r[c]) &&
				!slices.Contains(strings.Fields(softWordList), string(r[c-1:c+1])) &&
				dist(c) <= window && (best < 0 || dist(c) < dist(best)) {
				best = c
			}
		}
		if best < 0 {
			if pauses[b] != joined[b] {
				t.Errorf("boundary %d: pause %v, joined %v (cuts %v, lens %v, of %q)", b, pauses[b], joined[b], cuts, lens, string(r))
			}
			if joined[b] {
				joinedPauses++
			}
			continue
		}
		softCuts++
		if cuts[b] != best || joined[b] {
			t.Errorf("boundary %d at %d (joined %v), want %d right after %q (cuts %v, lens %v, target %.2f, of %q)", b, cuts[b], joined[b], best, r[best-1], cuts, lens, float64(cum)*float64(n)/float64(total), string(r))
		}
	}
}

func TestPropertiesLecture(t *testing.T) {
	g := gen{rand.New(rand.NewSource(7))}
	for iter := 0; iter < 300; iter++ {
		var ss []Sentence
		at := int64(g.r.Intn(100_000))
		for i := 0; i < 1+g.r.Intn(12); i++ {
			s, _ := g.sentence(i, at)
			ss = append(ss, s)
			at = s.EndMS + int64(g.r.Intn(3500)) - 300 // sometimes overlapping
		}
		cues := SegmentLecture(ss)
		checkCues(t, ss, cues)
		// Each sentence keeps its own cues, except that the last one is
		// closed up to the next sentence when the gap is under 1.5 s.
		k := 0
		for i, s := range ss {
			own := SegmentCues(s)
			got := cues[k : k+len(own)]
			k += len(own)
			if k > len(own) && cues[k-len(own)-1].EndMS > own[0].StartMS {
				continue // delayed behind an out-of-order predecessor: only the invariants hold
			}
			if i+1 < len(ss) {
				next, last := SegmentCues(ss[i+1])[0], own[len(own)-1]
				if next.StartMS > last.StartMS && next.StartMS-last.EndMS < JoinGapMS {
					own[len(own)-1].EndMS = next.StartMS
				}
			}
			if !reflect.DeepEqual(got, own) {
				t.Fatalf("lecture %d sentence %d:\n got %+v\nwant %+v", iter, i, got, own)
			}
		}
	}
}
