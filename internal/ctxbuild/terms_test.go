// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"strings"
	"testing"

	"lectureflow/internal/model"
)

func TestEnKey(t *testing.T) {
	cases := map[string]string{
		"  Cache   Line ":  "cache line",
		"LRU":              "lru",
		"write-\tback":     "write- back",
		"Set\nAssociative": "set associative",
		"":                 "",
	}
	for in, want := range cases {
		if got := EnKey(in); got != want {
			t.Errorf("EnKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDropTerm(t *testing.T) {
	cases := []struct {
		en   string
		drop bool
	}{
		{"a", true},
		{" C ", true},
		{"C#", false},
		{"Go", false},
		{"64", true},
		{"3.14", true},
		{"--", true},
		{"(1-2)", true},
		{"x86", false},
		{"L1 cache", false},
		{"one two three four five six", false},
		{"one two three four five six seven", true},
		{"Week 3 lecture overview of the memory hierarchy", true},
		{"", true},
	}
	for _, c := range cases {
		if got := DropTerm(c.en); got != c.drop {
			t.Errorf("DropTerm(%q) = %v, want %v", c.en, got, c.drop)
		}
	}
}

func TestCountTerm(t *testing.T) {
	cases := []struct {
		text, phrase string
		n            int
	}{
		{"Cache misses and the CACHE.", "cache", 2},
		{"cached caches cache", "cache", 1},
		{"Each cache line, every cache  line\nhere", "Cache Line", 2},
		{"a write-back cache", "write-back", 1},
		{"a write-back cache", "back", 1}, // hyphen is a boundary
		{"x86-64 machines", "x86", 1},
		{"conflict misses", "conflict miss", 0},
		{"C++ and C++11", "C++", 1},
		{"tag_index", "tag", 0},
		{"", "cache", 0},
		{"cache", "", 0},
		{"Über über", "über", 2},
	}
	for _, c := range cases {
		if got := CountTerm(c.text, c.phrase); got != c.n {
			t.Errorf("CountTerm(%q, %q) = %d, want %d", c.text, c.phrase, got, c.n)
		}
		if got := PrepareTermText(c.text).Count(c.phrase); got != c.n {
			t.Errorf("TermText(%q).Count(%q) = %d, want %d", c.text, c.phrase, got, c.n)
		}
		if ContainsTerm(c.text, c.phrase) != (c.n > 0) {
			t.Errorf("ContainsTerm(%q, %q) inconsistent", c.text, c.phrase)
		}
	}
}

func TestContainsTermPlural(t *testing.T) {
	cases := []struct {
		text, phrase string
		want         bool
	}{
		{"Two caches here.", "cache", true},
		{"The cache.", "cache", true},
		{"Many page faults occur.", "page fault", true},
		{"Shared buses.", "bus", true},
		{"cached data", "cache", false}, // other suffixes do not count
		{"cachess", "cache", false},     // only s / es
		{"page faulty", "page fault", false},
		{"pages fault", "page fault", false}, // the plural only applies to the last word
		{"ship it", "IP", false},
		{"IPs and IP", "IP", true},
		{"x86-64", "x86", true},
		{"", "cache", false},
		{"cache", "", false},
	}
	for _, c := range cases {
		if got := ContainsTermPlural(c.text, c.phrase); got != c.want {
			t.Errorf("ContainsTermPlural(%q, %q) = %v, want %v", c.text, c.phrase, got, c.want)
		}
	}
}

func TestStripTerms(t *testing.T) {
	got := StripTerms("最后，LRU 替换和 LRU cache 都用 lru", []string{"LRU", "LRU cache"})
	if strings.Contains(strings.ToLower(got), "lru") || strings.Contains(got, "cache") {
		t.Errorf("got %q", got)
	}
	if got := StripTerms("TLBs are TLB", []string{"TLB"}); got != "tlbs are  " {
		t.Errorf("got %q", got)
	}
}

// TestGlossaryLines: the v1 line format, kept by the MANDATORY section of
// {glossary_lines} (SPEC-v2 G5.1) and by GlossaryHashText for enforce terms.
func TestGlossaryLines(t *testing.T) {
	none := MandatoryTermsHeader + "\n(none)\n\n" + PreferredTermsHeader + "\n(none)"
	if got := GlossaryLines(nil); got != none {
		t.Errorf("no terms: %q", got)
	}
	if got := GlossaryLines([]model.Term{{En: "TLB", Zh: "快表", Enabled: false}}); got != none {
		t.Errorf("only disabled: %q", got)
	}
	terms := []model.Term{
		{En: "Write-back", EnKey: "write-back", Zh: "写回", Enabled: true},
		{En: "LRU", Zh: "  ", Enabled: true}, // EnKey computed when missing; blank zh = none
		{En: "cache", EnKey: "cache", Zh: "缓存", Enabled: true},
		{En: "TLB", EnKey: "tlb", Zh: "快表", Enabled: false},
		{En: "Cache", EnKey: "cache", Zh: "高速缓存", Enabled: true}, // duplicate key: first wins
	}
	want := "cache => 缓存\nLRU\nWrite-back => 写回"
	if got := GlossaryLines(terms); got != MandatoryTermsHeader+"\n"+want+"\n\n"+PreferredTermsHeader+"\n(none)" {
		t.Errorf("got %q want the MANDATORY section %q", got, want)
	}
	if got := GlossaryHashText(terms); got != want {
		t.Errorf("hash text %q want %q", got, want)
	}
}

func TestBuildASRPrompt(t *testing.T) {
	terms := []model.Term{
		{En: "cache", EnKey: "cache", Source: model.SourceSyllabus, Enabled: true, Freq: 9},
		{En: "TLB", EnKey: "tlb", Source: model.SourceMaterial, Enabled: false, Freq: 50},
		{En: "write-back", EnKey: "write-back", Source: model.SourceManual, Enabled: true, Freq: 1},
		{En: "LRU", EnKey: "lru", Source: model.SourceMaterial, Enabled: true, Freq: 4},
		{En: "Belady", EnKey: "belady", Source: model.SourceReview, Enabled: true, Freq: 0},
		{En: "MESI", EnKey: "mesi", Source: model.SourceMaterial, Enabled: true, Freq: 4},
		{En: "cache line", EnKey: "cache line", Source: model.SourceManual, Enabled: true, Freq: 7},
	}
	want := "cache line, write-back, Belady, cache, LRU, MESI"
	if got := BuildASRPrompt(terms, ""); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if BuildASRPrompt(nil, "") != "" {
		t.Error("no terms must give an empty prompt")
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		d    int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"flaw", "lawn", 2},
		{"缓存", "缓存行", 1},
		{"héllo", "hello", 1},
		{"same", "same", 0},
	}
	for _, c := range cases {
		if got := Levenshtein(c.a, c.b); got != c.d {
			t.Errorf("Levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.d)
		}
		if got := Levenshtein(c.b, c.a); got != c.d {
			t.Errorf("Levenshtein not symmetric for %q, %q", c.a, c.b)
		}
	}
}

func TestEditExceedsHalf(t *testing.T) {
	cases := []struct {
		before, after string
		exceeds       bool
	}{
		{"abcd", "abxy", false}, // 2 of 4 = exactly 50%: not more
		{"abcd", "axyz", true},  // 3 of 4
		{"Today we talk about the cash hierarchy in modern processors.", "Today we talk about the cache hierarchy in modern processors.", false},
		{"Blah lab is something we will see in the lab next week.", "Belady's optimal replacement algorithm is something we will study in next week's lab session.", true},
		{"", "x", true},
		{"", "", false},
	}
	for _, c := range cases {
		if got := EditExceedsHalf(c.before, c.after); got != c.exceeds {
			t.Errorf("EditExceedsHalf(%q, %q) = %v (distance %d)", c.before, c.after, got, Levenshtein(c.before, c.after))
		}
	}
}
