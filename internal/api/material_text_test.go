// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestTextIncomplete(t *testing.T) {
	cases := []struct {
		empty, total int
		want         bool
	}{
		{0, 0, false},    // no pages: nothing to judge
		{0, 10, false},   // every page has text
		{1, 1, true},     // the only page is empty (100 %)
		{1, 2, true},     // exactly 50 %
		{1, 3, false},    // 33 %, fewer than 3 pages
		{2, 4, true},     // exactly 50 %
		{2, 5, false},    // 40 %, fewer than 3 pages
		{1, 50, false},   // a title picture
		{2, 50, false},   // two figures: normal
		{5, 10, true},    // exactly 50 %
		{4, 10, true},    // 3+ and 40 %
		{3, 15, true},    // 3 pages and exactly 20 %
		{3, 16, false},   // 3 pages but under 20 %
		{3, 100, false},  // 3 %: a few section dividers
		{20, 100, true},  // exactly 20 %
		{19, 100, false}, // just under 20 %
		{3, 3, true},
	}
	for _, c := range cases {
		if got := textIncomplete(c.empty, c.total); got != c.want {
			t.Errorf("textIncomplete(%d, %d) = %v, want %v", c.empty, c.total, got, c.want)
		}
	}
}

func TestMaterialTextWarning(t *testing.T) {
	if w := materialTextWarning(nil); w != "" {
		t.Errorf("not extracted: %q", w)
	}
	if w := materialTextWarning([]string{}); w != "" {
		t.Errorf("0 pages: %q", w)
	}
	// White space only counts as empty (the worker trims, older rows may not).
	if w := materialTextWarning([]string{"Intro", " \n\t", "　"}); w != TextWarningIncomplete {
		t.Errorf("2 of 3 blank: %q", w)
	}
	if w := materialTextWarning([]string{"Intro", "", "Body", "More"}); w != "" {
		t.Errorf("1 of 4 empty: %q", w)
	}
}

// The course view and the material view carry text_warning once the
// extracted pages are mostly empty, and omit it otherwise.
func TestMaterialTextWarningInViews(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	base := "/api/courses/" + c.ID
	upload := func(name string) Material {
		t.Helper()
		res, b := e.upload(base+"/materials", nil, name, []byte("%PDF-1.4"))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", name, res.StatusCode, b)
		}
		var cv Course
		if err := json.Unmarshal(b, &cv); err != nil {
			t.Fatal(err)
		}
		for _, m := range cv.Materials {
			if m.Filename == name {
				return m
			}
		}
		t.Fatalf("%s not in %+v", name, cv.Materials)
		return Material{}
	}
	scan := upload("scan.pdf")
	slides := upload("slides.pdf")
	if scan.TextWarning != "" {
		t.Errorf("not extracted yet: %q", scan.TextWarning)
	}
	ctx := context.Background()
	if err := e.st.SetMaterialPages(ctx, scan.ID, []string{"Title", "", " ", ""}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetMaterialPages(ctx, slides.ID, []string{"Title", "", "Body", "More", "End"}); err != nil {
		t.Fatal(err)
	}
	var cv Course
	e.call("GET", base, nil, http.StatusOK, &cv)
	got := map[string]string{}
	for _, m := range cv.Materials {
		got[m.Filename] = m.TextWarning
	}
	if got["scan.pdf"] != TextWarningIncomplete || got["slides.pdf"] != "" {
		t.Errorf("course view warnings %v", got)
	}
	var mv Material
	e.call("PATCH", "/api/materials/"+scan.ID, map[string]any{"lecture_id": nil}, http.StatusOK, &mv)
	if mv.TextWarning != TextWarningIncomplete {
		t.Errorf("material view %+v", mv)
	}
	// omitempty: no key at all on a clean file.
	res, b := e.do("GET", base, nil)
	if res.StatusCode != http.StatusOK || strings.Count(string(b), `"text_warning"`) != 1 {
		t.Errorf("text_warning keys in %s", b)
	}
}
