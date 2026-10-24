// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
)

// SPEC-v2 P14.3 (p2-tail): the reasons of the file-name rule preview
// (calendar.Parsed.Error, calendar.Validate's problems) also come as codes
// of the catalog — error_info on the rows, problems_info beside problems —
// so the front end can translate them.

// zhOfProblem renders an ErrorInfo of a calendar problem with the catalog's
// Chinese text, joining list params with "、" and rendering a nested reason
// first (zhText itself only knows scalar params).
func zhOfProblem(info model.ErrorInfo) string {
	flat := model.ErrorInfo{Code: info.Code, Params: map[string]any{}}
	for k, v := range info.Params {
		switch x := v.(type) {
		case []string:
			v = strings.Join(x, "、")
		case model.ErrorInfo:
			v = zhOfProblem(x)
		}
		flat.Params[k] = v
	}
	return zhText(flat)
}

func TestFilenameProblemCatalog(t *testing.T) {
	for _, code := range calendar.ErrorCodes {
		if _, ok := errorCatalog[code]; !ok {
			t.Errorf("calendar code %s is not in the catalog", code)
		}
	}
	up := time.Date(2026, 10, 20, 21, 10, 0, 0, time.UTC)
	term := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	s := func(names ...string) []calendar.Sample {
		out := []calendar.Sample{}
		for _, n := range names {
			out = append(out, calendar.Sample{Filename: n, UploadedAt: up})
		}
		return out
	}
	ymd := `_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})(?:_(?P<hour>\d{2})(?P<minute>\d{2}))?`
	runs := []struct {
		regex   string
		samples []calendar.Sample
	}{
		{"(", s("a.mp4")},
		{`(?P<mon>\d+)`, s("a.mp4")},
		{ymd, nil},
		{ymd, s("x_2026-13-01.mp4", "x_2026-10-32.mp4", "x_1890-10-01.mp4", "x_2026-02-30.mp4", "x_2026-10-01_2575.mp4",
			"x_2026-10-21.mp4", "x_2026-08-01.mp4", "intro.mp4", "a.mp4", "b.mp4", "c.mp4", "d.mp4")},
	}
	seen := map[string]bool{}
	check := func(what string, info model.ErrorInfo, text string) {
		t.Helper()
		seen[info.Code] = true
		wantCataloged(t, what, &info)
		if info.Detail != text {
			t.Errorf("%s: detail %q, want %q", what, info.Detail, text)
		}
		// The catalog's text is the calendar package's text.
		if got := zhOfProblem(info); got != text {
			t.Errorf("%s: catalog text %q, calendar text %q", what, got, text)
		}
		if r, ok := info.Params["reason"]; ok {
			sub, ok := r.(model.ErrorInfo)
			if !ok {
				t.Errorf("%s: reason %#v is not an ErrorInfo", what, r)
			} else {
				wantCataloged(t, what+" reason", &sub)
			}
		}
	}
	for _, r := range runs {
		res := ValidateRule(calendar.FilenameRule{Regex: r.regex}, r.samples, &term)
		if len(res.Problems) != len(res.ProblemsInfo) || len(res.Problems) == 0 {
			t.Fatalf("%s: problems %q, problems_info %+v", r.regex, res.Problems, res.ProblemsInfo)
		}
		for i, info := range res.ProblemsInfo {
			check(r.regex+" problem", info, res.Problems[i])
		}
		for _, row := range res.Preview {
			if (row.ErrorInfo == nil) != (row.Error == "") {
				t.Errorf("%s: row %+v", row.Filename, row)
			} else if row.ErrorInfo != nil {
				check(row.Filename, *row.ErrorInfo, row.Error)
			}
			// Lecture kinds (lead ruling 1.1): a date more than 30 days
			// before the term start is a past lecture, not a failure.
			if row.Filename == "x_2026-08-01.mp4" && (row.Status != calendar.StatusOK || row.ErrorInfo != nil || !row.Past ||
				row.RecordedAt == nil || *row.RecordedAt != "2026-08-01T00:00") {
				t.Errorf("past sample row %+v", row)
			}
		}
	}
	// calendar.before_term stays in the catalog while calendar.Validate (the
	// rule of before lecture kinds) exists, but the API's preview uses
	// calendar.ValidateRule, which never produces it.
	for _, code := range calendar.ErrorCodes {
		if !seen[code] && code != calendar.CodeTimeUnknown && code != calendar.CodeBeforeTerm {
			t.Errorf("code %s not exercised", code)
		}
	}
	if seen[calendar.CodeBeforeTerm] {
		t.Errorf("the preview reported %s", calendar.CodeBeforeTerm)
	}
}

func TestFilenamePreviewCoded(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	base := "/api/courses/" + c.ID + "/filename-rule"
	e.uploadNamed(c.ID, "CSC1003_2025-10-06_1000.mp4")
	e.uploadNamed(c.ID, "CSC1003_2025-13-08_1400.mp4")
	e.uploadNamed(c.ID, "random.mp4")

	var pr FilenamePreviewResult
	e.call("POST", base+"/preview", FilenamePreviewInput{Regex: testRule}, http.StatusOK, &pr)
	if pr.OK || len(pr.ProblemsInfo) != 1 || len(pr.Problems) != 1 {
		t.Fatalf("preview %+v", pr)
	}
	p := pr.ProblemsInfo[0]
	reason, _ := p.Params["reason"].(map[string]any) // decoded JSON
	if p.Code != calendar.CodeFileInvalid || p.Params["filename"] != "CSC1003_2025-13-08_1400.mp4" || p.Detail != pr.Problems[0] ||
		reason == nil || reason["code"] != calendar.CodeMonthUnknown || reason["params"].(map[string]any)["value"] != "13" {
		t.Fatalf("problems_info %+v", pr.ProblemsInfo)
	}
	rows := map[string]FilenamePreviewRow{}
	for _, r := range pr.Preview {
		rows[r.Filename] = r
	}
	if r := rows["CSC1003_2025-10-06_1000.mp4"]; r.ErrorInfo != nil || r.Error != "" {
		t.Fatalf("ok row %+v", r)
	}
	if r := rows["CSC1003_2025-13-08_1400.mp4"]; r.Status != calendar.StatusInvalid || r.ErrorInfo == nil ||
		r.ErrorInfo.Code != calendar.CodeMonthUnknown || r.ErrorInfo.Detail != r.Error || r.ErrorInfo.Params["value"] != "13" {
		t.Fatalf("invalid row %+v (%+v)", r, r.ErrorInfo)
	}
	if r := rows["random.mp4"]; r.Status != calendar.StatusUnmatched || r.ErrorInfo == nil || r.ErrorInfo.Code != calendar.CodeNoMatch {
		t.Fatalf("unmatched row %+v", r)
	}

	// A rule that does not compile: one problem, every row says why.
	e.call("POST", base+"/preview", FilenamePreviewInput{Regex: "("}, http.StatusOK, &pr)
	if pr.OK || len(pr.ProblemsInfo) != 1 || pr.ProblemsInfo[0].Code != calendar.CodeRegexInvalid || pr.ProblemsInfo[0].Params["error"] == "" {
		t.Fatalf("broken rule %+v", pr)
	}
	for _, r := range pr.Preview {
		if r.ErrorInfo == nil || r.ErrorInfo.Code != calendar.CodeRegexInvalid {
			t.Fatalf("broken rule row %+v", r)
		}
	}
	e.call("POST", base+"/preview", FilenamePreviewInput{Regex: `(?P<mon>\d+)`}, http.StatusOK, &pr)
	if len(pr.ProblemsInfo) != 1 || pr.ProblemsInfo[0].Code != calendar.CodeRegexGroupsMissing ||
		!slices.Equal(anyStrings(pr.ProblemsInfo[0].Params["groups"]), []string{"month", "day"}) {
		t.Fatalf("missing groups %+v", pr.ProblemsInfo)
	}

	// The saved rule's view carries the codes too.
	e.call("PUT", base, FilenamePreviewInput{Regex: testRule}, http.StatusOK, nil)
	var v FilenameRuleView
	e.call("GET", base, nil, http.StatusOK, &v)
	for _, r := range v.Preview {
		if (r.ErrorInfo == nil) != (r.Error == "") {
			t.Fatalf("view row %+v", r)
		}
	}
}

// anyStrings is a decoded JSON array of strings.
func anyStrings(v any) []string {
	xs, _ := v.([]any)
	out := []string{}
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
