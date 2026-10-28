// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/components"
	"lectureflow/internal/devices"
	"lectureflow/internal/fetch"
	"lectureflow/internal/glossary"
	"lectureflow/internal/llm"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/selfupdate"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// TestBackendErrorCodesCatalog keeps testdata/api/error_codes_backend.json
// (SPEC-v2 P14.3, codes-p2) in step with the codes the back-end packages
// define: every code is listed exactly once, has a kind and a Chinese text
// whose placeholders are among its params, its extra parts exist, and no
// code is also one of the HTTP layer's (error_codes.json).
func TestBackendErrorCodesCatalog(t *testing.T) {
	read := func(name string, v any) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var raw map[string]json.RawMessage
	read("error_codes_backend.json", &raw)
	type entry struct {
		Zh     string   `json:"zh"`
		Params []string `json:"params"`
		Kind   string   `json:"kind"`
		Extra  []string `json:"extra"`
	}
	catalog := map[string]entry{}
	for code, b := range raw {
		if code[0] == '_' {
			continue
		}
		var e entry
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		catalog[code] = e
	}

	var defined []string
	for _, list := range [][]string{settings.ErrorCodes, llm.ErrorCodes, stages.ErrorCodes, glossary.ErrorCodes,
		sched.ErrorCodes, resources.ErrorCodes, state.ErrorCodes, devices.ErrorCodes, colab.ErrorCodes, components.ErrorCodes,
		fetch.ErrorCodes, selfupdate.ErrorCodes} {
		defined = append(defined, list...)
	}
	seen := map[string]bool{}
	for _, c := range defined {
		if seen[c] {
			t.Errorf("code %s defined twice", c)
		}
		seen[c] = true
		if _, ok := catalog[c]; !ok {
			t.Errorf("code %s is not in error_codes_backend.json", c)
		}
	}
	var stale []string
	for c := range catalog {
		if !seen[c] {
			stale = append(stale, c)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("error_codes_backend.json lists codes no package defines: %v", stale)
	}

	placeholder := regexp.MustCompile(`\{(\w+)\}`)
	for code, e := range catalog {
		if e.Zh == "" || !slices.Contains([]string{"error", "note", "warning"}, e.Kind) || e.Params == nil {
			t.Errorf("%s: incomplete entry %+v", code, e)
		}
		for _, m := range placeholder.FindAllStringSubmatch(e.Zh, -1) {
			if !slices.Contains(e.Params, m[1]) {
				t.Errorf("%s: placeholder {%s} is not a param", code, m[1])
			}
		}
		for _, x := range e.Extra {
			if xe, ok := catalog[x]; !ok || xe.Kind != e.Kind {
				t.Errorf("%s: extra part %s missing or of another kind", code, x)
			}
		}
	}

	var httpCodes map[string]json.RawMessage
	read("error_codes.json", &httpCodes)
	for c := range catalog {
		if _, ok := httpCodes[c]; ok {
			t.Errorf("%s is in both error_codes.json and error_codes_backend.json", c)
		}
	}
	t.Logf("%d back-end codes", len(catalog))
}
