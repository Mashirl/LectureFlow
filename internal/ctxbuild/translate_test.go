// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

const testX = 3

var targetRE = regexp.MustCompile(`\[TARGET #\d+\]`)

// TestTranslateGolden pins the exact bytes of the translate requests
// (SPEC §10 step 7: golden test first).
func TestTranslateGolden(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	golden(t, "translate_system.golden", TranslateSystem(lec))
	var hashes strings.Builder
	for _, idx := range []int{0, 1, 2, 5, n - 1} {
		r := BuildTranslateRequest(lec, idx, testX)
		golden(t, fmt.Sprintf("translate_user_%02d.golden", idx), r.User)
	}
	for i := range n {
		fmt.Fprintf(&hashes, "%d %s\n", i, BuildTranslateRequest(lec, i, testX).Hash)
	}
	golden(t, "translate_hashes.golden", hashes.String())
}

// TestTranslateFirstSentenceHasNoBefore: an empty [BEFORE] holds (none)
// (SPEC-v2 G5.3; v1 omitted the section).
func TestTranslateFirstSentenceHasNoBefore(t *testing.T) {
	lec := sampleSnapshot(t)
	r := BuildTranslateRequest(lec, 0, testX)
	if !strings.HasPrefix(r.User, "[BEFORE]\n(none)\n[TARGET #0]\n"+lec.Sentences[0].Text+"\n") {
		t.Errorf("idx 0 must start with an empty [BEFORE] and the target:\n%s", r.User)
	}
	want := "[AFTER]\n" + lec.Sentences[1].Text + "\n" + lec.Sentences[2].Text + "\n" + lec.Sentences[3].Text
	if !strings.HasSuffix(r.User, want) {
		t.Errorf("idx 0 [AFTER] must be sentences 1..3:\n%s", r.User)
	}
	if r.Idx != 0 {
		t.Errorf("Idx = %d", r.Idx)
	}
}

func TestTranslateLastSentenceHasNoAfter(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	r := BuildTranslateRequest(lec, n-1, testX)
	want := fmt.Sprintf("[BEFORE]\n%s\n%s\n%s\n[TARGET #%d]\n%s\n[AFTER]\n(none)", lec.Sentences[n-4].Text, lec.Sentences[n-3].Text,
		lec.Sentences[n-2].Text, n-1, lec.Sentences[n-1].Text)
	if r.User != want {
		t.Errorf("idx n-1 user:\n%s\nwant:\n%s", r.User, want)
	}
}

func TestTranslateASRLine(t *testing.T) {
	lec := sampleSnapshot(t)
	// idx 5: text == asr_text → no [ASR]
	if r := BuildTranslateRequest(lec, 5, testX); strings.Contains(r.User, "[ASR]") {
		t.Errorf("asr == text must omit [ASR]:\n%s", r.User)
	}
	// idx 2: repaired → [ASR] right after the target: the English note of
	// SPEC §7.1 ("may contain errors, for reference only"; lead ruling, Gate
	// round 1), then the asr_text
	r := BuildTranslateRequest(lec, 2, testX)
	want := "[TARGET #2]\n" + lec.Sentences[2].Text + "\n[ASR]\n" + ASRNote + "\n" + lec.Sentences[2].ASRText + "\n[AFTER]\n"
	if ASRNote != "(Raw speech recognition output; may contain errors. For reference only.)" {
		t.Errorf("ASRNote = %q", ASRNote)
	}
	if !strings.Contains(r.User, want) {
		t.Errorf("idx 2 must carry the asr line:\n%s", r.User)
	}
	// sections in §7.2 order (SPEC-v2 G5.3 labels)
	order := []string{"[BEFORE]", "[TARGET #2]", "[ASR]", "[AFTER]"}
	last := -1
	for _, h := range order {
		i := strings.Index(r.User, h)
		if i <= last {
			t.Errorf("section %s out of order", h)
		}
		last = i
	}
}

func TestTranslatePrefixIdenticalAcrossSentences(t *testing.T) {
	lec := sampleSnapshot(t)
	first := BuildTranslateRequest(lec, 0, testX).System
	for i := range lec.Sentences {
		if s := BuildTranslateRequest(lec, i, testX).System; s != first {
			t.Fatalf("prefix of idx %d differs from idx 0", i)
		}
	}
	// the prefix only lists enabled terms, en_key order, zh-less term as bare
	// en; every term of the sample is enforce (SPEC-v2 G5.1)
	want := "GLOSSARY:\n" + MandatoryTermsHeader + "\ncache => 缓存\ncache line => 缓存行\nLRU\nset associative => 组相联\nwrite-back => 写回\n\n" +
		PreferredTermsHeader + "\n(none)\n\nSUMMARY:\n" + lec.Summary
	if !strings.HasSuffix(first, want) {
		t.Errorf("prefix tail:\n%s", first[strings.Index(first, "GLOSSARY:"):])
	}
	if strings.Contains(first, "TLB") {
		t.Error("disabled term in prefix")
	}
}

func TestTranslateDeterministic(t *testing.T) {
	lec := sampleSnapshot(t)
	other := clone(lec)
	// a different load order of terms and materials must not matter
	slices.Reverse(other.Terms)
	slices.Reverse(other.Materials)
	for i := range lec.Sentences {
		a := BuildTranslateRequest(lec, i, testX)
		b := BuildTranslateRequest(other, i, testX)
		c := BuildTranslateRequest(lec, i, testX)
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c) {
			t.Fatalf("idx %d: two builds differ", i)
		}
	}
}

func TestTranslateHasNoChinese(t *testing.T) {
	lec := sampleSnapshot(t)
	for i := range lec.Sentences {
		u := BuildTranslateRequest(lec, i, testX).User
		for _, h := range []string{"[BEFORE]", "[ASR]", "[AFTER]"} {
			u = strings.ReplaceAll(u, h, "")
		}
		u = targetRE.ReplaceAllString(u, "")
		for _, r := range u {
			if unicode.Is(unicode.Han, r) {
				t.Fatalf("idx %d: Chinese text in user message: %q", i, u)
			}
		}
	}
}

func TestTranslateWindowZero(t *testing.T) {
	lec := sampleSnapshot(t)
	r := BuildTranslateRequest(lec, 6, 0)
	if r.User != "[BEFORE]\n(none)\n[TARGET #6]\n"+lec.Sentences[6].Text+"\n[AFTER]\n(none)" {
		t.Errorf("x=0 user: %q", r.User)
	}
	if BuildTranslateRequest(lec, 6, -2).User != r.User {
		t.Error("negative x must behave like 0")
	}
}

// TestTranslateHashInvalidationRange: changing the text of sentence i
// changes exactly the hashes of [i-x, i+x] (SPEC §7.3, §10 step 7).
func TestTranslateHashInvalidationRange(t *testing.T) {
	base := sampleSnapshot(t)
	n := len(base.Sentences)
	for _, x := range []int{0, 1, 3, 20} {
		before := TranslateHashes(base, x)
		for i := range n {
			if before[i] != TranslateHash(base, i, x) || before[i] != BuildTranslateRequest(base, i, x).Hash {
				t.Fatalf("x=%d idx %d: TranslateHashes, TranslateHash and request hash disagree", x, i)
			}
		}
		for i := range n {
			lec := clone(base)
			lec.Sentences[i].Text += " (edited)"
			after := TranslateHashes(lec, x)
			for j := range n {
				changed := after[j] != before[j]
				want := j >= i-x && j <= i+x
				if changed != want {
					t.Errorf("x=%d edit %d: hash of %d changed=%v, want %v", x, i, j, changed, want)
				}
			}
		}
	}
}

func TestTranslateHashPrefixChange(t *testing.T) {
	base := sampleSnapshot(t)
	before := TranslateHashes(base, testX)
	mods := map[string]func(*LectureSnapshot){
		"term zh":       func(l *LectureSnapshot) { l.Terms[0].Zh = "回写" },
		"term disabled": func(l *LectureSnapshot) { l.Terms[1].Enabled = false },
		"summary":       func(l *LectureSnapshot) { l.Summary += " More." },
	}
	for name, mod := range mods {
		lec := clone(base)
		mod(&lec)
		after := TranslateHashes(lec, testX)
		for i := range after {
			if after[i] == before[i] {
				t.Errorf("%s: hash of %d unchanged", name, i)
			}
		}
	}
	// materials are not part of the translate prefix
	lec := clone(base)
	lec.Materials = nil
	if !slices.Equal(TranslateHashes(lec, testX), before) {
		t.Error("materials changed translate hashes")
	}
	// x is part of the hash even where the window is clipped
	if TranslateHash(base, 0, 3) == TranslateHash(base, 0, 4) {
		t.Error("x not part of the hash")
	}
}

func TestTranslateStageHash(t *testing.T) {
	base := sampleSnapshot(t)
	h := TranslateStageHash(base, testX)
	lec := clone(base)
	lec.Sentences[3].Text += " (edited)"
	if TranslateStageHash(lec, testX) != h {
		t.Error("sentence edits must not change the stage hash")
	}
	lec.Summary += " More."
	if TranslateStageHash(lec, testX) == h {
		t.Error("summary change must change the stage hash")
	}
	lec = clone(base)
	lec.Terms[0].Zh = "回写"
	if TranslateStageHash(lec, testX) == h {
		t.Error("glossary change must change the stage hash")
	}
	if TranslateStageHash(base, testX+1) == h {
		t.Error("x must be part of the stage hash")
	}
}

func TestTranslateRetryUser(t *testing.T) {
	got := TranslateRetryUser("[TARGET #1]\nHello.", `the number 64 from the source sentence is missing`)
	want := "[TARGET #1]\nHello.\n\nYour previous translation was rejected: the number 64 from the source sentence is missing. Translate again."
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestTranslateOutOfRangePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	BuildTranslateRequest(sampleSnapshot(t), 14, 3)
}
