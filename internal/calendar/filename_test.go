// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// csc3631Rule is the rule a model would write for the lecture-capture names
// of CSC1003 ("CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4"). The L01/L02
// part is a slot code, not a lecture number, so it is not captured.
const csc3631Rule = `(?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3,4} (?P<year>\d{4})`

// csc3631Samples are the CSC1003 lecture files (real names) with plausible
// upload and media creation times, plus one file from elsewhere.
func csc3631Samples() []Sample {
	up := at(2026, 10, 20, 21, 10)
	return []Sample{
		{Filename: "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4", UploadedAt: up},
		// creation_time is the download time: same day as the upload.
		{Filename: "CSC1003-L01-01 Wed Oct 14 09-35-00 BST 2026.mp4", UploadedAt: up, CreationTime: tp(at(2026, 10, 20, 20, 58))},
		// creation_time is the capture system's encoding time.
		{Filename: "CSC1003-L01-01 Wed Sep 30 09-35-00 BST 2026.mp4", UploadedAt: up, CreationTime: tp(at(2026, 9, 30, 10, 41))},
		{Filename: "CSC1003-L02-01 Tue Sep 29 12-35-00 BST 2026.mp4", UploadedAt: up, CreationTime: tp(at(2026, 9, 29, 13, 44))},
		{Filename: "lecture recording (1).mp4", UploadedAt: up, CreationTime: tp(at(2026, 10, 2, 11, 0))},
	}
}

// The whole pure pipeline on CSC1003: validate the rule (preview table),
// resolve each lecture's recording time, number the lectures.
func TestCSC1003Golden(t *testing.T) {
	samples := csc3631Samples()
	termStart := day(2026, 9, 28)
	parsed, matched, ok, problems := Validate(csc3631Rule, samples, &termStart)
	if !ok || matched != 4 || len(problems) != 0 {
		t.Fatalf("Validate: ok=%v matched=%d problems=%v", ok, matched, problems)
	}
	var b strings.Builder
	b.WriteString(renderValidate(parsed, matched, ok, problems))

	lectures := make([]LectureTime, len(samples))
	b.WriteString("\nrecorded_at:\n")
	for i, s := range samples {
		rec, src := ResolveRecordedAt(nil, parsed[i].RecordedAt, s.CreationTime, s.UploadedAt)
		fmt.Fprintf(&b, "  %-8s %s  %s\n", src, fmtTime(rec), s.Filename)
		lectures[i] = LectureTime{ID: fmt.Sprintf("L%d", i), RecordedAt: rec, OriginalFilename: s.Filename}
	}
	nums := LabelAll(lectures, &termStart, nil)
	b.WriteString("\nlectures:\n")
	b.WriteString(renderNumbered(lectures, nums))
	golden(t, "csc3631.golden", b.String())

	// The lecture-capture slot (L01, L02) is not the order: L02 on Tuesday
	// is the first lecture of week 1. The unmatched file is numbered by its
	// creation_time (Friday 2 October).
	want := []string{"W02L1", "W03L1", "W01L2", "W01L1", "W01L3"}
	for i, n := range nums {
		if n.Label != want[i] {
			t.Errorf("%s: %s, want %s", samples[i].Filename, n.Label, want[i])
		}
	}
	// The unmatched file falls back to its creation_time (an earlier day).
	if rec, src := ResolveRecordedAt(nil, parsed[4].RecordedAt, samples[4].CreationTime, samples[4].UploadedAt); src != SourceMetadata || !rec.Equal(at(2026, 10, 2, 11, 0)) {
		t.Errorf("unmatched file: %v %s, want metadata", rec, src)
	}
}

func TestValidateFailures(t *testing.T) {
	up := at(2026, 10, 20, 21, 10)
	s := func(names ...string) []Sample {
		out := make([]Sample, len(names))
		for i, n := range names {
			out[i] = Sample{Filename: n, UploadedAt: up}
		}
		return out
	}
	termStart := day(2026, 9, 28)
	l01 := "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4"
	l01b := "CSC1003-L01-01 Wed Oct 14 09-35-00 BST 2026.mp4"
	l01c := "CSC1003-L01-01 Wed Sep 30 09-35-00 BST 2026.mp4"
	l02 := "CSC1003-L02-01 Tue Sep 29 12-35-00 BST 2026.mp4"
	onlyL01 := `L01-01 [A-Z][a-z]{2} ` + csc3631Rule

	cases := []struct {
		name      string
		regex     string
		samples   []Sample
		termStart *time.Time
		ok        bool
		matched   int
		problem   string // substring of the first problem
	}{
		{"lookbehind", `(?<=CSC)(?P<month>[A-Z][a-z]{2}) (?P<day>\d{2})`, s(l01), nil, false, 0, "无法编译"},
		{"backreference", `(?P<month>[A-Z][a-z]{2}) (?P<day>\d{2})\1`, s(l01), nil, false, 0, "无法编译"},
		{"unbalanced", `(?P<month>[A-Z][a-z]{2} (?P<day>\d{2})`, s(l01), nil, false, 0, "无法编译"},
		{"no day group", `(?P<month>[A-Z][a-z]{2}) \d{2}`, s(l01), nil, false, 0, "缺少命名分组 day"},
		{"no groups", `(?P<mon>[A-Z][a-z]{2}) (?P<d>\d{2})`, s(l01), nil, false, 0, "缺少命名分组 month、day"},
		{"no samples", csc3631Rule, nil, nil, false, 0, "没有可用于校验的文件名"},
		{"match rate 50%", onlyL01, s(l01, l01b, l02, "intro.mp4"), nil, false, 2, "只匹配了 2 / 4 个文件名，少于 60%"},
		{"match rate 60%", onlyL01, s(l01, l01b, l01c, l02, "intro.mp4"), nil, true, 3, ""},
		{"no match", csc3631Rule, s("intro.mp4", "outro.mp4"), nil, false, 0, "只匹配了 0 / 2"},
		{"30 February", csc3631Rule, s(l01, "CSC1003-L01-01 Mon Feb 30 09-35-00 GMT 2026.mp4"), nil, false, 2, "日期不合法：2026-02-30"},
		{"31 September", csc3631Rule, s(l01, "CSC1003-L01-01 Thu Sep 31 09-35-00 BST 2026.mp4"), nil, false, 2, "日期不合法：2026-09-31"},
		{"month 13", `_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`, s("CSC1003_2026-10-07.mp4", "CSC1003_2026-13-01.mp4"), nil, false, 2, "无法识别月份“13”"},
		{"unknown month name", csc3631Rule, s(l01, "CSC1003-L01-01 Wed Okt 07 09-35-00 BST 2026.mp4"), nil, false, 2, "无法识别月份“Okt”"},
		{"hour 25", csc3631Rule, s(l01, "CSC1003-L01-01 Wed Oct 07 25-35-00 BST 2026.mp4"), nil, false, 2, "时间不合法：25:35"},
		{"after upload", csc3631Rule, s(l01, "CSC1003-L01-01 Wed Oct 21 09-35-00 BST 2026.mp4"), nil, false, 2, "解析出的日期 2026-10-21 晚于上传时间 2026-10-20"},
		{"wrong year is after upload", csc3631Rule, s(l01, "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2027.mp4"), nil, false, 2, "晚于上传时间"},
		// The upload date is compared by day: a recording later on the same
		// day (the capture system may be in another time zone) is accepted.
		{"same day as upload", csc3631Rule, s(l01, "CSC1003-L01-01 Tue Oct 20 22-35-00 BST 2026.mp4"), nil, true, 2, ""},
		{"31 days before term", csc3631Rule, s(l01, "CSC1003-L01-01 Fri Aug 28 09-35-00 BST 2026.mp4"), &termStart, false, 2, "解析出的日期 2026-08-28 早于开课日期 2026-09-28 超过 30 天"},
		{"30 days before term", csc3631Rule, s(l01, "CSC1003-L01-01 Sat Aug 29 09-35-00 BST 2026.mp4"), &termStart, true, 2, ""},
		{"before term, no term start", csc3631Rule, s(l01, "CSC1003-L01-01 Fri Aug 28 09-35-00 BST 2026.mp4"), nil, true, 2, ""},
		// A term start given as a Wednesday is aligned to its Monday first.
		{"term start aligned", csc3631Rule, s(l01, "CSC1003-L01-01 Sat Aug 29 09-35-00 BST 2026.mp4"), tp(day(2026, 9, 30)), true, 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, matched, ok, problems := Validate(c.regex, c.samples, c.termStart)
			if ok != c.ok || matched != c.matched {
				t.Errorf("ok=%v matched=%d, want ok=%v matched=%d (problems %v)", ok, matched, c.ok, c.matched, problems)
			}
			if len(parsed) != len(c.samples) {
				t.Errorf("%d preview rows for %d samples", len(parsed), len(c.samples))
			}
			if c.ok && len(problems) != 0 {
				t.Errorf("ok with problems %v", problems)
			}
			if !c.ok && (len(problems) == 0 || !strings.Contains(problems[0].Text, c.problem)) {
				t.Errorf("problems %q, want the first to contain %q", problems, c.problem)
			}
			for _, p := range parsed {
				if p.RecordedAt == nil && p.Error == "" {
					t.Errorf("%s: no date and no error", p.Filename)
				}
				if p.RecordedAt != nil && p.Error != "" {
					t.Errorf("%s: date %v with error %q", p.Filename, p.RecordedAt, p.Error)
				}
				checkRowProblem(t, p)
			}
			for _, pr := range problems {
				checkProblem(t, pr)
			}
		})
	}
}

// checkRowProblem: a row has a coded Problem exactly when it has an error,
// with the same text.
func checkRowProblem(t *testing.T, p Parsed) {
	t.Helper()
	switch {
	case (p.Problem == nil) != (p.Error == ""):
		t.Errorf("%s: error %q with problem %+v", p.Filename, p.Error, p.Problem)
	case p.Problem != nil:
		if p.Problem.Text != p.Error {
			t.Errorf("%s: problem text %q, error %q", p.Filename, p.Problem.Text, p.Error)
		}
		checkProblem(t, *p.Problem)
	}
}

// checkProblem: a listed code with a text; a file_invalid row carries its
// row's reason.
func checkProblem(t *testing.T, pr Problem) {
	t.Helper()
	if !slices.Contains(ErrorCodes, pr.Code) || pr.Text == "" {
		t.Errorf("problem %+v: unknown code or no text", pr)
	}
	if pr.Code == CodeFileInvalid {
		r, ok := pr.Params["reason"].(Problem)
		if !ok || r.Code == "" || r.Code == CodeFileInvalid || pr.Text != pr.Params["filename"].(string)+"："+r.Text {
			t.Errorf("file_invalid %+v: reason %+v", pr, pr.Params["reason"])
		}
	}
}

// The coded problems (SPEC-v2 P14.3): code and params of each reason; the
// Chinese text is unchanged (TestValidateFailures).
func TestValidateProblemCodes(t *testing.T) {
	up := at(2026, 10, 20, 21, 10)
	s := func(names ...string) []Sample {
		out := make([]Sample, len(names))
		for i, n := range names {
			out[i] = Sample{Filename: n, UploadedAt: up}
		}
		return out
	}
	termStart := day(2026, 9, 28)
	l01 := "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4"
	onlyL01 := `L01-01 [A-Z][a-z]{2} ` + csc3631Rule
	ampm := `(?P<month>\d{2})-(?P<day>\d{2}) (?P<hour>\d{1,2})(?P<ampm>[ap]m)`
	cases := []struct {
		name    string
		regex   string
		samples []Sample
		term    *time.Time
		code    string         // the first problem
		params  map[string]any // of the first problem, or of its row's reason
	}{
		{"lookbehind", `(?<=CSC)(?P<month>[A-Z][a-z]{2}) (?P<day>\d{2})`, s(l01), nil, CodeRegexInvalid, nil},
		{"no groups", `(?P<mon>[A-Z][a-z]{2}) (?P<d>\d{2})`, s(l01), nil, CodeRegexGroupsMissing, map[string]any{"groups": []string{"month", "day"}}},
		{"no samples", csc3631Rule, nil, nil, CodeNoSamples, nil},
		{"low match", onlyL01, s(l01, "intro.mp4"), nil, CodeLowMatch, map[string]any{"matched": 1, "total": 2, "percent": MinMatchPercent}},
		{"30 February", csc3631Rule, s("CSC1003-L01-01 Mon Feb 30 09-35-00 GMT 2026.mp4"), nil, CodeDateInvalid, map[string]any{"date": "2026-02-30"}},
		{"month 13", `_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`, s("CSC1003_2026-13-01.mp4"), nil, CodeMonthUnknown, map[string]any{"value": "13"}},
		{"day 32", `_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`, s("CSC1003_2026-10-32.mp4"), nil, CodeDayUnknown, map[string]any{"value": "32"}},
		{"year 1890", `_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`, s("CSC1003_1890-10-02.mp4"), nil, CodeYearUnknown, map[string]any{"value": "1890"}},
		{"hour 25", csc3631Rule, s("CSC1003-L01-01 Wed Oct 07 25-35-00 BST 2026.mp4"), nil, CodeTimeInvalid, map[string]any{"time": "25:35"}},
		{"13 pm", ampm, s("10-07 13pm.mp4"), nil, CodeTimeInvalid, map[string]any{"time": "13:00 pm"}},
		{"after upload", csc3631Rule, s("CSC1003-L01-01 Wed Oct 21 09-35-00 BST 2026.mp4"), nil, CodeAfterUpload, map[string]any{"date": "2026-10-21", "uploaded": "2026-10-20"}},
		{"before term", csc3631Rule, s("CSC1003-L01-01 Fri Aug 28 09-35-00 BST 2026.mp4"), &termStart, CodeBeforeTerm, map[string]any{"date": "2026-08-28", "term_start": "2026-09-28", "days": MaxDaysBeforeTerm}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, _, ok, problems := Validate(c.regex, c.samples, c.term)
			if ok || len(problems) == 0 {
				t.Fatalf("ok=%v problems=%v", ok, problems)
			}
			first := problems[0]
			checkProblem(t, first)
			got := first
			if first.Code == CodeFileInvalid {
				got = first.Params["reason"].(Problem)
				if first.Params["filename"] != c.samples[0].Filename {
					t.Errorf("file_invalid filename %v", first.Params["filename"])
				}
			}
			if got.Code != c.code {
				t.Fatalf("code %s (%s), want %s", got.Code, got.Text, c.code)
			}
			if c.params != nil && !reflect.DeepEqual(got.Params, c.params) {
				t.Errorf("params %#v, want %#v", got.Params, c.params)
			}
			for _, p := range parsed {
				checkRowProblem(t, p)
			}
		})
	}

	// An unmatched row says so; the rule still compiles.
	p := Parse(FilenameRule{Regex: csc3631Rule}, Sample{Filename: "intro.mp4", UploadedAt: up})
	if p.Problem == nil || p.Problem.Code != CodeNoMatch || p.Error != "文件名与规则不匹配" {
		t.Errorf("unmatched row: %+v", p)
	}
	// Compile's error is the Problem; Failed keeps it.
	_, err := Compile(FilenameRule{Regex: `(`})
	if pr, ok := err.(*Problem); !ok || pr.Code != CodeRegexInvalid || pr.Params["error"] == "" {
		t.Errorf("Compile error %#v", err)
	}
	if f := Failed("a.mp4", err); f.Problem == nil || f.Problem.Code != CodeRegexInvalid || f.Error != err.Error() || f.Status() != StatusUnmatched {
		t.Errorf("Failed: %+v", f)
	}
}

// A file whose date differs from its creation_time by more than a year is
// marked suspicious but does not fail the rule (P13.3.2 rule 5).
func TestValidateSuspicious(t *testing.T) {
	up := at(2026, 10, 20, 21, 10)
	samples := []Sample{
		{Filename: "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4", UploadedAt: up, CreationTime: tp(at(2024, 10, 7, 9, 0))},
		{Filename: "CSC1003-L01-01 Wed Oct 14 09-35-00 BST 2026.mp4", UploadedAt: up, CreationTime: tp(at(2025, 10, 15, 9, 0))}, // within a year
		{Filename: "CSC1003-L01-01 Wed Sep 30 09-35-00 BST 2026.mp4", UploadedAt: up},
	}
	parsed, matched, ok, problems := Validate(csc3631Rule, samples, nil)
	if !ok || matched != 3 || len(problems) != 0 {
		t.Fatalf("ok=%v matched=%d problems=%v", ok, matched, problems)
	}
	want := []string{StatusSuspicious, StatusOK, StatusOK}
	for i, p := range parsed {
		if p.Status() != want[i] || p.RecordedAt == nil || p.Error != "" {
			t.Errorf("%s: status %s (%+v), want %s", p.Filename, p.Status(), p, want[i])
		}
	}
	golden(t, "validate_suspicious.golden", renderValidate(parsed, matched, ok, problems))
}

// Other naming schemes: no year, month names, 12-hour times, titles,
// two-digit years, a year default, alternatives with the same group names.
func TestParseFormatsGolden(t *testing.T) {
	y2025 := 2025
	type row struct {
		name     string
		uploaded time.Time
		creation *time.Time
	}
	rules := []struct {
		rule FilenameRule
		rows []row
	}{
		{FilenameRule{Regex: `^COMP2001 (?P<day>\d{2}) (?P<month>[A-Za-z]{3}) (?P<hour>\d{2})(?P<minute>\d{2})\.mp4$`}, []row{
			{"COMP2001 07 Oct 0935.mp4", at(2026, 10, 20, 21, 0), nil},
			{"COMP2001 15 Dec 1400.mp4", at(2027, 1, 5, 8, 0), nil},    // December is after the upload → the year before
			{"COMP2001 05 Jan 1000.mp4", at(2027, 1, 5, 8, 0), nil},    // same day as the upload → upload year
			{"COMP2001 06 Jan 1000.mp4", at(2027, 1, 5, 8, 0), nil},    // the day after → the year before
			{"COMP2001 29 Feb 1000.mp4", at(2028, 3, 10, 8, 0), nil},   // leap year
			{"COMP2001 29 Feb 1000.mp4", at(2027, 3, 10, 8, 0), nil},   // not a leap year
			{"COMP2001 07 OCT 0935.mp4", at(2026, 10, 20, 21, 0), nil}, // month in capitals
			{"COMP2001 7 Oct 0935.mp4", at(2026, 10, 20, 21, 0), nil},  // does not match
		}},
		{FilenameRule{Regex: `^(?P<title>[A-Za-z_ ]+?) - (?P<month>[A-Za-z]+\.?) (?P<day>\d{1,2}(?:st|nd|rd|th)?),? (?P<year>\d{4}) (?P<hour>\d{1,2}):(?P<minute>\d{2}) ?(?P<ampm>[AaPp]\.?[Mm]\.?)\.mp4$`}, []row{
			{"Block_Ciphers - October 7th, 2026 9:35 AM.mp4", at(2026, 10, 20, 21, 0), nil},
			{"Stream  Ciphers - Sept. 30th, 2026 2:05 pm.mp4", at(2026, 10, 20, 21, 0), nil},
			{"Hash Functions - October 14, 2026 12:10 PM.mp4", at(2026, 10, 20, 21, 0), nil},
			{"Late Lab - October 13, 2026 12:30 a.m..mp4", at(2026, 10, 20, 21, 0), nil},
			{"Bad Time - October 14, 2026 13:10 PM.mp4", at(2026, 10, 20, 21, 0), nil},
			{"Old Notes - May 2nd, 2024 10:00 am.mp4", at(2026, 10, 20, 21, 0), tp(at(2026, 10, 1, 9, 0))}, // suspicious
		}},
		{FilenameRule{Regex: `^Lec_(?P<day>\d{2})(?P<month>\d{2})(?P<year>\d{2})\.mp4$`}, []row{
			{"Lec_071026.mp4", at(2026, 10, 20, 21, 0), nil}, // date only
			{"Lec_311026.mp4", at(2026, 11, 2, 21, 0), nil},
			{"Lec_310926.mp4", at(2026, 10, 20, 21, 0), nil}, // 31 September
		}},
		{FilenameRule{Regex: `^W\d+ (?P<month>\d{1,2})-(?P<day>\d{1,2})\.mp4$`, YearDefault: &y2025}, []row{
			{"W3 10-7.mp4", at(2026, 10, 20, 21, 0), nil}, // the year default wins over the upload year
			{"notes.mp4", at(2026, 10, 20, 21, 0), nil},
		}},
		{FilenameRule{Regex: `(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})|(?P<day>\d{1,2})\.(?P<month>\d{1,2})\.(?P<year>\d{4})`}, []row{
			{"Lec 2026-10-07.mp4", at(2026, 10, 20, 21, 0), nil},
			{"Lec 7.10.2026.mp4", at(2026, 10, 20, 21, 0), nil},
		}},
	}
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "## %s", r.rule.Regex)
		if r.rule.YearDefault != nil {
			fmt.Fprintf(&b, " (year_default %d)", *r.rule.YearDefault)
		}
		b.WriteString("\n")
		for _, x := range r.rows {
			p := Parse(r.rule, Sample{Filename: x.name, UploadedAt: x.uploaded, CreationTime: x.creation})
			fmt.Fprintf(&b, "uploaded %s\n", fmtTime(&x.uploaded))
			b.WriteString(renderParsed([]Parsed{p}))
		}
		b.WriteString("\n")
	}
	golden(t, "parse_formats.golden", b.String())
}

// Spot checks of the formats golden, so a careless -update cannot hide a
// regression.
func TestParseFormats(t *testing.T) {
	cases := []struct {
		regex    string
		name     string
		uploaded time.Time
		want     time.Time
		hasTime  bool
		title    string
	}{
		{`^COMP2001 (?P<day>\d{2}) (?P<month>[A-Za-z]{3}) (?P<hour>\d{2})(?P<minute>\d{2})\.mp4$`,
			"COMP2001 15 Dec 1400.mp4", at(2027, 1, 5, 8, 0), at(2026, 12, 15, 14, 0), true, ""},
		{`^COMP2001 (?P<day>\d{2}) (?P<month>[A-Za-z]{3}) (?P<hour>\d{2})(?P<minute>\d{2})\.mp4$`,
			"COMP2001 05 Jan 1000.mp4", at(2027, 1, 5, 8, 0), at(2027, 1, 5, 10, 0), true, ""},
		{`^(?P<title>.+?) - (?P<month>[A-Za-z]+\.?) (?P<day>\d+(?:st|nd|rd|th)?),? (?P<year>\d{4}) (?P<hour>\d+):(?P<minute>\d{2}) ?(?P<ampm>[AaPp]\.?[Mm]\.?)\.mp4$`,
			"Stream  Ciphers - Sept. 30th, 2026 2:05 pm.mp4", at(2026, 10, 20, 21, 0), at(2026, 9, 30, 14, 5), true, "Stream Ciphers"},
		{`^(?P<title>.+?) - (?P<month>[A-Za-z]+) (?P<day>\d+),? (?P<year>\d{4}) (?P<hour>\d+):(?P<minute>\d{2}) ?(?P<ampm>[AaPp]\.?[Mm]\.?)\.mp4$`,
			"Late Lab - October 13, 2026 12:30 a.m..mp4", at(2026, 10, 20, 21, 0), at(2026, 10, 13, 0, 30), true, "Late Lab"},
		{`^(?P<title>.+?) - (?P<month>[A-Za-z]+) (?P<day>\d+),? (?P<year>\d{4}) (?P<hour>\d+):(?P<minute>\d{2}) ?(?P<ampm>[AaPp]\.?[Mm]\.?)\.mp4$`,
			"Noon - October 13, 2026 12:00 PM.mp4", at(2026, 10, 20, 21, 0), at(2026, 10, 13, 12, 0), true, "Noon"},
		{`^Lec_(?P<day>\d{2})(?P<month>\d{2})(?P<year>\d{2})\.mp4$`,
			"Lec_071026.mp4", at(2026, 10, 20, 21, 0), day(2026, 10, 7), false, ""},
		{`(?P<month>\d+)/(?P<day>\d+) (?P<hour>\d+)h`, // hour without minute
			"10/7 9h.mp4", at(2026, 10, 20, 21, 0), at(2026, 10, 7, 9, 0), true, ""},
		{`(?P<title>_+)(?P<month>\d+)-(?P<day>\d+)`, // a title of separators only
			"__10-7.mp4", at(2026, 10, 20, 21, 0), day(2026, 10, 7), false, ""},
	}
	for _, c := range cases {
		p := Parse(FilenameRule{Regex: c.regex}, Sample{Filename: c.name, UploadedAt: c.uploaded})
		if p.RecordedAt == nil || !p.RecordedAt.Equal(c.want) || p.HasTime != c.hasTime || p.Title != c.title || p.Status() != StatusOK {
			t.Errorf("%s: %+v (at %s), want %v hasTime=%v title=%q", c.name, p, fmtTime(p.RecordedAt), c.want, c.hasTime, c.title)
		}
		if p.RecordedAt != nil && p.RecordedAt.Location() != c.uploaded.Location() {
			t.Errorf("%s: location %v, want the upload's", c.name, p.RecordedAt.Location())
		}
	}

	// A rule that does not compile gives an unmatched row with the reason.
	p := Parse(FilenameRule{Regex: `(?<=x)(?P<month>\d+)-(?P<day>\d+)`}, Sample{Filename: "x10-7.mp4"})
	if p.Matched || p.RecordedAt != nil || !strings.Contains(p.Error, "无法编译") || p.Status() != StatusUnmatched {
		t.Errorf("broken rule: %+v", p)
	}
	if _, err := Compile(FilenameRule{Regex: csc3631Rule}); err != nil {
		t.Errorf("Compile: %v", err)
	}
}

func TestResolveRecordedAt(t *testing.T) {
	up := at(2026, 10, 20, 21, 10)
	user := tp(at(2026, 10, 8, 10, 0))
	file := tp(at(2026, 10, 7, 9, 35))
	early := tp(at(2026, 10, 1, 9, 0))
	utc := time.UTC
	cases := []struct {
		name               string
		user, file, create *time.Time
		want               *time.Time
		source             string
	}{
		{"user first", user, file, early, user, SourceUser},
		{"then filename", nil, file, early, file, SourceFilename},
		{"then metadata", nil, nil, early, early, SourceMetadata},
		{"metadata on the upload day is a download time", nil, nil, tp(at(2026, 10, 20, 8, 0)), nil, SourceNone},
		{"metadata after the upload", nil, nil, tp(at(2026, 10, 22, 8, 0)), nil, SourceNone},
		// creation_time from the media is UTC; the day is the local one.
		{"UTC metadata on the local upload day", nil, nil, tp(time.Date(2026, 10, 19, 23, 30, 0, 0, utc)), nil, SourceNone},
		{"UTC metadata the local day before", nil, nil, tp(time.Date(2026, 10, 19, 22, 30, 0, 0, utc)), tp(at(2026, 10, 19, 23, 30)), SourceMetadata},
		{"zero times count as missing", &time.Time{}, &time.Time{}, &time.Time{}, nil, SourceNone},
		{"none", nil, nil, nil, nil, SourceNone},
	}
	for _, c := range cases {
		got, src := ResolveRecordedAt(c.user, c.file, c.create, up)
		if src != c.source || (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: (%v, %s), want (%v, %s)", c.name, got, src, c.want, c.source)
			continue
		}
		if src == SourceMetadata && (got.Location() != bst || got.Format("15:04") != c.want.Format("15:04")) {
			t.Errorf("%s: %v, want local wall-clock time in %v", c.name, got, bst)
		}
		if got != nil && (got == c.user || got == c.file || got == c.create) {
			t.Errorf("%s: returned the caller's pointer", c.name)
		}
	}
}
