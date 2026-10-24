// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// asrFixture is the G4 fixture: a cryptography lecture whose bound slides
// mention some of the course terms.
func asrFixture() ([]model.Term, string) {
	text := "Classical ciphers\nThe Vigenère cipher and the Kasiski examination.\nKasiski found the key length.\n" +
		"Feistel networks; a Feistel round.\nblock cipher\nCaesar cipher\nkey key\n"
	term := func(en, source string, freq int, tier string, common, enabled bool) model.Term {
		return model.Term{ID: "t-" + EnKey(en), En: en, EnKey: EnKey(en), Source: source, Enabled: enabled,
			Freq: freq, Tier: tier, CommonWord: common}
	}
	terms := []model.Term{
		term("key", model.SourceMaterial, 50, model.TierEnforce, true, true),             // common word: last group
		term("public", model.SourceMaterial, 41, model.TierReference, true, true),        // reference: never prompted
		term("block", model.SourceSyllabus, 12, model.TierReference, false, true),        // reference: never prompted
		term("world", model.SourceMaterial, 9, model.TierEnforce, true, false),           // disabled
		term("Vigenère cipher", model.SourceMaterial, 1, model.TierEnforce, false, true), // 1 in this lecture
		term("Kasiski", model.SourceMaterial, 2, "", false, true),                        // tier "" = enforce; 2 in this lecture
		term("Feistel", model.SourceSyllabus, 10, model.TierEnforce, false, true),        // 2 in this lecture, course freq 10
		term("AES", model.SourceManual, 0, model.TierEnforce, false, true),               // manual
		term("Caesar cipher", model.SourceReview, 3, model.TierEnforce, false, true),     // review, 1 in this lecture
		term("DES", model.SourceMaterial, 7, model.TierEnforce, false, true),
		term("ECB", model.SourceMaterial, 7, model.TierEnforce, false, true),
		term("stream cipher", model.SourceMaterial, 7, model.TierEnforce, false, true), // ties DES/ECB: multi-word first
		term("block cipher", model.SourceMaterial, 4, model.TierEnforce, false, true),  // 1 in this lecture
		term("state", model.SourceSyllabus, 30, model.TierEnforce, true, true),         // common word
		term("padding", model.SourceManual, 5, model.TierEnforce, true, true),          // manual but common: common group
	}
	return terms, text
}

// TestBuildASRPromptG4 pins the whisper prompt order of SPEC-v2 G4 (lead
// ruling, Gate round 1): only enabled enforce terms; common_word = 0 first;
// then manual/review; then the count in this lecture's bound slides (desc);
// then terms found only in this lecture's slides; then the course freq
// (desc); then multi-word terms; then en_key.
func TestBuildASRPromptG4(t *testing.T) {
	terms, text := asrFixture()
	want := "Caesar cipher, AES, " + // not common, manual/review: lecture count 1 before 0
		"Kasiski, Feistel, " + // lecture count 2: only in this lecture (2 = freq 2) before freq 10
		"Vigenère cipher, block cipher, " + // lecture count 1: only in this lecture (1 = freq 1) before freq 4
		"stream cipher, DES, ECB, " + // lecture count 0, freq 7: multi-word first, then en_key
		"padding, key, state" // common words last: manual, then lecture count, then freq
	got := BuildASRPrompt(terms, text)
	if got != want {
		t.Errorf("prompt\n got %q\nwant %q", got, want)
	}
	golden(t, "asr_prompt.golden", got+"\n")
	// the input order of the terms does not matter
	rev := slices.Clone(terms)
	slices.Reverse(rev)
	if BuildASRPrompt(rev, text) != got {
		t.Error("prompt depends on the term order")
	}
	for _, en := range strings.Split(got, ", ") {
		if en == "public" || en == "world" || en == "block" {
			t.Errorf("%q must not be prompted (reference or disabled)", en)
		}
	}
	// without slides every lecture count is 0: the course freq decides,
	// multi-word terms only break freq ties
	noSlides := "Caesar cipher, AES, Feistel, stream cipher, DES, ECB, block cipher, Kasiski, Vigenère cipher, padding, key, state"
	if got := BuildASRPrompt(terms, ""); got != noSlides {
		t.Errorf("no slides:\n got %q\nwant %q", got, noSlides)
	}
}

// TestBuildASRPromptLectureOnlyBeforeFreq pins the ruled order (Gate round
// 1): at equal lecture count, a term found only in this lecture's slides
// goes before a more frequent course term; a multi-word term does not jump
// ahead of a more frequent one (it only wins a freq tie), so rare
// multi-word terms never push single-word proper nouns out of the prompt.
func TestBuildASRPromptLectureOnlyBeforeFreq(t *testing.T) {
	terms := []model.Term{
		{En: "Feistel", EnKey: "feistel", Source: model.SourceMaterial, Enabled: true, Freq: 10},
		{En: "Kasiski", EnKey: "kasiski", Source: model.SourceMaterial, Enabled: true, Freq: 2},
		{En: "DES", EnKey: "des", Source: model.SourceMaterial, Enabled: true, Freq: 7},
		{En: "one-time pad", EnKey: "one-time pad", Source: model.SourceMaterial, Enabled: true, Freq: 1},
		{En: "stream cipher", EnKey: "stream cipher", Source: model.SourceMaterial, Enabled: true, Freq: 7},
	}
	text := "Kasiski and Feistel. Kasiski again, then Feistel."
	// lecture count 2: Kasiski (2 = freq, only here) before Feistel (freq 10);
	// lecture count 0: freq 7 before the multi-word "one-time pad" (freq 1),
	// and at freq 7 the multi-word "stream cipher" before DES
	if got, want := BuildASRPrompt(terms, text), "Kasiski, Feistel, stream cipher, DES, one-time pad"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// a term with the same lecture count that also occurs elsewhere is not preferred
	terms[1].Freq = 5
	if got, want := BuildASRPrompt(terms, text), "Feistel, Kasiski, stream cipher, DES, one-time pad"; got != want {
		t.Errorf("Kasiski also in other lectures: got %q want %q", got, want)
	}
}

// TestBuildASRPromptLectureCount: a term the lecture's slides use often
// moves ahead of a more frequent course term.
func TestBuildASRPromptLectureCount(t *testing.T) {
	terms := []model.Term{
		{En: "TLB", EnKey: "tlb", Source: model.SourceMaterial, Enabled: true, Freq: 40},
		{En: "Belady", EnKey: "belady", Source: model.SourceMaterial, Enabled: true, Freq: 3},
	}
	if got := BuildASRPrompt(terms, ""); got != "TLB, Belady" {
		t.Errorf("no slides: %q", got)
	}
	if got := BuildASRPrompt(terms, "Belady's anomaly. Belady again; TLB once."); got != "Belady, TLB" {
		t.Errorf("with slides: %q", got)
	}
}

func TestBoundMaterialText(t *testing.T) {
	ms := []model.Material{
		{ID: "m2", LectureID: strp("L1"), CreatedAt: "2026-01-02T00:00:00Z", PagesMD: []string{"Second", ""}},
		{ID: "m0", LectureID: nil, CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"Course-wide"}},
		{ID: "m3", LectureID: strp("L2"), CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"Other lecture"}},
		{ID: "m1", LectureID: strp("L1"), CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"First", "page two"}},
		{ID: "m4", LectureID: strp("L1"), CreatedAt: "2026-01-03T00:00:00Z"}, // not extracted yet
	}
	if got, want := BoundMaterialText(ms, "L1"), "First\npage two\nSecond\n\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := BoundMaterialText(ms, "L9"); got != "" {
		t.Errorf("no bound material: %q", got)
	}
}

// TestGlossaryLinesTwoSections pins {glossary_lines} of SPEC-v2 G5.1.
func TestGlossaryLinesTwoSections(t *testing.T) {
	block := codeBlocks(specFileSection(t, "v2.md", "### G5.1 "))
	if len(block) != 1 {
		t.Fatalf("G5.1: %d code blocks", len(block))
	}
	lines := strings.Split(block[0], "\n")
	if lines[0] != MandatoryTermsHeader {
		t.Errorf("MandatoryTermsHeader = %q, SPEC-v2 G5.1 has %q", MandatoryTermsHeader, lines[0])
	}
	if !slices.Contains(lines, PreferredTermsHeader) {
		t.Errorf("PreferredTermsHeader %q is not a line of SPEC-v2 G5.1", PreferredTermsHeader)
	}

	none := MandatoryTermsHeader + "\n(none)\n\n" + PreferredTermsHeader + "\n(none)"
	if got := GlossaryLines(nil); got != none {
		t.Errorf("no terms: %q", got)
	}
	if got := GlossaryLines([]model.Term{{En: "TLB", Zh: "快表", Enabled: false}}); got != none {
		t.Errorf("only disabled: %q", got)
	}
	terms := []model.Term{
		{En: "Write-back", EnKey: "write-back", Zh: "写回", Enabled: true},
		{En: "public", EnKey: "public", Zh: "公开 / 公钥 (public key)", Enabled: true, Tier: model.TierReference},
		{En: "LRU", Zh: "  ", Enabled: true}, // EnKey computed when missing; blank zh = none
		{En: "key", EnKey: "key", Zh: "密钥", Enabled: true, Tier: model.TierReference},
		{En: "cache", EnKey: "cache", Zh: "缓存", Enabled: true, Tier: model.TierEnforce},
		{En: "block", EnKey: "block", Zh: "分组", Enabled: false, Tier: model.TierReference},
		{En: "Cache", EnKey: "cache", Zh: "高速缓存", Enabled: true}, // duplicate key: first wins
		{En: "Python", EnKey: "python", Enabled: true, Tier: model.TierReference},
	}
	want := MandatoryTermsHeader + "\ncache => 缓存\nLRU\nWrite-back => 写回\n\n" +
		PreferredTermsHeader + "\nkey => 密钥\npublic => 公开 / 公钥 (public key)\nPython"
	if got := GlossaryLines(terms); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	enforceOnly := []model.Term{terms[0], terms[4]}
	if got, w := GlossaryLines(enforceOnly), MandatoryTermsHeader+"\ncache => 缓存\nWrite-back => 写回\n\n"+PreferredTermsHeader+"\n(none)"; got != w {
		t.Errorf("enforce only: %q", got)
	}
	referenceOnly := []model.Term{terms[1], terms[3]}
	if got, w := GlossaryLines(referenceOnly), MandatoryTermsHeader+"\n(none)\n\n"+PreferredTermsHeader+"\nkey => 密钥\npublic => 公开 / 公钥 (public key)"; got != w {
		t.Errorf("reference only: %q", got)
	}
}

func TestOuterTerms(t *testing.T) {
	cases := []struct {
		text    string
		phrases []string
		want    []bool
	}{
		{"The secret key is short.", []string{"key", "secret key"}, []bool{false, true}},
		{"Secret keys are short.", []string{"key", "secret key"}, []bool{false, true}},
		{"The secret key and the key schedule.", []string{"key", "secret key"}, []bool{true, true}},
		{"Both keys.", []string{"key", "secret key"}, []bool{true, false}},
		{"the cache line size", []string{"cache line", "line size", "line"}, []bool{true, true, false}},
		{"Two buses and a bus.", []string{"bus"}, []bool{true}},
		{"A KEY.", []string{"key", "Key"}, []bool{true, true}}, // same phrase twice: no shadowing
		{"keyboard", []string{"key"}, []bool{false}},
		{"anything", nil, []bool{}},
		{"key", []string{""}, []bool{false}},
	}
	for _, c := range cases {
		got := OuterTerms(c.text, c.phrases)
		if !slices.Equal(got, c.want) {
			t.Errorf("OuterTerms(%q, %q) = %v, want %v", c.text, c.phrases, got, c.want)
		}
	}
}
