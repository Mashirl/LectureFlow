// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

func TestRepairGolden(t *testing.T) {
	lec := sampleSnapshot(t)
	chunks := BuildRepairRequest(lec)
	if len(chunks) != 1 {
		t.Fatalf("14 sentences: %d chunks", len(chunks))
	}
	golden(t, "repair_system.golden", chunks[0].System)
	golden(t, "repair_user_00.golden", chunks[0].User)
}

func TestRepairSingleChunkContextNone(t *testing.T) {
	lec := sampleSnapshot(t)
	c := BuildRepairRequest(lec)[0]
	if c.Start != 0 || c.End != 14 {
		t.Errorf("range [%d,%d)", c.Start, c.End)
	}
	if !strings.HasPrefix(c.User, "CONTEXT (before):\n(none)\n\nTARGET:\n[0] ") {
		t.Errorf("user start: %q", c.User[:60])
	}
	if !strings.HasSuffix(c.User, "\n\nCONTEXT (after):\n(none)") {
		t.Errorf("user end: %q", c.User[len(c.User)-60:])
	}
	// lines carry asr_text (not the repaired text) with ⟦⟧ marks
	for _, want := range []string{
		"[1] Today we talk about the ⟦cash⟧ hierarchy in modern processors.",
		"[2] A ⟦right⟧ ⟦back⟧ cache only writes a line to memory on eviction.",
		"[4] So the the first question is how we find a line in the cache.",
		"[9] A ⟦two-way⟧ set associative cache halves the number of sets.",
		"[11] ⟦Blah⟧ ⟦lab⟧ is something we will see in the lab next week.",
		"[12] Finally, ⟦LRU⟧ replacement evicts the least recently used line.",
	} {
		if !strings.Contains(c.User, "\n"+want+"\n") {
			t.Errorf("missing line %q", want)
		}
	}
	if strings.Contains(c.User, "the cache hierarchy") {
		t.Error("repair input must use asr_text, not text")
	}
	// prefix: glossary, summary and slides of this lecture (course-wide + own, upload order)
	for _, want := range []string{
		"GLOSSARY:\n" + MandatoryTermsHeader + "\ncache => 缓存\ncache line => 缓存行\nLRU\nset associative => 组相联\nwrite-back => 写回\n\n" +
			PreferredTermsHeader + "\n(none)\n\nSUMMARY:\n" + lec.Summary + "\n\nSLIDES:\n",
		"SLIDES:\n--- page 1 ---\nCaches\nwrite-back vs write-through\n--- page 2 ---\nSet associative caches\nLRU\n--- page 1 ---\nCourse overview\nMemory hierarchy\n--- page 2 ---\n",
	} {
		if !strings.Contains(c.System, want) {
			t.Errorf("system lacks %q", want)
		}
	}
	if strings.Contains(c.System, "other lecture") {
		t.Error("material of another lecture in slides")
	}
}

func synthLecture(n int) LectureSnapshot {
	ss := make([]Sentence, n)
	for i := range ss {
		ss[i] = Sentence{Idx: i, Text: fmt.Sprintf("Sentence %d.", i), ASRText: fmt.Sprintf("Sentence %d.", i)}
	}
	return LectureSnapshot{LectureID: "L", Sentences: ss}
}

func linesOf(t *testing.T, user, section string) []string {
	t.Helper()
	i := strings.Index(user, section+"\n")
	if i < 0 {
		t.Fatalf("no section %q", section)
	}
	body := user[i+len(section)+1:]
	if j := strings.Index(body, "\n\n"); j >= 0 {
		body = body[:j]
	}
	return strings.Split(body, "\n")
}

func idxLines(from, to int) []string {
	if from >= to {
		return []string{None}
	}
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("[%d] Sentence %d.", i, i))
	}
	return out
}

func TestRepairChunking(t *testing.T) {
	for _, n := range []int{1, 39, 40, 41, 80, 90, 123} {
		lec := synthLecture(n)
		chunks := BuildRepairRequest(lec)
		owner := make([]int, n)
		for k, c := range chunks {
			if c.Start != k*RepairChunkSize || c.End != min(n, (k+1)*RepairChunkSize) {
				t.Errorf("n=%d chunk %d: range [%d,%d)", n, k, c.Start, c.End)
			}
			if c.System != chunks[0].System {
				t.Errorf("n=%d chunk %d: prefix differs", n, k)
			}
			for i := c.Start; i < c.End; i++ {
				owner[i]++
				if !c.Contains(i) {
					t.Errorf("Contains(%d) false", i)
				}
			}
			if c.Contains(c.Start-1) || c.Contains(c.End) {
				t.Errorf("Contains outside range true")
			}
			got := map[string][]string{
				"CONTEXT (before):": linesOf(t, c.User, "CONTEXT (before):"),
				"TARGET:":           linesOf(t, c.User, "TARGET:"),
				"CONTEXT (after):":  linesOf(t, c.User, "CONTEXT (after):"),
			}
			want := map[string][]string{
				"CONTEXT (before):": idxLines(max(0, c.Start-RepairOverlap), c.Start),
				"TARGET:":           idxLines(c.Start, c.End),
				"CONTEXT (after):":  idxLines(c.End, min(n, c.End+RepairOverlap)),
			}
			for sec := range want {
				if strings.Join(got[sec], "\n") != strings.Join(want[sec], "\n") {
					t.Errorf("n=%d chunk %d %s:\n%v\nwant\n%v", n, k, sec, got[sec], want[sec])
				}
			}
		}
		for i, o := range owner {
			if o != 1 {
				t.Errorf("n=%d: sentence %d is target of %d chunks", n, i, o)
			}
		}
		if want := (n + RepairChunkSize - 1) / RepairChunkSize; len(chunks) != want {
			t.Errorf("n=%d: %d chunks, want %d", n, len(chunks), want)
		}
	}
	if BuildRepairRequest(synthLecture(0)) != nil {
		t.Error("no sentences must give no chunks")
	}
}

func TestRepairEmptySummaryAndSlides(t *testing.T) {
	c := BuildRepairRequest(synthLecture(3))[0]
	if !strings.HasSuffix(c.System, "GLOSSARY:\n"+MandatoryTermsHeader+"\n(none)\n\n"+PreferredTermsHeader+"\n(none)\n\nSUMMARY:\n(none)\n\nSLIDES:\n(none)") {
		t.Errorf("empty prefix values: %q", c.System[len(c.System)-80:])
	}
}

func TestMarkLowConf(t *testing.T) {
	lw := func(pairs ...any) []model.LowConfWord {
		var out []model.LowConfWord
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, model.LowConfWord{I: pairs[i].(int), W: pairs[i+1].(string)})
		}
		return out
	}
	cases := []struct {
		name, text string
		words      []model.LowConfWord
		want       string
	}{
		{"none", "No marks here.", nil, "No marks here."},
		{"by index", "Today we talk about the cash hierarchy.", lw(5, "cash"), "Today we talk about the ⟦cash⟧ hierarchy."},
		{"punctuation kept outside", "We pay in cash, mostly.", lw(3, "cash"), "We pay in ⟦cash⟧, mostly."},
		{"word with punctuation", "We pay in cash, mostly.", lw(3, "cash,"), "We pay in ⟦cash⟧, mostly."},
		{"two adjacent", "A right back cache.", lw(1, "right", 2, "back"), "A ⟦right⟧ ⟦back⟧ cache."},
		{"first word", "Blah lab is next.", lw(0, "Blah", 1, "lab"), "⟦Blah⟧ ⟦lab⟧ is next."},
		// whisper words " x" "86" make i one larger than the token index
		{"shifted index falls back to nearest equal token", "On x86 we use LRU now.", lw(5, "LRU"), "On x86 we use ⟦LRU⟧ now."},
		{"nearest of several", "lab one lab two lab", lw(5, "lab"), "lab one lab two ⟦lab⟧"},
		{"tie picks the earlier token", "lab one lab two lab", lw(3, "lab"), "lab one ⟦lab⟧ two lab"},
		{"no substring fallback for short words", "The cache.", lw(5, "a"), "The cache."},
		// Gate B: whisper words " I","'m"," a"," cash" → i counts sub-words, the token at i=2 is "cash"
		{"index token must equal the word", "I'm a cash", lw(2, "a"), "I'm ⟦a⟧ cash"},
		{"word inside the index token is not marked", "the cash register", lw(1, "as"), "the cash register"},
		{"case-insensitive equality", "Blah lab is next.", lw(0, "blah"), "⟦Blah⟧ lab is next."},
		{"case-insensitive fallback", "We use lru here.", lw(9, "LRU"), "We use ⟦lru⟧ here."},
		{"punctuation on both sides", "(cache) and more", lw(0, "cache"), "(⟦cache⟧) and more"},
		{"punctuation-only token never marked", "a -- b", lw(1, "--"), "a -- b"},
		{"unknown word ignored", "Hello world.", lw(1, "planet"), "Hello world."},
		{"index out of range ignored", "Hello world.", lw(9, "moon"), "Hello world."},
		{"duplicate marks once", "Hello world.", lw(1, "world", 1, "world"), "Hello ⟦world⟧."},
		{"newline flattened", "Hello\nworld.", lw(1, "world"), "Hello ⟦world⟧."},
	}
	for _, c := range cases {
		if got := MarkLowConf(c.text, c.words); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestStripMarks(t *testing.T) {
	if got := StripMarks("A ⟦right⟧ ⟦back⟧ cache"); got != "A right back cache" {
		t.Errorf("got %q", got)
	}
}
