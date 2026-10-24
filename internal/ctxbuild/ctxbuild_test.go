// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"lectureflow/internal/model"
)

func TestSelectMaterialsAndSlides(t *testing.T) {
	lec := sampleSnapshot(t)
	sel := SelectMaterials(lec.Materials, "L1")
	if len(sel) != 2 || sel[0].ID != "m1" || sel[1].ID != "m2" {
		t.Fatalf("selected %+v", sel)
	}
	want := "--- page 1 ---\nCaches\nwrite-back vs write-through\n" +
		"--- page 2 ---\nSet associative caches\nLRU\n" +
		"--- page 1 ---\nCourse overview\nMemory hierarchy\n" +
		"--- page 2 ---\n"
	if got := LectureSlides(lec); got != want {
		t.Errorf("slides:\n%q\nwant\n%q", got, want)
	}
	if got := SlidesText(nil); got != None {
		t.Errorf("no material: %q", got)
	}
	if got := SlidesText([]model.Material{{ID: "x"}}); got != None {
		t.Errorf("unextracted material: %q", got)
	}
}

func TestSlidesTruncated(t *testing.T) {
	page := strings.Repeat("页", 1000) // multi-byte: truncation counts characters
	pages := make([]string, 40)
	for i := range pages {
		pages[i] = page
	}
	got := SlidesText([]model.Material{{PagesMD: pages}})
	if n := utf8.RuneCountInString(got); n != SlidesMaxChars {
		t.Errorf("slides length %d, want %d", n, SlidesMaxChars)
	}
	if !utf8.ValidString(got) {
		t.Error("truncation split a character")
	}
}

func TestGlossarySeedRequest(t *testing.T) {
	r := GlossarySeedRequest("Course: Computer Architecture\nTopics: caches")
	if r.System != GlossarySeedSystem || r.User != "SYLLABUS:\nCourse: Computer Architecture\nTopics: caches" {
		t.Errorf("request %+v", r)
	}
	long := GlossarySeedRequest(strings.Repeat("é", SyllabusMaxChars+10))
	if n := utf8.RuneCountInString(strings.TrimPrefix(long.User, "SYLLABUS:\n")); n != SyllabusMaxChars {
		t.Errorf("syllabus truncated to %d", n)
	}
}

func TestMaterialChunks(t *testing.T) {
	big := func(n int) string { return strings.Repeat("x", n) }
	cases := []struct {
		name  string
		pages []string
		sizes []int // rune length of each chunk
	}{
		{"empty", nil, nil},
		{"blank pages only", []string{"", "  \n"}, nil},
		{"one chunk", []string{"a", "", "b"}, []int{4}},
		{"exact fit", []string{big(9999), big(9999)}, []int{20000}},
		{"split at page boundary", []string{big(9999), big(9999), big(5)}, []int{20000, 5}},
		{"one over", []string{big(10000), big(9999)}, []int{10000, 9999}},
		{"oversized page alone", []string{big(5), big(25000), big(5)}, []int{5, 25000, 5}},
	}
	for _, c := range cases {
		chunks := MaterialChunks(c.pages)
		if len(chunks) != len(c.sizes) {
			t.Errorf("%s: %d chunks, want %d", c.name, len(chunks), len(c.sizes))
			continue
		}
		for i, ch := range chunks {
			if n := utf8.RuneCountInString(ch); n != c.sizes[i] {
				t.Errorf("%s: chunk %d has %d chars, want %d", c.name, i, n, c.sizes[i])
			}
		}
	}
	if got := MaterialChunks([]string{" a ", "b"}); len(got) != 1 || got[0] != "a\n\nb" {
		t.Errorf("join: %q", got)
	}
	reqs := MaterialTermsRequests([]string{"Caches", "LRU"})
	if len(reqs) != 1 || reqs[0].System != MaterialTermsSystem || reqs[0].User != "SLIDES:\nCaches\n\nLRU" {
		t.Errorf("requests %+v", reqs)
	}
}

func TestSummaryGolden(t *testing.T) {
	lec := sampleSnapshot(t)
	r := SummaryRequest(lec)
	golden(t, "summary_system.golden", r.System)
	golden(t, "summary_user.golden", r.User)
	lines := strings.Split(strings.TrimPrefix(r.User, "TRANSCRIPT:\n"), "\n")
	if len(lines) != len(lec.Sentences) || lines[1] != lec.Sentences[1].ASRText {
		t.Errorf("summary user must list every asr_text: %q", lines)
	}
	if SummaryRequest(clone(lec)) != r {
		t.Error("summary request not deterministic")
	}
}

func TestInputHashes(t *testing.T) {
	base := sampleSnapshot(t)
	sum, rep := SummaryInputHash(base), RepairInputHash(base)
	if len(sum) != 64 || len(rep) != 64 || sum == rep {
		t.Fatalf("hashes %q %q", sum, rep)
	}
	type mod struct {
		name            string
		f               func(*LectureSnapshot)
		summary, repair bool // expected change
	}
	mods := []mod{
		{"asr text", func(l *LectureSnapshot) { l.Sentences[3].ASRText += "!" }, true, true},
		{"sentence text (review)", func(l *LectureSnapshot) { l.Sentences[3].Text += "!" }, false, false},
		{"slides", func(l *LectureSnapshot) { l.Materials[2].PagesMD[0] += " more" }, true, false},
		{"other lecture material", func(l *LectureSnapshot) { l.Materials[1].PagesMD[0] += " more" }, false, false},
		{"summary", func(l *LectureSnapshot) { l.Summary += " More." }, false, true},
		{"term zh", func(l *LectureSnapshot) { l.Terms[0].Zh = "回写" }, false, true},
		{"term disabled", func(l *LectureSnapshot) { l.Terms[1].Enabled = false }, false, true},
		{"disabled term edited", func(l *LectureSnapshot) { l.Terms[4].Zh = "转换检测缓冲区" }, false, false},
		{"term freq", func(l *LectureSnapshot) { l.Terms[0].Freq = 99 }, false, false},
		{"term order", func(l *LectureSnapshot) { l.Terms[0], l.Terms[1] = l.Terms[1], l.Terms[0] }, false, false},
	}
	for _, m := range mods {
		lec := clone(base)
		m.f(&lec)
		if got := SummaryInputHash(lec) != sum; got != m.summary {
			t.Errorf("%s: summary hash changed=%v, want %v", m.name, got, m.summary)
		}
		if got := RepairInputHash(lec) != rep; got != m.repair {
			t.Errorf("%s: repair hash changed=%v, want %v", m.name, got, m.repair)
		}
	}

	// part boundaries are unambiguous
	if hashParts("ab", "c") == hashParts("a", "bc") {
		t.Error("hashParts collision")
	}
}

// TestASRInputHash pins the v1.1 asr hash (SPEC-v1.1 A1, lead ruling 3):
// wav hash + model + compute_type (configured value) + language; no prompt,
// no device.
func TestASRInputHash(t *testing.T) {
	p := ASRParams{Model: "large-v3", ComputeType: "auto", Language: "en"}
	h := ASRInputHash("abc", p)
	// SHA-256 of "3:asr" "3:abc" "8:large-v3" "4:auto" "2:en" (hashParts encoding)
	if want := "e898e663627d8d4705c26de5342be2f9a454b0276607ab5ec1b9b378cb697478"; h != want {
		t.Errorf("ASRInputHash = %s, want %s", h, want)
	}
	if ASRInputHash("abc", p) != h {
		t.Error("asr hash not deterministic")
	}
	model, ct, lang := p, p, p
	model.Model = "large-v2"
	ct.ComputeType = "float16"
	lang.Language = "de"
	for name, other := range map[string]string{
		"wav":          ASRInputHash("abd", p),
		"model":        ASRInputHash("abc", model),
		"compute_type": ASRInputHash("abc", ct),
		"language":     ASRInputHash("abc", lang),
	} {
		if other == h {
			t.Errorf("asr hash ignores %s", name)
		}
	}
}

// TestPackageIsPure enforces SPEC §1.1: no I/O, clock or randomness in
// ctxbuild (non-test files).
func TestPackageIsPure(t *testing.T) {
	checkPure(t, ".")
}

var forbiddenImports = []string{
	"os", "os/exec", "io/ioutil", "io/fs", "net", "net/http", "time", "math/rand", "math/rand/v2",
	"crypto/rand", "database/sql", "syscall", "log", "log/slog", "path/filepath", "runtime",
	"lectureflow/internal/db", "lectureflow/internal/llm", "lectureflow/internal/sched",
}

func checkPure(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if slices.Contains(forbiddenImports, p) || strings.HasPrefix(p, "net/") {
				t.Errorf("%s imports %s (SPEC §1.1: no I/O in this package)", f, p)
			}
		}
	}
}
