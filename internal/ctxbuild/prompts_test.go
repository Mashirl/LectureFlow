// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// specSection returns the text of SPEC.md from the heading line that starts
// with heading up to the next "### " or "## " heading (headings inside
// fenced code blocks, such as the Markdown of SPEC-v1.1 A8.3, do not count).
func specSection(t *testing.T, heading string) string {
	t.Helper()
	return specFileSection(t, "v1.md", heading)
}

// specFileSection is specSection for any version of the specification. The
// prompt sections of each version are in testdata/prompt-spec (v1.md is
// SPEC.md, v1.1.md SPEC-v1.1.md, v2.md SPEC-v2.md), copied verbatim.
func specFileSection(t *testing.T, file, heading string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "prompt-spec", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	spec := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(spec, "\n"+heading)
	if i < 0 {
		t.Fatalf("%s: heading %q not found", file, heading)
	}
	rest := spec[i+1:]
	nl := strings.IndexByte(rest, '\n')
	lines := strings.Split(rest[nl+1:], "\n")
	heading2 := regexp.MustCompile(`^##+ `)
	inFence := false
	for k, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
			continue
		}
		if !inFence && heading2.MatchString(l) {
			lines = lines[:k]
			break
		}
	}
	return strings.Join(lines, "\n")
}

// codeBlocks returns the fenced ``` blocks of a section, without the fences.
func codeBlocks(section string) []string {
	var blocks []string
	lines := strings.Split(section, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```" {
			continue
		}
		var buf []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			buf = append(buf, lines[i])
		}
		blocks = append(blocks, strings.Join(buf, "\n"))
	}
	return blocks
}

// inlineUser returns the inline code after "user：" in a section, with the
// literal \n turned into a newline.
func inlineUser(t *testing.T, section string) string {
	t.Helper()
	m := regexp.MustCompile("user：`([^`]*)`").FindStringSubmatch(section)
	if m == nil {
		t.Fatalf("no inline user template in section")
	}
	return strings.ReplaceAll(m[1], `\n`, "\n")
}

func TestPromptsVerbatim(t *testing.T) {
	s131 := specSection(t, "### 13.1 glossary_seed")
	b := codeBlocks(s131)
	if len(b) != 1 {
		t.Fatalf("13.1: %d code blocks", len(b))
	}
	if GlossarySeedSystem != b[0] {
		t.Errorf("GlossarySeedSystem differs from SPEC §13.1:\n%s", diffHint(GlossarySeedSystem, b[0]))
	}
	if got := inlineUser(t, s131); got != GlossarySeedUserTemplate {
		t.Errorf("13.1 user: got %q want %q", GlossarySeedUserTemplate, got)
	}

	s132 := specSection(t, "### 13.2 material_terms")
	line := ""
	for _, l := range strings.Split(s132, "\n") {
		if strings.Contains(l, "第一句改为") {
			line = l
		}
	}
	inl := regexp.MustCompile("`([^`]*)`").FindAllStringSubmatch(line, -1)
	if len(inl) != 3 {
		t.Fatalf("13.2: expected 3 inline codes in %q", line)
	}
	newFirst, from, to := inl[0][1], inl[1][1], inl[2][1]
	oldFirst := b[0][:strings.Index(b[0], ". ")+1]
	want := strings.Replace(b[0], oldFirst, newFirst, 1)
	if strings.Count(b[0], from) != 1 {
		t.Fatalf("13.2: %q occurs %d times in 13.1", from, strings.Count(b[0], from))
	}
	want = strings.Replace(want, from, to, 1)
	if MaterialTermsSystem != want {
		t.Errorf("MaterialTermsSystem differs from SPEC §13.2:\n%s", diffHint(MaterialTermsSystem, want))
	}
	if !strings.HasPrefix(MaterialTermsSystem, newFirst+" The terms will be used") || !strings.Contains(MaterialTermsSystem, "at most 100 terms") {
		t.Errorf("MaterialTermsSystem replacements not applied")
	}
	if got := inlineUser(t, s132); got != MaterialTermsUserTemplate {
		t.Errorf("13.2 user: got %q want %q", MaterialTermsUserTemplate, got)
	}

	s133 := specSection(t, "### 13.3 summary")
	if b := codeBlocks(s133); len(b) != 1 || b[0] != SummarySystemTemplate {
		t.Errorf("SummarySystemTemplate differs from SPEC §13.3")
	}
	if got := inlineUser(t, s133); got != SummaryUserTemplate {
		t.Errorf("13.3 user: got %q want %q", SummaryUserTemplate, got)
	}

	s134 := specSection(t, "### 13.4 repair")
	b4 := codeBlocks(s134)
	if len(b4) != 2 {
		t.Fatalf("13.4: %d code blocks", len(b4))
	}
	if b4[0] != RepairSystemTemplate {
		t.Errorf("RepairSystemTemplate differs from SPEC §13.4:\n%s", diffHint(RepairSystemTemplate, b4[0]))
	}
	if b4[1] != RepairUserTemplate {
		t.Errorf("RepairUserTemplate differs from SPEC §13.4:\n%s", diffHint(RepairUserTemplate, b4[1]))
	}
	if !strings.Contains(s134, "`"+None+"`") {
		t.Errorf("13.4 no longer mentions %s", None)
	}

	s135 := specSection(t, "### 13.5 translate")
	b5 := codeBlocks(s135)
	// SPEC-v2 G5.2/G5.3 amend two rules of the live prompt (checked by
	// TestTranslatePromptV2); the frozen v1 copy used by the hashes is §13.5.
	if len(b5) != 1 || b5[0] != translateSystemTemplateV1 {
		t.Errorf("translateSystemTemplateV1 differs from SPEC §13.5")
	}
	m := regexp.MustCompile("末尾追加：`([^`]*)`").FindStringSubmatch(s135)
	if m == nil {
		t.Fatal("13.5: retry suffix not found")
	}
	if got := strings.ReplaceAll(m[1], `\n`, "\n"); got != TranslateRetrySuffix {
		t.Errorf("TranslateRetrySuffix: got %q want %q", TranslateRetrySuffix, got)
	}
}

// TestSyllabusFormatPromptVerbatim compares the syllabus_format prompt and
// request with SPEC-v1.1 A8.3 (same verbatim rule as SPEC §13).
func TestSyllabusFormatPromptVerbatim(t *testing.T) {
	sec := specFileSection(t, "v1.1.md", "### A8.3 提示词（syllabus_format）")
	b := codeBlocks(sec)
	if len(b) != 1 {
		t.Fatalf("A8.3: %d code blocks", len(b))
	}
	if SyllabusFormatSystem != b[0] {
		t.Errorf("SyllabusFormatSystem differs from SPEC-v1.1 A8.3:\n%s", diffHint(SyllabusFormatSystem, b[0]))
	}
	if got := inlineUser(t, sec); got != SyllabusFormatUserTemplate {
		t.Errorf("A8.3 user: got %q want %q", SyllabusFormatUserTemplate, got)
	}
	raw := "CSC1003 Cryptography\nWeek 1: {raw_text} and {summary} stay literal.\n"
	system, user := BuildSyllabusFormatRequest(raw)
	if system != b[0] {
		t.Error("BuildSyllabusFormatRequest: system is not the A8.3 prompt")
	}
	if want := "RAW SYLLABUS:\n" + raw; user != want {
		t.Errorf("BuildSyllabusFormatRequest user = %q, want %q", user, want)
	}
	long := strings.Repeat("x", SyllabusMaxChars+10)
	if _, u := BuildSyllabusFormatRequest(long); u != "RAW SYLLABUS:\n"+long {
		t.Error("the raw syllabus must be passed whole")
	}
}

// TestFilledPromptsKeepTemplate checks that filling only replaces placeholders.
func TestFilledPromptsKeepTemplate(t *testing.T) {
	lec := sampleSnapshot(t)
	cases := map[string]struct{ tmpl, got string }{
		"summary":   {SummarySystemTemplate, SummarySystem(lec)},
		"repair":    {RepairSystemTemplate, RepairSystem(lec)},
		"translate": {TranslateSystemTemplate, TranslateSystem(lec)},
	}
	ph := regexp.MustCompile(`\{[a-z_]+\}`)
	for name, c := range cases {
		parts := ph.Split(c.tmpl, -1)
		pos := 0
		for _, p := range parts {
			i := strings.Index(c.got[pos:], p)
			if i < 0 {
				t.Errorf("%s: template fragment %q missing from filled prompt", name, p)
				break
			}
			pos += i + len(p)
		}
		if ph.MatchString(c.got) {
			t.Errorf("%s: unreplaced placeholder in %q", name, ph.FindString(c.got))
		}
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
