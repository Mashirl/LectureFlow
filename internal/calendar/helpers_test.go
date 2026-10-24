// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

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
		t.Fatalf("%v (run go test ./internal/calendar -update to create it)", err)
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs from golden file.\n--- got ---\n%s--- want ---\n%s", name, got, w)
	}
}

// bst is the lecture-capture system's summer time; tests use a fixed zone so
// they do not depend on the machine's time zone.
var bst = time.FixedZone("BST", 3600)

// at builds a local time in bst.
func at(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, bst)
}

// day builds a local date (00:00, i.e. no time of day) in bst.
func day(y int, m time.Month, d int) time.Time { return at(y, m, d, 0, 0) }

func tp(t time.Time) *time.Time { return &t }

// fmtTime renders a local time for golden files; a time of day of 00:00
// shows as --:-- (unknown).
func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	if !HasClock(*t) {
		return t.Format("2006-01-02 --:-- Mon")
	}
	return t.Format("2006-01-02 15:04 Mon")
}

// renderParsed renders a preview table.
func renderParsed(rows []Parsed) string {
	var b strings.Builder
	for _, p := range rows {
		fmt.Fprintf(&b, "%s\n  status=%s recorded_at=%s has_time=%v", p.Filename, p.Status(), fmtTime(p.RecordedAt), p.HasTime)
		if p.Title != "" {
			fmt.Fprintf(&b, " title=%q", p.Title)
		}
		if p.Error != "" {
			fmt.Fprintf(&b, "\n  error=%s", p.Error)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderValidate renders the full result of Validate.
func renderValidate(parsed []Parsed, matched int, ok bool, problems []Problem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ok=%v matched=%d/%d\n", ok, matched, len(parsed))
	for _, p := range problems {
		fmt.Fprintf(&b, "problem: %s\n", p)
	}
	b.WriteString("\n")
	b.WriteString(renderParsed(parsed))
	return b.String()
}

// renderNumbered renders LabelAll's result in list order (Order).
func renderNumbered(lectures []LectureTime, nums []Numbered) string {
	var b strings.Builder
	for seq, i := range Order(lectures) {
		n := nums[i]
		flag := ""
		if n.SameDayByName {
			flag = " 同日顺序按文件名推测"
		}
		fmt.Fprintf(&b, "%2d %-6s ok=%-5v w=%d n=%d %s  %s%s\n",
			seq+1, n.Label, n.OK, n.Week, n.N, fmtTime(lectures[i].RecordedAt), lectures[i].OriginalFilename, flag)
	}
	return b.String()
}

// TestPackageIsPure enforces SPEC-v2 P13.4: no I/O, clock or randomness in
// calendar (non-test files). time is imported for its types only.
func TestPackageIsPure(t *testing.T) {
	allowed := []string{"fmt", "regexp", "sort", "strconv", "strings", "time"}
	forbiddenCalls := []string{"time.Now", "time.Since", "time.Until", "time.Local", "time.LoadLocation", "time.Sleep", "time.After", "time.Tick", "time.NewTimer"}
	files, err := filepath.Glob("*.go")
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
			if !slices.Contains(allowed, p) {
				t.Errorf("%s imports %s (calendar must stay pure)", f, p)
			}
		}
		for _, c := range forbiddenCalls {
			if strings.Contains(string(src), c) {
				t.Errorf("%s uses %s (callers pass every time value)", f, c)
			}
		}
	}
}
