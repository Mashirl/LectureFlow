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

	"lectureflow/internal/model"
)

// TestTranslatePromptV2 compares the translate system prompt with SPEC.md
// §13.5 as amended by SPEC-v2: rule 2 replaced by the G5.2 text (verbatim)
// and rule 1 naming the BEFORE and AFTER sections (G5.3). The frozen v1
// template used by the hashes must stay SPEC.md §13.5 verbatim.
func TestTranslatePromptV2(t *testing.T) {
	b := codeBlocks(specSection(t, "### 13.5 translate"))
	if len(b) != 1 {
		t.Fatalf("13.5: %d code blocks", len(b))
	}
	v1 := b[0]
	if translateSystemTemplateV1 != v1 {
		t.Errorf("translateSystemTemplateV1 differs from SPEC.md §13.5:\n%s", diffHint(translateSystemTemplateV1, v1))
	}
	g52 := codeBlocks(specFileSection(t, "v2.md", "### G5.2 "))
	if len(g52) != 1 {
		t.Fatalf("G5.2: %d code blocks", len(g52))
	}
	lines := strings.Split(v1, "\n")
	replaced := 0
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "- When a glossary term"): // §13.5 rule 2 → G5.2
			lines[i] = g52[0]
			replaced++
		case strings.HasPrefix(l, "- Translate only the TARGET sentence."): // §13.5 rule 1 → G5.3
			lines[i] = strings.Replace(l, "The sentences before and after are context", "The BEFORE and AFTER sentences are context", 1)
			replaced++
		}
	}
	if replaced != 2 {
		t.Fatalf("SPEC.md §13.5: %d rules replaced, want 2", replaced)
	}
	if want := strings.Join(lines, "\n"); TranslateSystemTemplate != want {
		t.Errorf("TranslateSystemTemplate differs from SPEC §13.5 + G5.2 + G5.3:\n%s", diffHint(TranslateSystemTemplate, want))
	}
	// G5.3 quotes the two system prompt sentences ("…" = the rest unchanged)
	g53 := specFileSection(t, "v2.md", "### G5.3 ")
	quotes := regexp.MustCompile(`“([^”]*)…”`).FindAllStringSubmatch(g53, -1)
	if len(quotes) != 2 {
		t.Fatalf("G5.3: %d quoted sentences", len(quotes))
	}
	for _, q := range quotes {
		if !strings.Contains(TranslateSystemTemplate, q[1]) {
			t.Errorf("TranslateSystemTemplate lacks G5.3 %q", q[1])
		}
	}
	// the fake LLM identifies the role by the first line (SPEC §14)
	if FirstLine(TranslateSystemTemplate) != FirstLine(v1) {
		t.Error("the first line of the translate prompt changed")
	}
	// the labels of the user message are those of G5.3
	block := codeBlocks(g53)
	if len(block) != 1 {
		t.Fatalf("G5.3: %d code blocks", len(block))
	}
	var labels []string
	for _, l := range strings.Split(block[0], "\n") {
		if strings.HasPrefix(l, "[") {
			labels = append(labels, l)
		}
	}
	if want := []string{LabelBefore, fmt.Sprintf(LabelTarget, 376), LabelASR, LabelAfter}; !slices.Equal(labels, want) {
		t.Errorf("G5.3 labels %q, constants give %q", labels, want)
	}
}

// tieredSnapshot is the sample lecture with two reference terms.
func tieredSnapshot(t *testing.T) LectureSnapshot {
	lec := sampleSnapshot(t)
	lec.Terms = append(lec.Terms,
		model.Term{ID: "t7", En: "line", EnKey: "line", Zh: "行", Source: model.SourceMaterial, Enabled: true, Freq: 12, Tier: model.TierReference},
		model.Term{ID: "t8", En: "hit", EnKey: "hit", Zh: "命中", Source: model.SourceMaterial, Enabled: true, Freq: 4, Tier: model.TierReference, CommonWord: true},
	)
	return lec
}

// TestTieredPrefixStable: with both tiers, the translate prefix is byte-
// identical for every sentence of a lecture and independent of the load
// order (cache hits, SPEC §7.1); the repair prefix is identical for every
// chunk.
func TestTieredPrefixStable(t *testing.T) {
	lec := tieredSnapshot(t)
	other := clone(lec)
	slices.Reverse(other.Terms)
	slices.Reverse(other.Materials)
	first := BuildTranslateRequest(lec, 0, testX).System
	for i := range lec.Sentences {
		a, b := BuildTranslateRequest(lec, i, testX), BuildTranslateRequest(other, i, testX)
		if a.System != first {
			t.Fatalf("translate prefix of idx %d differs from idx 0", i)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("idx %d: the request depends on the load order", i)
		}
	}
	want := "GLOSSARY:\n" + MandatoryTermsHeader + "\ncache => 缓存\ncache line => 缓存行\nLRU\nset associative => 组相联\nwrite-back => 写回\n\n" +
		PreferredTermsHeader + "\nhit => 命中\nline => 行\n\nSUMMARY:\n" + lec.Summary
	if !strings.HasSuffix(first, want) {
		t.Errorf("translate prefix tail:\n%s", first[strings.Index(first, "GLOSSARY:"):])
	}
	if !strings.Contains(SummarySystem(lec), "GLOSSARY:\n"+GlossaryLines(lec.Terms)+"\n\nSLIDES:\n") {
		t.Error("summary prefix lacks the two-section glossary")
	}
	if !strings.Contains(RepairSystem(lec), "GLOSSARY:\n"+GlossaryLines(lec.Terms)+"\n\nSUMMARY:\n") {
		t.Error("repair prefix lacks the two-section glossary")
	}
	long := clone(lec)
	long.Sentences = synthLecture(130).Sentences
	chunks := BuildRepairRequest(long)
	for k, c := range chunks {
		if c.System != chunks[0].System {
			t.Errorf("repair chunk %d prefix differs", k)
		}
	}
}

// TestPromptsDoNotEnterHashes: the hashes never contain the v2 prompt text
// (lead ruling 9): the translate request's hash is the one recomputed from
// the frozen v1 template, and editing the v2 template wording would not
// change it (checked by recomputing with the v1 prefix).
func TestPromptsDoNotEnterHashes(t *testing.T) {
	lec := tieredSnapshot(t)
	v1Prefix := fill(translateSystemTemplateV1, "glossary_lines", GlossaryHashText(lec.Terms), "summary", summaryOrNone(lec.Summary))
	for i := range lec.Sentences {
		r := BuildTranslateRequest(lec, i, testX)
		if r.Hash != translateHash(v1Prefix, lec.Sentences, i, testX) {
			t.Fatalf("idx %d: hash is not computed from the frozen v1 prefix", i)
		}
		if r.Hash == translateHash(r.System, lec.Sentences, i, testX) {
			t.Fatalf("idx %d: hash computed from the v2 system prompt", i)
		}
	}
}
