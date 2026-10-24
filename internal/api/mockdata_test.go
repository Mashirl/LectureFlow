// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mockExamples lists the extra front-end mock files (testdata/api/mock_*.json,
// added by web-builder). Like the frozen examples they must decode into the
// API structs without unknown fields.
var mockExamples = map[string]func() any{
	"mock_lectures.json":  func() any { return &[]Lecture{} },
	"mock_sentences.json": func() any { return &SentencePage{} },
	"mock_patches.json":   func() any { return &[]Patch{} },
	"mock_system_ok.json": func() any { return &System{} },
}

func TestMockFilesMatchTypes(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "api")
	files, err := filepath.Glob(filepath.Join(dir, "mock_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, ok := mockExamples[filepath.Base(f)]; !ok {
			t.Errorf("%s is not listed in mockExamples", filepath.Base(f))
		}
	}
	for name, mk := range mockExamples {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(mk()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The transcript view assumes sentences[i].idx == i and the review cards
// point at real sentences; keep the mock data consistent with that.
func TestMockSentencesAndPatchesConsistent(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "api")
	var page SentencePage
	var patches []Patch
	readJSON(t, filepath.Join(dir, "mock_sentences.json"), &page)
	readJSON(t, filepath.Join(dir, "mock_patches.json"), &patches)
	if page.Total != len(page.Sentences) {
		t.Errorf("total %d != %d sentences", page.Total, len(page.Sentences))
	}
	for i, s := range page.Sentences {
		if s.Idx != i {
			t.Errorf("sentence %d has idx %d", i, s.Idx)
		}
		want := s.RepairedText
		if s.HumanText != nil {
			want = *s.HumanText
		}
		if s.Text != want {
			t.Errorf("sentence %d: text %q, want %q (human_text ?? repaired_text)", i, s.Text, want)
		}
	}
	for _, p := range patches {
		if p.SentenceIdx < 0 || p.SentenceIdx >= len(page.Sentences) {
			t.Errorf("patch %s: sentence_idx %d out of range", p.ID, p.SentenceIdx)
			continue
		}
		s := page.Sentences[p.SentenceIdx]
		if s.Patch == nil || s.Patch.ID != p.ID || s.Patch.Review != p.Review {
			t.Errorf("patch %s does not match sentence %d's patch ref", p.ID, p.SentenceIdx)
		}
		if p.Before != s.ASRText {
			t.Errorf("patch %s: before must be the asr_text", p.ID)
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
