// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

type sampleRow struct {
	Idx          int                 `json:"idx"`
	StartMS      int64               `json:"start_ms"`
	EndMS        int64               `json:"end_ms"`
	ASRText      string              `json:"asr_text"`
	LowConfWords []model.LowConfWord `json:"low_conf_words"`
}

// sampleSentences loads testdata/sample_sentences.json with text = asr_text.
func sampleSentences(t *testing.T) []Sentence {
	t.Helper()
	b, err := os.ReadFile("../../testdata/sample_sentences.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []sampleRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	out := make([]Sentence, len(rows))
	for i, r := range rows {
		if r.Idx != i {
			t.Fatalf("sample idx %d at position %d", r.Idx, i)
		}
		out[i] = Sentence{Idx: r.Idx, Text: r.ASRText, ASRText: r.ASRText, LowConfWords: r.LowConfWords}
	}
	return out
}

func strp(s string) *string { return &s }

// sampleSnapshot is the 14-sentence sample lecture after a repair that fixed
// sentences 1, 2 and 4, with a small glossary (given out of order, one
// disabled term, one term without zh) and three materials.
func sampleSnapshot(t *testing.T) LectureSnapshot {
	ss := sampleSentences(t)
	ss[1].Text = "Today we talk about the cache hierarchy in modern processors."
	ss[2].Text = "A write-back cache only writes a line to memory on eviction."
	ss[4].Text = "So the first question is how we find a line in the cache."
	return LectureSnapshot{
		LectureID: "L1",
		Sentences: ss,
		Terms: []model.Term{
			{ID: "t3", En: "write-back", EnKey: "write-back", Zh: "写回", Source: model.SourceSyllabus, Enabled: true, Freq: 3},
			{ID: "t1", En: "cache", EnKey: "cache", Zh: "缓存", Source: model.SourceSyllabus, Enabled: true, Freq: 9},
			{ID: "t5", En: "LRU", EnKey: "lru", Zh: "", Source: model.SourceMaterial, Enabled: true, Freq: 1},
			{ID: "t2", En: "cache line", EnKey: "cache line", Zh: "缓存行", Source: model.SourceManual, Enabled: true, Freq: 2},
			{ID: "t4", En: "TLB", EnKey: "tlb", Zh: "快表", Source: model.SourceMaterial, Enabled: false, Freq: 5},
			{ID: "t6", En: "set associative", EnKey: "set associative", Zh: "组相联", Source: model.SourceMaterial, Enabled: true, Freq: 2},
		},
		Summary: "The lecture introduces caches: write-back policy, cache lines, address splitting, direct mapped and set associative caches, and LRU replacement.",
		Materials: []model.Material{
			{ID: "m2", CourseID: "C1", LectureID: nil, Filename: "course.pdf", CreatedAt: "2026-01-02T00:00:00Z",
				PagesMD: []string{"Course overview\nMemory hierarchy", ""}},
			{ID: "m3", CourseID: "C1", LectureID: strp("L2"), Filename: "other.pdf", CreatedAt: "2026-01-01T12:00:00Z",
				PagesMD: []string{"Virtual memory (other lecture)"}},
			{ID: "m1", CourseID: "C1", LectureID: strp("L1"), Filename: "caches.pdf", CreatedAt: "2026-01-01T00:00:00Z",
				PagesMD: []string{"Caches\nwrite-back vs write-through", "Set associative caches\nLRU"}},
		},
	}
}

// clone deep-copies a snapshot.
func clone(lec LectureSnapshot) LectureSnapshot {
	c := lec
	c.Sentences = make([]Sentence, len(lec.Sentences))
	for i, s := range lec.Sentences {
		s.LowConfWords = append([]model.LowConfWord(nil), s.LowConfWords...)
		c.Sentences[i] = s
	}
	c.Terms = append([]model.Term(nil), lec.Terms...)
	c.Materials = make([]model.Material, len(lec.Materials))
	for i, m := range lec.Materials {
		m.PagesMD = append([]string(nil), m.PagesMD...)
		c.Materials[i] = m
	}
	return c
}

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
		t.Fatalf("%v (run go test ./internal/ctxbuild -update to create it)", err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs from golden file:\n%s", name, diffHint(got, w))
	}
}
