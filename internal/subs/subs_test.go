// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// checkLimits verifies the P4.3 limits that hold whenever they can be met.
func checkLimits(t *testing.T, cues []Cue) {
	t.Helper()
	for i, c := range cues {
		if n := utf8.RuneCountInString(c.En); n > MaxCueChars {
			t.Errorf("cue %d has %d characters: %q", i, n, c.En)
		}
		d := c.EndMS - c.StartMS
		if d > MaxCueMS || d < MinCueMS {
			t.Errorf("cue %d lasts %d ms: %q", i, d, c.En)
		}
	}
}

// --- English timing (P4.3, P4.5 case 1) ---

func TestSameText(t *testing.T) {
	s := spoken(4, 61_000,
		"Today we look at block ciphers, and in particular at how AES turns a key and a block of plaintext into ciphertext.",
		"", "今天我们来看分组密码，特别是 AES 如何把密钥和一个明文分组变成密文。")
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	// text == asr_text: every word keeps its own timestamp.
	for i, w := range WordTimes(s) {
		if w.S != s.Words[i].S || w.E != s.Words[i].E {
			t.Errorf("word %d %q: %d-%d, want %d-%d", i, w.W, w.S, w.E, s.Words[i].S, s.Words[i].E)
		}
	}
	if len(cues) < 2 || cues[0].StartMS != s.Words[0].S || cues[len(cues)-1].EndMS != s.Words[len(s.Words)-1].E {
		t.Errorf("cues do not span the words: %+v", cues)
	}
	golden(t, "same_text.golden", render([]Sentence{s}, cues, true))
}

// faster-whisper splits written words into several timestamps (`IND` +
// `-CCA`, `two` + `-way`, `3` + `.14`); words are mapped onto the text by
// their characters, not by index.
func TestSameTextSplitWords(t *testing.T) {
	words := []Word{
		{"The", 5000, 5150}, {"cipher", 5200, 5600}, {"is", 5650, 5800},
		{"IND", 5850, 6200}, {"-CCA", 6200, 6700}, {"secure,", 6750, 7300},
		{"so", 7500, 7650}, {"a", 7700, 7750}, {"two", 7800, 8000}, {"-way", 8000, 8300},
		{"channel", 8350, 8800}, {"with", 8850, 9000}, {" 3", 9050, 9300}, {".14", 9300, 9700},
		{"bits", 9750, 10000}, {"leaks", 10050, 10400}, {"nothing.", 10450, 11000},
	}
	text := "The cipher is IND-CCA secure, so a two-way channel with 3.14 bits leaks nothing."
	s := Sentence{Idx: 0, StartMS: 5000, EndMS: 11000, Text: text, ASRText: text, Words: words,
		Zh: "该密码是 IND-CCA 安全的，所以带 3.14 比特的双向信道不会泄露任何信息。"}
	wt := WordTimes(s)
	if len(wt) != 14 {
		t.Fatalf("%d word times, want 14: %+v", len(wt), wt)
	}
	for _, c := range []struct {
		i    int
		w    string
		s, e int64
	}{{3, "IND-CCA", 5850, 6700}, {7, "two-way", 7800, 8300}, {10, "3.14", 9050, 9700}, {13, "nothing.", 10450, 11000}} {
		if wt[c.i].W != c.w || wt[c.i].S != c.s || wt[c.i].E != c.e {
			t.Errorf("word %d = %+v, want %s[%d-%d]", c.i, wt[c.i], c.w, c.s, c.e)
		}
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	golden(t, "same_text_split_words.golden", render([]Sentence{s}, cues, true))
}

// --- English timing (P4.5 case 2: text differs from asr_text) ---

func TestDiffInsertedWord(t *testing.T) {
	s := spoken(7, 120_000,
		"the key is kept secret by both parties +900 and never sent over the network",
		"The shared key is kept secret by both parties, and it is never sent over the network.",
		"双方都对共享密钥保密，并且它从不通过网络发送。")
	wt := WordTimes(s)
	// Aligned words keep their timestamps even with a different case or
	// punctuation; "shared", "it", "is" are interpolated between neighbours.
	want := map[int]int{0: 0, 2: 1, 3: 2, 7: 6, 8: 7, 9: 8, 12: 9, 15: 12}
	for ti, wi := range want {
		if wt[ti].S != s.Words[wi].S || wt[ti].E != s.Words[wi].E {
			t.Errorf("text word %d %q = %d-%d, want word %d %d-%d", ti, wt[ti].W, wt[ti].S, wt[ti].E, wi, s.Words[wi].S, s.Words[wi].E)
		}
	}
	if wt[1].S < s.Words[0].E || wt[1].E > s.Words[1].S {
		t.Errorf("inserted %q at %d-%d, outside the gap %d-%d", wt[1].W, wt[1].S, wt[1].E, s.Words[0].E, s.Words[1].S)
	}
	if wt[10].S < s.Words[8].E || wt[11].E > s.Words[9].S || wt[10].S > wt[11].S {
		t.Errorf("inserted %q %q at %d, %d: outside the gap %d-%d", wt[10].W, wt[11].W, wt[10].S, wt[11].S, s.Words[8].E, s.Words[9].S)
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	golden(t, "diff_insert.golden", render([]Sentence{s}, cues, true))
}

func TestDiffDeletedWords(t *testing.T) {
	s := spoken(8, 200_000,
		"so um the the cipher text is sent over the channel +400 together with the tag",
		"So the ciphertext is sent over the channel together with the tag.",
		"所以密文会和标签一起通过信道发送。")
	wt := WordTimes(s)
	// "um" and one "the" are gone; "ciphertext" replaces "cipher text" and
	// takes the time between "the" and "is".
	if wt[0].S != s.Words[0].S || wt[1].S != s.Words[3].S {
		t.Errorf("So/the = %+v %+v, want starts %d %d", wt[0], wt[1], s.Words[0].S, s.Words[3].S)
	}
	if wt[2].W != "ciphertext" || wt[2].S != s.Words[3].E || wt[2].E != s.Words[6].S {
		t.Errorf("ciphertext = %+v, want %d-%d", wt[2], s.Words[3].E, s.Words[6].S)
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	golden(t, "diff_delete.golden", render([]Sentence{s}, cues, true))
}

func TestDiffCaseAndPunctuation(t *testing.T) {
	s := spoken(9, 300_000,
		"we use aes for the data and rsa for the key exchange because rsa is slow",
		"We use AES for the data, and RSA for the key exchange, because RSA is slow.",
		"我们用 AES 加密数据，用 RSA 交换密钥，因为 RSA 很慢。")
	wt := WordTimes(s)
	for i := range wt {
		if wt[i].S != s.Words[i].S || wt[i].E != s.Words[i].E {
			t.Errorf("word %d %q = %d-%d, want %d-%d", i, wt[i].W, wt[i].S, wt[i].E, s.Words[i].S, s.Words[i].E)
		}
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	for _, c := range cues {
		if strings.Contains(c.En, "aes") || strings.Contains(c.En, "rsa") {
			t.Errorf("cue uses asr_text: %q", c.En)
		}
	}
	golden(t, "diff_case.golden", render([]Sentence{s}, cues, true))
}

// --- English timing (P4.5 case 3: no word timestamps) ---

func TestNoWords(t *testing.T) {
	text := "This lecture was transcribed before word timestamps existed, so its time is spread over the characters."
	s := Sentence{Idx: 2, StartMS: 10_000, EndMS: 17_400, Text: text, ASRText: text,
		Zh: "这节课在有词级时间戳之前就已转写，因此时间按字符平均分配。"}
	wt := WordTimes(s)
	n := int64(utf8.RuneCountInString(text))
	pos := int64(0)
	for _, w := range wt {
		if want := s.StartMS + 7400*pos/n; w.S != want {
			t.Errorf("%q starts at %d, want %d", w.W, w.S, want)
		}
		pos += int64(utf8.RuneCountInString(w.W)) + 1
	}
	if last := wt[len(wt)-1]; last.E != s.EndMS {
		t.Errorf("last word ends at %d, want %d", last.E, s.EndMS)
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	golden(t, "no_words.golden", render([]Sentence{s}, cues, false))
}

// --- Splitting (P4.5 case 2: a 30-second sentence) ---

func TestLongSentence(t *testing.T) {
	s := spoken(12, 600_000,
		"So what we want to do today is look at how a block cipher like AES actually works inside, "+
			"because once you understand the round structure, the key schedule and the way the S-boxes "+
			"give you confusion, +700 you will see why the attacks we discussed last week do not apply "+
			"to it, and why people still trust it after more than twenty years of public analysis by "+
			"some of the best cryptographers in the world.",
		"", "所以今天我们要看看像 AES 这样的分组密码内部究竟如何工作，因为一旦你理解了轮结构、密钥编排以及 S 盒提供混淆的方式，"+
			"你就会明白为什么我们上周讨论的攻击对它不适用，以及为什么在世界上一些最好的密码学家二十多年的公开分析之后，人们仍然信任它。")
	if d := s.EndMS - s.StartMS; d < 29_000 || d > 33_000 {
		t.Fatalf("test sentence lasts %d ms, want about 30 s", d)
	}
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	if len(cues) < 5 {
		t.Errorf("%d cues for a 30 s sentence of %d characters", len(cues), len(s.Text))
	}
	golden(t, "long_sentence.golden", render([]Sentence{s}, cues, false))
}

func TestBreakPriority(t *testing.T) {
	for _, c := range []struct {
		prev, next string
		want       int
	}{
		{"works.", "So", priStop},
		{"right?", "Yes", priStop},
		{`"done!"`, "and", priStop},
		{"e.g.", "AES", priWord},
		{"Dr.", "Smith", priWord},
		{"A.", "Turing", priWord},
		{"i.e.,", "the", priComma},
		{"follows:", "first", priColon},
		{"block;", "then", priColon},
		{"key,", "and", priComma},
		{"key—", "the", priComma},
		{"--", "the", priComma},
		{"two-", "way", priWord},
		{"data", "and", priConjunction},
		{"data", "Because", priConjunction},
		{"data", "(which", priConjunction},
		{"data", "android", priWord},
	} {
		if got := breakPriority(c.prev, c.next); got != c.want {
			t.Errorf("breakPriority(%q, %q) = %d, want %d", c.prev, c.next, got, c.want)
		}
	}
}

// A higher-priority break wins over a more even one, and among equal
// priorities the most even one wins.
func TestSplitChoosesPriorityThenBalance(t *testing.T) {
	// 7.6 s, short enough in characters: one cut needed.
	s := spoken(0, 0, "the sender encrypts the message +500 with the shared key, and then the receiver decrypts it again", "", "")
	cues := SegmentCues(s)
	checkLimits(t, cues)
	if len(cues) != 2 || !strings.HasSuffix(cues[0].En, "key,") {
		t.Errorf("want a cut after the comma, got %+v", cues)
	}
	s = spoken(0, 0, "the sender encrypts the message with the shared key and then the receiver decrypts the message", "", "")
	cues = SegmentCues(s)
	checkLimits(t, cues)
	if len(cues) != 2 || !strings.HasPrefix(cues[1].En, "and ") {
		t.Errorf("want a cut before the conjunction, got %+v", cues)
	}
}

// --- Chinese distribution (P4.4) ---

func TestZhAtPunctuation(t *testing.T) {
	s := spoken(3, 40_000,
		"First we pad the message to a multiple of the block size, +600 then we split it into blocks, "+
			"+600 and finally we encrypt every block with the same key in counter mode.",
		"", "首先我们把消息填充到分组长度的整数倍，然后把它分成若干分组，最后用同一个密钥在计数器模式下加密每个分组。")
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	checkLimits(t, cues)
	if len(cues) != 3 {
		t.Fatalf("want 3 cues, got %+v", cues)
	}
	for i, want := range []string{"首先我们把消息填充到分组长度的整数倍，", "然后把它分成若干分组，", "最后用同一个密钥在计数器模式下加密每个分组。"} {
		if cues[i].Zh != want {
			t.Errorf("cue %d zh = %q, want %q", i, cues[i].Zh, want)
		}
	}
	golden(t, "zh_punctuation.golden", render([]Sentence{s}, cues, false))
}

// Without punctuation near the proportional position, the cut must not fall
// inside `IND-CCA`, `AES-256` or `O(n log n)`. Each translation is written so
// that the plain proportional position lands inside the fragment (checked
// below), and its only punctuation is the final full stop.
func TestZhKeepsFormulas(t *testing.T) {
	cases := []struct{ head, frag, tail string }{
		{"我们证明该方案满足", "IND-CCA", "的安全性并分析效率。"},
		{"我们的实现使用", "AES-256", "加密所有数据块。"},
		{"该算法的复杂度为", "O(n log n)", "而且常数因子很小。"},
	}
	var all []Sentence
	var allCues []Cue
	for i, c := range cases {
		s := spoken(i, int64(i)*20_000,
			"Now we show that the construction is secure under the standard assumption, "+
				"and we analyse its cost and the details of an implementation in practice.", "",
			c.head+c.frag+c.tail)
		cues := SegmentCues(s)
		checkCues(t, []Sentence{s}, cues)
		if len(cues) != 2 {
			t.Fatalf("want 2 cues, got %+v", cues)
		}
		cum := utf8.RuneCountInString(cues[0].En)
		total := cum + utf8.RuneCountInString(cues[1].En)
		h, l, n := utf8.RuneCountInString(c.head), utf8.RuneCountInString(c.frag), utf8.RuneCountInString(s.Zh)
		if p := (2*cum*n + total) / (2 * total); p <= h || p >= h+l {
			t.Fatalf("%s: the plain cut %d is not inside the fragment (%d, %d); adjust the test", c.frag, p, h, h+l)
		}
		if cues[0].Zh != c.head+c.frag || cues[1].Zh != c.tail {
			t.Errorf("%s was not kept whole: %q | %q", c.frag, cues[0].Zh, cues[1].Zh)
		}
		all = append(all, s)
		allCues = append(allCues, cues...)
	}
	golden(t, "zh_formula.golden", render(all, allCues, false))
}

// Lead ruling on P4.4: without punctuation within ±30%, cut right after the
// nearest function character (的、和、在…) within ±10% (at least 3
// characters); only without one, at the proportional position.
func TestZhSoftBreak(t *testing.T) {
	for _, c := range []struct {
		zh   string
		want []string
	}{
		// 的 one character before the middle: not "…的基|本…".
		{"我们今天学习分组密码的基本结构以及常见攻击方法。", []string{"我们今天学习分组密码的", "基本结构以及常见攻击方法。"}},
		// Punctuation within ±30% still wins over a nearer 的.
		{"我们今天学习分组密码的基本结构，以及常见攻击方法。", []string{"我们今天学习分组密码的基本结构，", "以及常见攻击方法。"}},
		// The only 的 is far from the middle: proportional cut.
		{"我们今天学习分组密码基本结构以及常见攻击的方法。", []string{"我们今天学习分组密码基本", "结构以及常见攻击的方法。"}},
		// A cut after 在 may precede a Latin run (it does not cut it).
		{"证明假设攻击者在IND-CCA游戏中询问", []string{"证明假设攻击者在", "IND-CCA游戏中询问"}},
	} {
		r := []rune(c.zh)
		cuts, _ := zhCuts(r, []int{50, 50}, nil)
		got := []string{string(r[cuts[0]:cuts[1]]), string(r[cuts[1]:cuts[2]])}
		if got[0] != c.want[0] || got[1] != c.want[1] {
			t.Errorf("%s: cut into %q, want %q", c.zh, got, c.want)
		}
	}
	// Never inside a common word that starts with a function character: 并
	// and 向 are the nearest candidates here, but 并且 and 向量 stay whole.
	for _, c := range []struct {
		zh   string
		lens []int
		want string // first chunk
	}{
		{"这个方案很安全并且计算效率很高也易实现。", []int{45, 55}, "这个方案很安全并且"},
		{"我们先把这两个向量相加再取模。", []int{60, 40}, "我们先把这两个向量"},
	} {
		r := []rune(c.zh)
		cuts, _ := zhCuts(r, c.lens, nil)
		if got := string(r[:cuts[1]]); got != c.want {
			t.Errorf("%s: first chunk %q, want %q", c.zh, got, c.want)
		}
	}
	for w := range zhSoftWords {
		if got := zhSoftPoints([]rune(w + "中")); len(got) != 0 && got[0] == 1 {
			t.Errorf("%s: cut allowed inside the word", w)
		}
	}
	// No candidate before a mark that may not begin a line, or at the end.
	if got := zhSoftPoints([]rune("快速的”变换和")); len(got) != 0 {
		t.Errorf("soft points %v, want none", got)
	}
	if got := zhSoftPoints([]rune("在AES中")); len(got) != 1 || got[0] != 1 {
		t.Errorf("soft points %v, want [1]", got)
	}
}

func TestAdjustZhCut(t *testing.T) {
	for _, c := range []struct {
		zh   string
		at   int // cut position in runes
		want string
	}{
		{"安全性满足IND-CCA并且高效", 7, "安全性满足IND-CCA"},
		{"安全性满足IND-CCA并且高效", 5, "安全性满足"}, // before the run: allowed
		{"使用 AES-256 加密数据", 6, "使用 AES-256 "},
		{"复杂度为O(n log n)的算法", 9, "复杂度为O(n log n)"},
		{"复杂度为O(n log n)", 9, "复杂度为O(n log n)"}, // the run reaches the end: no cut
		{"他说“这是密钥”然后走了", 3, "他说"},               // never end a chunk with an opening mark
		{"这是密钥）然后走了", 4, "这是密钥）"},               // never start a chunk with a closing mark
	} {
		r := []rune(c.zh)
		if got := string(r[:adjustZhCut(r, c.at)]); got != c.want {
			t.Errorf("adjustZhCut(%q, %d) cuts after %q, want %q", c.zh, c.at, got, c.want)
		}
	}
}

func TestZhNotTranslated(t *testing.T) {
	s := spoken(5, 80_000,
		"Before we start, let me remind you that the coursework is due next Friday, "+
			"+500 and that late submissions will lose ten percent per day.", "", "")
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	if len(cues) < 2 {
		t.Fatalf("want several English cues, got %+v", cues)
	}
	for i, c := range cues {
		if c.Zh != "" || c.En == "" {
			t.Errorf("cue %d = %+v, want English only", i, c)
		}
	}
	golden(t, "zh_empty.golden", render([]Sentence{s}, cues, false))
}

// A short translation over many English cues: a cue whose share is empty
// keeps showing the previous chunk.
func TestZhFewerChunksThanCues(t *testing.T) {
	s := spoken(6, 90_000,
		"Okay, +800 so the next thing I want to show you +800 is the definition we will use for the rest of the course, "+
			"+800 and it is going to come up again and again in the exercises.", "", "好，下面是本课程通用的定义。")
	cues := SegmentCues(s)
	checkCues(t, []Sentence{s}, cues)
	if len(cues) < 3 {
		t.Fatalf("want at least 3 cues, got %+v", cues)
	}
	seen := map[string]bool{}
	for i, c := range cues {
		if c.Zh == "" {
			t.Errorf("cue %d has no Chinese", i)
		}
		if i > 0 && c.Zh != cues[i-1].Zh && seen[c.Zh] {
			t.Errorf("chunk %q shown again after another chunk", c.Zh)
		}
		seen[c.Zh] = true
	}
	if got := strings.Join(keys(cues), ""); got != s.Zh {
		t.Errorf("chunks spell %q, want %q", got, s.Zh)
	}
	golden(t, "zh_fewer_chunks.golden", render([]Sentence{s}, cues, false))
}

// keys returns the distinct consecutive Chinese chunks.
func keys(cues []Cue) []string {
	var out []string
	for i, c := range cues {
		if i == 0 || c.Zh != cues[i-1].Zh {
			out = append(out, c.Zh)
		}
	}
	return out
}

// --- Long pauses inside a sentence (lead ruling after the phase 2 acceptance) ---

// The shapes of the phase 2 acceptance (a real lecture with word
// timestamps), rebuilt with other words: a single word was shown alone
// during a 10–26 s silence ("the" 26 s, "here," 11 s, "Is" 10.5 s with
// the Chinese cut inside 大家), and "before. Exactly." was one cue across a
// 10 s silence. A pause of 1.5 s or more between two words now always ends a
// cue, which stays at most 1.5 s after its last word; the Chinese is cut
// there only at punctuation or after a function character, else both cues
// show the same chunk.
func TestLongPauses(t *testing.T) {
	// No gap before the "Is" sentence, as in the lecture.
	a := spoken(0, 896_000, "Remember the definition: what you learn afterwards is the same as before.",
		"", "请记住定义：之后你知道的东西与之前完全相同。")
	b := spoken(1, a.EndMS, "Is +10300 it obvious why this scheme is not secure?", "", "大家明白为什么这个方案不安全吗？")
	// "Exactly." (0.4 s) after a 10 s silence; the next sentence 40 ms later.
	c := spoken(2, 1_300_000, "So, it must be a value that you have not used before. +10250 Exactly.", "", "所以，它必须是你以前没有用过的值。没错。")
	d := spoken(3, c.EndMS+40, "So, you have to discard it.", "", "所以你必须丢弃它。")
	e := spoken(4, 2_667_000, "these are the bytes here, +11000 okay?", "", "就是这里的这些字节，明白吗？")
	f := spoken(5, 2_931_000, "So the matrices, the +25300 matrices, just like you repeat these rows,", "", "所以矩阵，矩阵，就像你重复这些行，")
	ss := []Sentence{a, b, c, d, e, f}
	cues := SegmentLecture(ss)
	checkCues(t, ss, cues)
	want := []struct {
		en, zh string
		end    int64 // 0: not checked
	}{
		{a.Text, a.Zh, b.StartMS}, // closed up to the next sentence (no gap)
		{"Is", b.Zh, b.Words[0].E + PauseHoldMS},
		{"it obvious why this scheme is not secure?", b.Zh, b.EndMS},
		{"So, it must be a value that you have not used before.", "所以，它必须是你以前没有用过的值。", c.Words[11].E + PauseHoldMS},
		{"Exactly.", "没错。", d.StartMS},
		{d.Text, d.Zh, d.EndMS},
		{"these are the bytes here,", "就是这里的这些字节，", e.Words[4].E + PauseHoldMS},
		{"okay?", "明白吗？", e.EndMS},
		{"So the matrices, the", "所以矩阵，", f.Words[3].E + PauseHoldMS},
		{"matrices, just like you repeat these rows,", "矩阵，就像你重复这些行，", f.EndMS},
	}
	if len(cues) != len(want) {
		t.Fatalf("%d cues, want %d: %+v", len(cues), len(want), cues)
	}
	for i, w := range want {
		c := cues[i]
		if c.En != w.en || c.Zh != w.zh || c.EndMS != w.end {
			t.Errorf("cue %d = %q / %q ending %d, want %q / %q ending %d", i, c.En, c.Zh, c.EndMS, w.en, w.zh, w.end)
		}
		if d := c.EndMS - c.StartMS; d > MaxCueMS {
			t.Errorf("cue %d lasts %d ms", i, d)
		}
	}
	golden(t, "long_pauses.golden", render(ss, cues, false))
}

// At a pause the Chinese is cut at punctuation or after a function
// character as anywhere else; without either nearby it is not cut (the two
// cues share the chunk) instead of being cut at the proportional position.
func TestZhAtPause(t *testing.T) {
	for _, c := range []struct {
		zh     string
		lens   []int
		pauses []bool
		want   []string // the chunk each cue shows
	}{
		// 大家 would be cut at the proportional position (大|家): joined.
		{"大家明白为什么这个方案不安全吗？", []int{2, 41}, []bool{false, true},
			[]string{"大家明白为什么这个方案不安全吗？", "大家明白为什么这个方案不安全吗？"}},
		// The same boundary without a pause: the proportional cut of P4.4.
		{"大家明白为什么这个方案不安全吗？", []int{2, 41}, nil,
			[]string{"大", "家明白为什么这个方案不安全吗？"}},
		// A function character within the window: cut after it.
		{"我们的方案为什么不安全？", []int{2, 38}, []bool{false, true},
			[]string{"我们的", "方案为什么不安全？"}},
		// Punctuation within ±30%: cut there.
		{"大家想想，为什么这个方案不安全？", []int{2, 41}, []bool{false, true},
			[]string{"大家想想，", "为什么这个方案不安全？"}},
		// After a cut at punctuation: the punctuation just used would leave
		// "Is" nothing of its own (it would show 首先，我问一下。again), so
		// the cues around the pause share the chunk between the cuts around them.
		{"首先，我问一下。大家明白为什么这个方案不安全吗？然后看下一个。", []int{20, 2, 41, 20}, []bool{false, false, true, false},
			[]string{"首先，我问一下。", "大家明白为什么这个方案不安全吗？", "大家明白为什么这个方案不安全吗？", "然后看下一个。"}},
		// Without the pause, P4.4 as before: the empty chunk continues the previous one.
		{"首先，我问一下。大家明白为什么这个方案不安全吗？然后看下一个。", []int{20, 2, 41, 20}, nil,
			[]string{"首先，我问一下。", "首先，我问一下。", "大家明白为什么这个方案不安全吗？", "然后看下一个。"}},
	} {
		cues := make([]Cue, len(c.lens))
		for i, l := range c.lens {
			cues[i].En = strings.Repeat("x", l)
		}
		assignZh(cues, c.pauses, c.zh)
		for i := range cues {
			if cues[i].Zh != c.want[i] {
				t.Errorf("%s %v: cue %d shows %q, want %q", c.zh, c.pauses, i, cues[i].Zh, c.want[i])
			}
		}
	}
}

// --- Between sentences (P4.5 case 5) ---

func TestLectureGaps(t *testing.T) {
	a := spoken(0, 1_000, "The first sentence ends here.", "", "第一句在这里结束。")
	b := spoken(1, a.EndMS+800, "The second one follows quickly.", "", "第二句紧随其后。")
	c := spoken(2, b.EndMS+JoinGapMS, "The third comes after exactly one and a half seconds.", "", "第三句恰好在一秒半之后。")
	d := spoken(3, c.EndMS+3_000, "The fourth starts after a long pause.", "", "第四句在长停顿之后开始。")
	e := spoken(4, d.EndMS-200, "The fifth overlaps the fourth a little.", "", "第五句与第四句略有重叠。")
	ss := []Sentence{a, b, c, d, e}
	cues := SegmentLecture(ss)
	checkCues(t, ss, cues)
	if len(cues) != 5 {
		t.Fatalf("want one cue per sentence, got %+v", cues)
	}
	for i, want := range []int64{b.StartMS, b.EndMS, c.EndMS, e.StartMS, e.EndMS} {
		if cues[i].EndMS != want {
			t.Errorf("cue %d ends at %d, want %d", i, cues[i].EndMS, want)
		}
	}
	for i, s := range ss {
		if cues[i].StartMS != s.StartMS {
			t.Errorf("cue %d starts at %d, want %d", i, cues[i].StartMS, s.StartMS)
		}
	}
	golden(t, "lecture_gaps.golden", render(ss, cues, false))
}

// --- Degenerate input ---

func TestDegenerate(t *testing.T) {
	ss := []Sentence{
		// No duration at all (corrupt timestamps): one cue of MinCueMS.
		{Idx: 0, StartMS: 5_000, EndMS: 5_000, Text: "Zero length.", ASRText: "Zero length.", Zh: "零长度。"},
		// All words at the same instant except the last: no zero-length cue.
		{Idx: 1, StartMS: 7_000, EndMS: 9_000,
			Text:    "every word here was stamped at the same instant by a broken decoder and only the last one has a duration",
			ASRText: "every word here was stamped at the same instant by a broken decoder and only the last one has a duration"},
		// No English words: a Chinese-only cue.
		{Idx: 2, StartMS: 10_000, EndMS: 12_000, Text: "  ", ASRText: "", Zh: "只有中文。"},
		// Starts before the previous cue: delayed, keeping its duration.
		{Idx: 3, StartMS: 9_500, EndMS: 11_000, Text: "Out of order.", ASRText: "Out of order.", Zh: "顺序错乱。"},
		// End before start without words.
		{Idx: 4, StartMS: 20_000, EndMS: 19_000, Text: "Inverted times.", ASRText: "Inverted times."},
	}
	var ws []Word
	for _, f := range strings.Fields(ss[1].Text) {
		ws = append(ws, Word{W: " " + f, S: 7_000, E: 7_000})
	}
	ws[len(ws)-1].E = 9_000
	ss[1].Words = ws
	cues := SegmentLecture(ss)
	checkCues(t, ss, cues)
	if c := SegmentCues(ss[0]); len(c) != 1 || c[0].StartMS != 5_000 || c[0].EndMS != 5_000+MinCueMS {
		t.Errorf("zero-length sentence alone: %+v", c)
	}
	if c := cues[0]; c.StartMS != 5_000 || c.EndMS != 7_000 { // then closed up to the next sentence (gap < 1.5 s)
		t.Errorf("zero-length sentence: %+v", c)
	}
	if c := cues[1]; c.StartMS != 7_000 || c.EndMS != 10_000 || c.En != ss[1].Text {
		t.Errorf("same-instant words: %+v", c)
	}
	if c := cues[2]; c.En != "" || c.Zh != "只有中文。" {
		t.Errorf("Chinese-only cue: %+v", c)
	}
	if c := cues[3]; c.StartMS != 12_000 || c.EndMS != 13_500 {
		t.Errorf("out-of-order sentence: %+v", c)
	}
	if c := cues[4]; c.StartMS != 20_000 || c.EndMS != 20_000+MinCueMS {
		t.Errorf("inverted sentence: %+v", c)
	}
	if got := SegmentCues(Sentence{Text: " ", ASRText: " "}); got != nil {
		t.Errorf("empty sentence: %+v", got)
	}
	if got := SegmentLecture(nil); got != nil {
		t.Errorf("empty lecture: %+v", got)
	}
	golden(t, "degenerate.golden", render(ss, cues, true))
}

// Words that do not spell asr_text (inconsistent data) still align word by
// word instead of failing.
func TestWordsNotSpellingASRText(t *testing.T) {
	s := Sentence{StartMS: 0, EndMS: 3000, Text: "alpha beta gamma delta", ASRText: "alpha beta gamma delta",
		Words: []Word{{" alpha", 0, 500}, {" BETA", 600, 1000}, {" epsilon", 1100, 2000}, {" delta", 2100, 3000}}}
	wt := WordTimes(s)
	want := []Word{{"alpha", 0, 500}, {"beta", 600, 1000}, {"gamma", 1000, 2100}, {"delta", 2100, 3000}}
	for i := range want {
		if wt[i] != want[i] {
			t.Errorf("word %d = %+v, want %+v", i, wt[i], want[i])
		}
	}
}

func TestJSONFields(t *testing.T) {
	b, err := json.Marshal(Cue{StartMS: 1, EndMS: 2, En: "a", Zh: "b", SentenceIdx: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"start_ms":1,"end_ms":2,"en":"a","zh":"b","sentence_idx":3}` {
		t.Errorf("cue JSON = %s", got)
	}
	var ws []Word
	if err := json.Unmarshal([]byte(`[{"w": "cipher", "s": 754120, "e": 754480}]`), &ws); err != nil {
		t.Fatal(err)
	}
	if ws[0] != (Word{W: "cipher", S: 754120, E: 754480}) {
		t.Errorf("word = %+v", ws[0])
	}
}
