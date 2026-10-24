// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Named groups of a file-name rule (P13.3.1). month and day are required.
const (
	GroupYear   = "year"
	GroupMonth  = "month"
	GroupDay    = "day"
	GroupHour   = "hour"
	GroupMinute = "minute"
	GroupAMPM   = "ampm"
	GroupTitle  = "title"
	// GroupKind captures the lecture kind's code in the file name ("L",
	// "T01", "Tutorial"); FilenameRule.KindMap maps it to a kind id (lecture
	// kinds, lead ruling 1.2/1.3). Optional.
	GroupKind = "kind"
)

// FilenameRule.Source values (P13.3.1).
const (
	RuleSourceLLM  = "llm"
	RuleSourceUser = "user"
)

// lecture.recorded_at_source values (P13.2, P13.3.4).
const (
	SourceUser     = "user"
	SourceFilename = "filename"
	SourceMetadata = "metadata"
	SourceNone     = "none"
)

// Validation thresholds (P13.3.2).
const (
	MinMatchPercent = 60 // rule 2
	// MaxDaysBeforeTerm is rule 4's threshold. Since lecture kinds (lead
	// ruling 1.1) a sample dated more than this many days before the term
	// start is a past lecture (ValidateRule: Parsed.Past), not a failure;
	// EffectiveKind uses the same threshold.
	MaxDaysBeforeTerm = 30
)

// Parsed.Status values, for the preview table (P13.3.3: 成功 / 不匹配 / 可疑).
const (
	StatusOK         = "ok"
	StatusUnmatched  = "unmatched"
	StatusInvalid    = "invalid" // matched, but the date is unusable (see Error)
	StatusSuspicious = "suspicious"
)

// FilenameRule is course.filename_rule (P13.3.1).
type FilenameRule struct {
	Regex          string `json:"regex"`
	YearDefault    *int   `json:"year_default"`
	Source         string `json:"source"` // "llm" | "user"
	CreatedAt      string `json:"created_at"`
	SamplesMatched int    `json:"samples_matched"`
	SamplesTotal   int    `json:"samples_total"`
	// KindMap maps what the (?P<kind>…) group captures to a kind id
	// (lecture kinds; e.g. {"L": "lecture", "T": "tutorial"}). Keys match
	// exactly, else case-insensitively (LookupKind). Absent when empty, so
	// rules saved before keep their JSON.
	KindMap map[string]string `json:"kind_map,omitempty"`
}

// Sample is one lecture file: its original name, when it was uploaded and
// the media's creation_time if it has one (local times).
type Sample struct {
	Filename     string
	UploadedAt   time.Time
	CreationTime *time.Time
}

// Parsed is the result of applying a rule to one file name: one row of the
// preview table.
type Parsed struct {
	Filename   string
	Matched    bool
	RecordedAt *time.Time // local; nil when not matched or invalid
	HasTime    bool       // hour (and minute) groups matched
	Title      string     // the (?P<title>) group, trimmed; "" if none
	Suspicious bool       // differs from CreationTime by more than a year (P13.3.2 rule 5)
	Error      string     // why it failed, in Chinese (Problem.Text; "" = no failure)
	Problem    *Problem   // why it failed, coded (nil = no failure)

	// Lecture kinds (K1).
	KindCode string // the (?P<kind>) group, trimmed; "" if none
	Kind     string // KindCode through the rule's KindMap (LookupKind); "" when unmapped
	// Past: set by ValidateRule when the date is more than
	// MaxDaysBeforeTerm days before the term start (rule 4 since lecture
	// kinds): numbered as a past lecture (EffectiveKind), not a failure.
	Past bool
}

// Problem is a coded reason why a rule or a file name failed (SPEC-v2
// P14.3): the API sends it as an ErrorInfo (same JSON shape) and the front
// end shows t("errors."+Code, Params). Text is the Chinese wording of the
// same reason (the ErrorInfo detail, Parsed.Error, logs). Codes are listed
// in ErrorCodes; the HTTP layer's catalog (testdata/api/error_codes.json)
// has their texts and params. A Problem is also the error Compile returns.
type Problem struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	Text   string         `json:"detail,omitempty"`
}

func (p Problem) Error() string { return p.Text }

// Problem codes (P14.3). Params: regex_invalid {error: Go's message};
// regex_groups_missing {groups: []string}; month_unknown, day_unknown,
// year_unknown {value}; time_unknown, time_invalid {time}; date_invalid
// {date: YYYY-MM-DD}; after_upload {date, uploaded}; before_term {date,
// term_start, days} (only from the transitional Validate: ValidateRule
// numbers such samples as past lectures, Parsed.Past); low_match {matched,
// total, percent}; file_invalid {filename, reason: the row's Problem}.
const (
	CodeRegexInvalid       = "calendar.regex_invalid"
	CodeRegexGroupsMissing = "calendar.regex_groups_missing"
	CodeNoMatch            = "calendar.no_match"
	CodeMonthUnknown       = "calendar.month_unknown"
	CodeDayUnknown         = "calendar.day_unknown"
	CodeYearUnknown        = "calendar.year_unknown"
	CodeTimeUnknown        = "calendar.time_unknown"
	CodeTimeInvalid        = "calendar.time_invalid"
	CodeDateInvalid        = "calendar.date_invalid"
	CodeAfterUpload        = "calendar.after_upload"
	CodeBeforeTerm         = "calendar.before_term"
	CodeNoSamples          = "calendar.no_samples"
	CodeLowMatch           = "calendar.low_match"
	CodeFileInvalid        = "calendar.file_invalid"
)

// ErrorCodes lists every Problem code (the API checks its catalog has them).
var ErrorCodes = []string{
	CodeRegexInvalid, CodeRegexGroupsMissing, CodeNoMatch, CodeMonthUnknown, CodeDayUnknown,
	CodeYearUnknown, CodeTimeUnknown, CodeTimeInvalid, CodeDateInvalid, CodeAfterUpload,
	CodeBeforeTerm, CodeNoSamples, CodeLowMatch, CodeFileInvalid,
}

// problem builds a Problem.
func problem(code string, params map[string]any, text string) *Problem {
	return &Problem{Code: code, Params: params, Text: text}
}

// Failed is the preview row of a file name that could not be parsed
// because of err (a rule that does not compile: Compile's error).
func Failed(filename string, err error) Parsed {
	pr := asProblem(err)
	return Parsed{Filename: filename, Error: pr.Text, Problem: pr}
}

// asProblem is err as a Problem (Compile's errors are; anything else is
// reported as a regex that does not compile).
func asProblem(err error) *Problem {
	switch e := err.(type) {
	case *Problem:
		return e
	case Problem:
		return &e
	}
	return problem(CodeRegexInvalid, map[string]any{"error": err.Error()}, err.Error())
}

// fail sets the row's failure.
func (p *Parsed) fail(pr *Problem) {
	p.Problem, p.Error = pr, pr.Text
}

// Status classifies a row of the preview table.
func (p Parsed) Status() string {
	switch {
	case !p.Matched:
		return StatusUnmatched
	case p.RecordedAt == nil:
		return StatusInvalid
	case p.Suspicious:
		return StatusSuspicious
	}
	return StatusOK
}

// Rule is a compiled file-name rule.
type Rule struct {
	re          *regexp.Regexp
	names       []string
	yearDefault *int
	kindMap     map[string]string
}

// Compile compiles a rule (P13.3.2 rule 1): Go RE2 syntax with named groups
// month and day. The error is a *Problem (its text is Chinese).
func Compile(rule FilenameRule) (*Rule, error) {
	re, err := regexp.Compile(rule.Regex)
	if err != nil {
		return nil, problem(CodeRegexInvalid, map[string]any{"error": err.Error()},
			fmt.Sprintf("正则表达式无法编译（须为 Go RE2 语法，不支持后行断言和反向引用）：%v", err))
	}
	r := &Rule{re: re, names: re.SubexpNames(), yearDefault: rule.YearDefault, kindMap: rule.KindMap}
	var missing []string
	for _, g := range []string{GroupMonth, GroupDay} {
		if !r.hasGroup(g) {
			missing = append(missing, g)
		}
	}
	if len(missing) > 0 {
		return nil, problem(CodeRegexGroupsMissing, map[string]any{"groups": missing},
			fmt.Sprintf("正则表达式缺少命名分组 %s", strings.Join(missing, "、")))
	}
	return r, nil
}

func (r *Rule) hasGroup(name string) bool {
	for _, n := range r.names {
		if n == name {
			return true
		}
	}
	return false
}

// Parse applies a rule to one file name (P13.3.1, P13.3.4). A rule that does
// not compile gives an unmatched row whose Error says why.
func Parse(rule FilenameRule, s Sample) Parsed {
	r, err := Compile(rule)
	if err != nil {
		return Failed(s.Filename, err)
	}
	return r.Parse(s)
}

// Parse applies the rule to one file name. Without a year group the year is
// rule.YearDefault when set, otherwise the upload year, or the year before
// when that date would be after the upload date (P13.3.4). A date after the
// upload date is rejected (P13.3.2 rule 3); the result is in the location
// of s.UploadedAt.
func (r *Rule) Parse(s Sample) Parsed {
	p := Parsed{Filename: s.Filename}
	m := r.re.FindStringSubmatchIndex(s.Filename)
	if m == nil {
		p.fail(problem(CodeNoMatch, nil, "文件名与规则不匹配"))
		return p
	}
	p.Matched = true
	// The first participating group of each name wins, so a rule may give
	// alternatives with the same group names.
	group := func(name string) string {
		for i, n := range r.names {
			if n == name && m[2*i] >= 0 {
				return strings.TrimSpace(s.Filename[m[2*i]:m[2*i+1]])
			}
		}
		return ""
	}
	p.Title = cleanTitle(group(GroupTitle))
	p.KindCode = group(GroupKind)
	p.Kind = LookupKind(r.kindMap, p.KindCode)

	rec, hasTime, why := r.date(group, s.UploadedAt)
	if why != nil {
		p.fail(why)
		return p
	}
	if dayNum(rec) > dayNum(s.UploadedAt) {
		d, up := FormatDate(rec), FormatDate(s.UploadedAt)
		p.fail(problem(CodeAfterUpload, map[string]any{"date": d, "uploaded": up},
			fmt.Sprintf("解析出的日期 %s 晚于上传时间 %s", d, up)))
		return p
	}
	p.RecordedAt, p.HasTime = &rec, hasTime
	if ct := s.CreationTime; ct != nil && !ct.IsZero() {
		p.Suspicious = rec.Before(ct.AddDate(-1, 0, 0)) || rec.After(ct.AddDate(1, 0, 0))
	}
	return p
}

// date builds the recording time from the matched groups; why says why it
// failed.
func (r *Rule) date(group func(string) string, uploaded time.Time) (rec time.Time, hasTime bool, why *Problem) {
	month, ok := parseMonth(group(GroupMonth))
	if !ok {
		v := group(GroupMonth)
		return time.Time{}, false, problem(CodeMonthUnknown, map[string]any{"value": v}, fmt.Sprintf("无法识别月份“%s”", v))
	}
	day, ok := parseDay(group(GroupDay))
	if !ok {
		v := group(GroupDay)
		return time.Time{}, false, problem(CodeDayUnknown, map[string]any{"value": v}, fmt.Sprintf("无法识别日期“%s”", v))
	}

	hour, minute := 0, 0
	if hs := group(GroupHour); hs != "" {
		h, okH := atoi(hs)
		mi, okM := 0, true
		if ms := group(GroupMinute); ms != "" {
			mi, okM = atoi(ms)
		}
		if !okH || !okM {
			v := hs + ":" + group(GroupMinute)
			return time.Time{}, false, problem(CodeTimeUnknown, map[string]any{"time": v}, fmt.Sprintf("无法识别时间“%s”", v))
		}
		if ap := group(GroupAMPM); ap != "" {
			pm, okAP := parseAMPM(ap)
			if !okAP || h < 1 || h > 12 {
				v := fmt.Sprintf("%s:%02d %s", hs, mi, ap)
				return time.Time{}, false, problem(CodeTimeInvalid, map[string]any{"time": v}, "时间不合法："+v)
			}
			h %= 12
			if pm {
				h += 12
			}
		}
		if h > 23 || mi > 59 {
			v := fmt.Sprintf("%02d:%02d", h, mi)
			return time.Time{}, false, problem(CodeTimeInvalid, map[string]any{"time": v}, "时间不合法："+v)
		}
		hour, minute, hasTime = h, mi, true
	}

	loc := uploaded.Location()
	var year int
	switch ys := group(GroupYear); {
	case ys != "":
		if year, ok = parseYear(ys); !ok {
			return time.Time{}, false, problem(CodeYearUnknown, map[string]any{"value": ys}, fmt.Sprintf("无法识别年份“%s”", ys))
		}
	case r.yearDefault != nil:
		year = *r.yearDefault
	default:
		year = uploaded.Year()
		if dayNum(time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)) > dayNum(uploaded) {
			year--
		}
	}
	rec = time.Date(year, time.Month(month), day, hour, minute, 0, 0, loc)
	if y, mo, d := rec.Date(); y != year || int(mo) != month || d != day {
		v := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		return time.Time{}, false, problem(CodeDateInvalid, map[string]any{"date": v}, "日期不合法："+v)
	}
	return rec, hasTime, nil
}

// LookupKind maps a captured kind code through a rule's kind map: the
// exact (trimmed) key, else a case-insensitive one (the same code); "" when
// code is empty or not in the map. Whether the kind id exists in the
// catalog is the caller's check.
func LookupKind(kindMap map[string]string, code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(kindMap) == 0 {
		return ""
	}
	if k, ok := kindMap[code]; ok {
		return k
	}
	var keys []string
	for c := range kindMap {
		keys = append(keys, c)
	}
	sort.Strings(keys) // deterministic when two keys differ only in case
	for _, c := range keys {
		if strings.EqualFold(strings.TrimSpace(c), code) {
			return kindMap[c]
		}
	}
	return ""
}

// Validate is the check of P13.3.2 as it was before lecture kinds, for a
// bare regex (no year default, no kind map): rule 4 still rejects a date
// more than MaxDaysBeforeTerm days before the term start (CodeBeforeTerm).
// TRANSITIONAL: kept unchanged for the callers that have not moved to
// ValidateRule yet (the filename_rule task, the API preview); lecture kinds
// (lead ruling 1.1) use ValidateRule, where such a sample is a past lecture.
func Validate(regex string, samples []Sample, termStart *time.Time) (parsed []Parsed, matched int, ok bool, problems []Problem) {
	return validate(FilenameRule{Regex: regex}, samples, termStart, true)
}

// ValidateRule is the deterministic check of a generated or edited rule over
// the course's samples (P13.3.2 rules 1–5). parsed is the preview table, one
// row per sample even when the rule does not compile; rows carry the kind
// through rule.KindMap. ok=false means the rule is rejected; problems says
// why (coded, with the Chinese text). Suspicious rows (rule 5) are marked
// but do not fail the rule. Rule 4 (lead ruling 1.1): a date more than
// MaxDaysBeforeTerm days before the term start is a past lecture — the row
// keeps its date and is marked Past; it is not a failure (ValidateRule never
// produces CodeBeforeTerm).
func ValidateRule(rule FilenameRule, samples []Sample, termStart *time.Time) (parsed []Parsed, matched int, ok bool, problems []Problem) {
	return validate(rule, samples, termStart, false)
}

// validate is ValidateRule; pastFails selects the rule 4 of before lecture
// kinds (Validate).
func validate(rule FilenameRule, samples []Sample, termStart *time.Time, pastFails bool) (parsed []Parsed, matched int, ok bool, problems []Problem) {
	parsed = make([]Parsed, len(samples))
	r, err := Compile(rule)
	if err != nil {
		for i, s := range samples {
			parsed[i] = Failed(s.Filename, err)
		}
		return parsed, 0, false, []Problem{*asProblem(err)}
	}
	if len(samples) == 0 {
		return parsed, 0, false, []Problem{*problem(CodeNoSamples, nil, "没有可用于校验的文件名")}
	}

	var rows []Problem
	for i, s := range samples {
		p := r.Parse(s)
		if p.Matched {
			matched++
		}
		if PastRecording(p.RecordedAt, termStart) {
			if pastFails {
				d, ts := FormatDate(*p.RecordedAt), FormatDate(AlignMonday(*termStart))
				p.fail(problem(CodeBeforeTerm, map[string]any{"date": d, "term_start": ts, "days": MaxDaysBeforeTerm},
					fmt.Sprintf("解析出的日期 %s 早于开课日期 %s 超过 %d 天", d, ts, MaxDaysBeforeTerm)))
				p.RecordedAt, p.HasTime, p.Suspicious = nil, false, false
			} else {
				p.Past = true
			}
		}
		if p.Matched && p.RecordedAt == nil {
			rows = append(rows, *problem(CodeFileInvalid, map[string]any{"filename": s.Filename, "reason": *p.Problem},
				s.Filename+"："+p.Error))
		}
		parsed[i] = p
	}
	if matched*100 < MinMatchPercent*len(samples) {
		problems = append(problems, *problem(CodeLowMatch,
			map[string]any{"matched": matched, "total": len(samples), "percent": MinMatchPercent},
			fmt.Sprintf("只匹配了 %d / %d 个文件名，少于 %d%%", matched, len(samples), MinMatchPercent)))
	}
	problems = append(problems, rows...)
	return parsed, matched, len(problems) == 0, problems
}

// ResolveRecordedAt is P13.3.4: user > filename > the media's creation_time
// > none. creation_time is ignored when it is on the same local day as the
// upload (a download or transcoding time) or after it; it is returned in
// uploadedAt's location. The source is a recorded_at_source value.
func ResolveRecordedAt(user *time.Time, fromFilename *time.Time, creationTime *time.Time, uploadedAt time.Time) (*time.Time, string) {
	switch {
	case dated(user):
		t := *user
		return &t, SourceUser
	case dated(fromFilename):
		t := *fromFilename
		return &t, SourceFilename
	case dated(creationTime):
		t := creationTime.In(uploadedAt.Location())
		if dayNum(t) < dayNum(uploadedAt) {
			return &t, SourceMetadata
		}
	}
	return nil, SourceNone
}

var monthNames = map[string]int{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
	"apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
	"aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
	"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

// parseMonth accepts 1–12 or an English month name or abbreviation, in any
// case, optionally followed by a dot ("Sept.").
func parseMonth(s string) (int, bool) {
	if n, ok := atoi(s); ok {
		return n, n >= 1 && n <= 12
	}
	n, ok := monthNames[strings.TrimSuffix(strings.ToLower(s), ".")]
	return n, ok
}

// parseDay accepts 1–31, optionally with an English ordinal suffix ("7th").
// Whether the day exists in its month is checked once the year is known.
func parseDay(s string) (int, bool) {
	l := strings.ToLower(s)
	for _, suf := range []string{"st", "nd", "rd", "th"} {
		l = strings.TrimSuffix(l, suf)
	}
	n, ok := atoi(l)
	return n, ok && n >= 1 && n <= 31
}

// parseYear accepts four digits, or two digits meaning 20xx.
func parseYear(s string) (int, bool) {
	n, ok := atoi(s)
	switch {
	case !ok:
		return 0, false
	case len(s) == 2:
		return 2000 + n, true
	case len(s) == 4 && n >= 1900:
		return n, true
	}
	return 0, false
}

// parseAMPM accepts am/pm/a/p with optional dots, in any case.
func parseAMPM(s string) (pm bool, ok bool) {
	switch strings.ReplaceAll(strings.ToLower(s), ".", "") {
	case "am", "a":
		return false, true
	case "pm", "p":
		return true, true
	}
	return false, false
}

// atoi parses a non-negative decimal number of at most 4 digits.
func atoi(s string) (int, bool) {
	if s == "" || len(s) > 4 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// cleanTitle tidies the title group for display: underscores become spaces,
// runs of spaces collapse, and separators at either end are dropped.
func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "_", " ")), " ")
	return strings.Trim(s, " -.")
}
