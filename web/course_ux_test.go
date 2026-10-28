// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// The course page requests of 2026-10-27 (course-ux): the batch rerun, the
// week overrides (supplements, make-ups). Every text the new code uses
// exists in zh-CN, en and context — in the catalogs, or (until the lead
// merges it) in web/i18n/_parts/course-ux; the requests are the server's
// routes; the stage list is the pipeline's; the "补" tag has the warn
// colour of the state tags.

// catalogWithCourseUX is the catalog of locale with the course-ux part laid
// over it (absent part: the catalog alone).
func catalogWithCourseUX(t *testing.T, locale string) catalog {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, locale+".json"))
	part := filepath.Join(catalogDir, "_parts", "course-ux."+locale+".json")
	if _, err := os.Stat(part); err == nil {
		for k, v := range loadCatalog(t, part) {
			c[k] = v
		}
	}
	return c
}

// courseUXCodes are the error codes of internal/api/course_ux.go in
// testdata/api/error_codes.json, with their params.
func courseUXCodes(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "api", "error_codes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]struct {
		Params []string `json:"params"`
	}
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for code, d := range all {
		if strings.HasPrefix(code, "rerun.") || strings.HasPrefix(code, "week_override.") {
			out[code] = d.Params
		}
	}
	if len(out) != 8 {
		t.Fatalf("%d course-ux codes in error_codes.json: %v", len(out), out)
	}
	return out
}

func TestCourseUXTexts(t *testing.T) {
	zh, en, ctx := catalogWithCourseUX(t, "zh-CN"), catalogWithCourseUX(t, "en"), catalogWithCourseUX(t, "context")
	keys := literalKeys(t, "course_ux.js")
	for _, k := range literalKeys(t, "lecture_list.js") {
		if strings.HasPrefix(k, "course.week.") || strings.HasPrefix(k, "course.lectures.batch.rerun") || strings.HasPrefix(k, "course.lectures.batch.select_") {
			keys = append(keys, k)
		}
	}
	codes := courseUXCodes(t)
	for code := range codes {
		keys = append(keys, "errors."+code)
	}
	if len(keys) < 45 {
		t.Fatalf("only %d keys found", len(keys))
	}
	for _, k := range keys {
		zv, zok := zh[k]
		ev, eok := en[k]
		if !zok || !eok {
			t.Errorf("%s: missing text (zh-CN %v, en %v)", k, zok, eok)
			continue
		}
		zp, err1 := paramSets(zv)
		ep, err2 := paramSets(ev)
		if err1 != nil || err2 != nil {
			t.Errorf("%s: %v %v", k, err1, err2)
			continue
		}
		want := strings.Join(zp[""], ",")
		for form, ps := range ep {
			if got := strings.Join(ps, ","); got != want {
				t.Errorf("%s: en %s has {%s}, zh-CN {%s}", k, form, got, want)
			}
		}
		// the shared keys only need to exist; the new ones explain themselves
		shared := strings.HasPrefix(k, "common.") || strings.HasPrefix(k, "kind.") || strings.HasPrefix(k, "calendar.")
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || (c["notes"] == nil && !shared) {
			t.Errorf("%s: no context entry with where and notes", k)
		}
		// an error text uses only the params the back end sends
		if code, isErr := strings.CutPrefix(k, "errors."); isErr {
			if ps, ok := codes[code]; ok {
				for _, p := range zp[""] {
					if !slices.Contains(ps, p) {
						t.Errorf("%s: {%s} is not a param of the code (%v)", k, p, ps)
					}
				}
			}
		}
	}
	// the tag is one character in Chinese (a small tag, the user's "补")
	if s := text(t, zh, "course.week.tag"); s != "补" {
		t.Errorf("course.week.tag = %q", s)
	}
}

// Every request of course_ux.js is a route of internal/api/course_ux.go and
// of the mock server.
func TestCourseUXRoutes(t *testing.T) {
	read := func(p ...string) string {
		b, err := os.ReadFile(filepath.Join(p...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	server := read("..", "internal", "api", "course_ux.go")
	mock := read("..", "internal", "api", "mockserve", "course_ux.go")
	apiJS := readWeb(t, "api.js")
	for name, route := range map[string]string{
		"rerunCourse":      "POST /api/courses/{id}/rerun",
		"setLectureWeek":   "PUT /api/lectures/{id}/week",
		"clearLectureWeek": "DELETE /api/lectures/{id}/week",
	} {
		if !strings.Contains(server, `h("`+route+`"`) {
			t.Errorf("the server has no %s", route)
		}
		if !strings.Contains(mock, `"`+route+`"`) {
			t.Errorf("the mock has no %s", route)
		}
		method, path, _ := strings.Cut(route, " ")
		fn := map[string]string{"POST": "post", "PUT": "put", "DELETE": "del"}[method]
		js := regexp.QuoteMeta(name+": ") + `[^\n]*\n?\s*` + regexp.QuoteMeta(fn+"(`") + `([^` + "`" + `]*)`
		m := regexp.MustCompile(js).FindStringSubmatch(apiJS)
		if m == nil {
			t.Errorf("api.js has no %s calling %s", name, fn)
			continue
		}
		got := regexp.MustCompile(`\$\{enc\([a-zA-Z]+\)\}`).ReplaceAllString(m[1], "{id}")
		got, _, _ = strings.Cut(got, "$") // a query part
		if got != path {
			t.Errorf("api.js %s: %s, want %s", name, got, path)
		}
	}
	if !strings.Contains(readWeb(t, "course_ux.js"), "api.rerunCourse(") || !strings.Contains(readWeb(t, "course_ux.js"), "api.setLectureWeek(") || !strings.Contains(readWeb(t, "course_ux.js"), "api.clearLectureWeek(") {
		t.Error("course_ux.js does not use the three requests")
	}
}

// The dialog's stages are the lecture pipeline's, in order; the results it
// reads are the server's.
func TestCourseUXStagesAndResults(t *testing.T) {
	src := readWeb(t, "course_ux.js")
	m := regexp.MustCompile(`const STAGES = \[([^\]]*)\];`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("course_ux.js has no STAGES")
	}
	var got []string
	for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
		got = append(got, q[1])
	}
	if !slices.Equal(got, model.LectureStages) {
		t.Fatalf("STAGES %v, pipeline %v", got, model.LectureStages)
	}
	types, err := os.ReadFile(filepath.Join("..", "internal", "api", "types_course_ux.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"queued", "skipped", "failed"} {
		if !strings.Contains(string(types), `= "`+r+`"`) {
			t.Errorf("types_course_ux.go has no result %q", r)
		}
	}
	for _, r := range []string{"'queued'", "'skipped'", "'failed'"} {
		if !strings.Contains(src, "r.result === "+r) {
			t.Errorf("course_ux.js does not read the result %s", r)
		}
	}
}

// The "补" tag uses the warn tokens of the state tags; the stylesheet has
// no colour of its own (the interface's blue accent and tokens only) and
// is loaded by the page.
func TestCourseUXStyles(t *testing.T) {
	css := readWeb(t, "course_ux.css")
	m := regexp.MustCompile(`\.chip-makeup \{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil || !strings.Contains(m[1], "var(--warn-soft)") || !strings.Contains(m[1], "var(--warn)") {
		t.Fatalf(".chip-makeup: %v", m)
	}
	if c := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(`).FindString(css); c != "" {
		t.Errorf("course_ux.css has its own colour %s; use the tokens of style.css", c)
	}
	if !strings.Contains(readWeb(t, "index.html"), `<link rel="stylesheet" href="course_ux.css">`) {
		t.Error("index.html does not load course_ux.css")
	}
}
