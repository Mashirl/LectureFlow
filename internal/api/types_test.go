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

// The example files under testdata/api must decode into the frozen structs
// without unknown fields, so the front-end mocks never drift from the API.
func TestExampleFilesMatchTypes(t *testing.T) {
	cases := map[string]any{
		"lecture.json":      &Lecture{},
		"lecture_done.json": &Lecture{},
		"lectures.json":     &[]Lecture{},
		"course.json":       &Course{},
		"courses.json":      &[]Course{},
		"terms.json":        &[]Term{},
		"sentences.json":    &SentencePage{},
		"patches.json":      &[]Patch{},
		"system.json":       &System{},
	}
	for name, v := range cases {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
