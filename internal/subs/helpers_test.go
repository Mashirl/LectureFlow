// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package subs

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// golden compares got with testdata/{name}, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/subs -update to create it)", err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs from golden file:\n%s", name, diffHint(got, w))
	}
}

func diffHint(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + strconv.Itoa(i+1) + ":\n got: " + gl + "\nwant: " + wl
		}
	}
	return "(no line difference)"
}

// speak times the words of a script like a speaker would: each word lasts
// 100 ms plus 60 ms per character and is followed by 50 ms of silence; a
// token "+N" inserts an extra pause of N ms. It returns whisper-style words
// (with a leading space) and the text they spell.
func speak(start int64, script string) ([]Word, string) {
	var words []Word
	var text []string
	t := start
	for _, f := range strings.Fields(script) {
		if strings.HasPrefix(f, "+") {
			ms, err := strconv.Atoi(f[1:])
			if err != nil {
				panic(err)
			}
			t += int64(ms)
			continue
		}
		d := 100 + 60*int64(utf8.RuneCountInString(f))
		words = append(words, Word{W: " " + f, S: t, E: t + d})
		text = append(text, f)
		t += d + 50
	}
	return words, strings.Join(text, " ")
}

// spoken builds a sentence whose asr_text is the script; text is the
// effective text ("" = same as asr_text).
func spoken(idx int, start int64, script, text, zh string) Sentence {
	words, asr := speak(start, script)
	if text == "" {
		text = asr
	}
	return Sentence{Idx: idx, StartMS: words[0].S, EndMS: words[len(words)-1].E,
		Text: text, ASRText: asr, Words: words, Zh: zh}
}

func clock(ms int64) string {
	neg := ""
	if ms < 0 {
		neg, ms = "-", -ms
	}
	return fmt.Sprintf("%s%02d:%02d.%03d", neg, ms/60000, ms/1000%60, ms%1000)
}

// render prints sentences and their cues for a golden file.
func render(ss []Sentence, cues []Cue, withWordTimes bool) string {
	var b strings.Builder
	b.WriteString("== input\n")
	for _, s := range ss {
		fmt.Fprintf(&b, "sentence #%d  %s → %s\n", s.Idx, clock(s.StartMS), clock(s.EndMS))
		fmt.Fprintf(&b, "  text:  %s\n", s.Text)
		if s.ASRText == s.Text {
			b.WriteString("  asr:   (same as text)\n")
		} else {
			fmt.Fprintf(&b, "  asr:   %s\n", s.ASRText)
		}
		if len(s.Words) == 0 {
			b.WriteString("  words: none\n")
		} else {
			parts := make([]string, len(s.Words))
			for i, w := range s.Words {
				parts[i] = fmt.Sprintf("%s[%d-%d]", strings.TrimSpace(w.W), w.S, w.E)
			}
			fmt.Fprintf(&b, "  words: %s\n", strings.Join(parts, " "))
		}
		if s.Zh == "" {
			b.WriteString("  zh:    (not translated)\n")
		} else {
			fmt.Fprintf(&b, "  zh:    %s\n", s.Zh)
		}
		if withWordTimes {
			wt := WordTimes(s)
			parts := make([]string, len(wt))
			for i, w := range wt {
				parts[i] = fmt.Sprintf("%s[%d-%d]", w.W, w.S, w.E)
			}
			fmt.Fprintf(&b, "  times: %s\n", strings.Join(parts, " "))
		}
	}
	b.WriteString("== cues\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "#%d  %s → %s  (%d ms, %d ch)\n", c.SentenceIdx, clock(c.StartMS), clock(c.EndMS),
			c.EndMS-c.StartMS, utf8.RuneCountInString(c.En))
		fmt.Fprintf(&b, "  en: %s\n", c.En)
		fmt.Fprintf(&b, "  zh: %s\n", c.Zh)
	}
	return b.String()
}

// checkCues verifies the invariants every result must hold: English covers
// text word for word, times are monotone, cues do not overlap, start < end.
func checkCues(t *testing.T, ss []Sentence, cues []Cue) {
	t.Helper()
	k := 0
	for _, s := range ss {
		var en []string
		for k < len(cues) && cues[k].SentenceIdx == s.Idx {
			if cues[k].En != "" {
				en = append(en, cues[k].En)
			}
			k++
		}
		if got, want := strings.Join(en, " "), strings.Join(strings.Fields(s.Text), " "); got != want {
			t.Fatalf("sentence %d: cues spell %q, text is %q", s.Idx, got, want)
		}
	}
	if k != len(cues) {
		t.Fatalf("%d cues not attributed to a sentence in order", len(cues)-k)
	}
	for i, c := range cues {
		if c.EndMS <= c.StartMS {
			t.Fatalf("cue %d: start %d, end %d", i, c.StartMS, c.EndMS)
		}
		if i > 0 && c.StartMS < cues[i-1].EndMS {
			t.Fatalf("cue %d starts at %d before cue %d ends at %d", i, c.StartMS, i-1, cues[i-1].EndMS)
		}
	}
}
