// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

func (e *env) runFilenameRule(d Deps) runResult {
	e.t.Helper()
	e.auxJob(model.StageFilenameRule, e.course.ID)
	return e.run(e.stage(d, model.StageFilenameRule), e.course.ID)
}

func (e *env) proposal() *model.FilenameProposal {
	e.t.Helper()
	v := e.kv(model.KVFilenameProposal + e.course.ID)
	if v == "" {
		return nil
	}
	var p model.FilenameProposal
	e.must(json.Unmarshal([]byte(v), &p))
	return &p
}

func (e *env) samplesUsed() []string {
	e.t.Helper()
	s, err := FilenameSamples(e.ctx, e.st, e.course.ID)
	e.must(err)
	return s
}

// TestFilenameRuleFirstException is P13.3.3 首次例外 / P13.3.5: no lecture
// has a recording time and every sample parses → applied at once, with an
// undo record; the request carries the names, upload times, the stored
// creation_time and the course code.
func TestFilenameRuleFirstException(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	code := "CS101"
	e.must(e.st.SetCourseCode(e.ctx, e.course.ID, &code))
	a := e.upload("CS101_2026-10-06_1000_Stream_Ciphers.mp4", lt(2026, 10, 20, 9, 0))
	b := e.upload("CS101_2026-09-29_1000.mp4", lt(2026, 10, 20, 9, 1))
	c := e.upload("CS101_2026-10-01_1400_Block_Ciphers.mp4", lt(2026, 10, 20, 9, 2))
	// a creation_time on the upload day: stored (shown to the model), not used
	ct := lt(2026, 10, 20, 8, 30)
	e.must(ResolveRecordedAt(e.ctx, d, c.ID, &ct))
	if e.lec(c.ID).RecordedAt != nil {
		t.Fatal("creation_time on the upload day was used")
	}

	r := e.runFilenameRule(d)
	if r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleFilenameRule)
	if len(reqs) != 1 {
		t.Fatalf("%d filename_rule requests", len(reqs))
	}
	for _, s := range []string{
		"COURSE CODE: CS101\n",
		"\nCS101_2026-10-06_1000_Stream_Ciphers.mp4 | 2026-10-20 09:00 | -\n",
		"\nCS101_2026-10-01_1400_Block_Ciphers.mp4 | 2026-10-20 09:02 | 2026-10-20 08:30",
	} {
		if !strings.Contains(reqs[0].User, s) {
			t.Errorf("request lacks %q:\n%s", s, reqs[0].User)
		}
	}
	if !reqs[0].JSONMode {
		t.Error("not a JSON-mode request")
	}

	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if rule.Source != calendar.RuleSourceLLM || rule.SamplesMatched != 3 || rule.SamplesTotal != 3 || rule.Regex != ruleCS101 {
		t.Errorf("rule %+v", rule)
	}
	for id, want := range map[string][2]string{
		a.ID: {"2026-10-06T10:00", "Stream Ciphers"},
		b.ID: {"2026-09-29T10:00", "CS101_2026-09-29_1000.mp4"},
		c.ID: {"2026-10-01T14:00", "Block Ciphers"},
	} {
		l := e.lec(id)
		if ptrStr(l.RecordedAt) != want[0] || l.RecordedAtSource != model.RecordedAtFilename || l.Title != want[1] || l.TitleSource != model.TitleSourceFilename {
			t.Errorf("lecture: %s (%s) %q (%s), want %v", ptrStr(l.RecordedAt), l.RecordedAtSource, l.Title, l.TitleSource, want)
		}
	}
	for id, want := range map[string]int{b.ID: 1, c.ID: 2, a.ID: 3} {
		if s := e.lec(id).Seq; s != want {
			t.Errorf("seq %d, want %d", s, want)
		}
	}
	if e.proposal() != nil {
		t.Error("proposal written although the rule was applied")
	}
	var undo FilenameUndo
	e.must(json.Unmarshal([]byte(e.kv(model.KVFilenameUndo+e.course.ID)), &undo))
	if undo.Rule != nil || len(undo.Lectures) != 3 {
		t.Errorf("undo record %+v", undo)
	}
	if got := e.samplesUsed(); len(got) != 3 {
		t.Errorf("samples used %v", got)
	}
	if !e.ev.has("course", e.course.ID) || !e.ev.has("lecture", a.ID) {
		t.Error("no SSE events")
	}
}

// TestFilenameRuleProposalOnly is P13.3.5 “存在不匹配项时只生成提示”, and
// the other conditions of the first exception: a lecture already dated, a
// user-written rule (never replaced automatically), a citation export.
func TestFilenameRuleProposalOnly(t *testing.T) {
	cases := map[string]func(e *env){
		"unmatched file": func(e *env) {
			e.upload("other format 2026.mp4", lt(2026, 10, 20, 9, 5))
		},
		"a lecture has a time": func(e *env) {
			l := e.upload("CS101_2026-10-05_0900.mp4", lt(2026, 10, 20, 9, 5)) // matches: only the time blocks
			e.must(e.st.SetLectureRecordedAt(e.ctx, l.ID, strp("2026-10-05T09:00"), model.RecordedAtMetadata))
		},
		"user rule": func(e *env) {
			e.setRule(`^XYZ(?P<month>\d\d)(?P<day>\d\d)$`, calendar.RuleSourceUser)
		},
		"citations exported": func(e *env) {
			e.must(e.st.SetKV(e.ctx, model.KVCiteExportPrefix+e.course.ID, db.Now()))
		},
		"suspicious file": func(e *env) {
			l := e.upload("CS101_2026-10-08_1000.mp4", lt(2026, 10, 20, 9, 5))
			ct := lt(2024, 10, 8, 10, 0) // two years earlier: P13.3.2 rule 5
			e.must(e.st.SetKV(e.ctx, KVCreationTimePrefix+l.ID, ct.Format(time.RFC3339)))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			a := e.upload("CS101_2026-10-06_1000_Intro.mp4", lt(2026, 10, 20, 9, 0))
			e.upload("CS101_2026-09-29_1000.mp4", lt(2026, 10, 20, 9, 1))
			e.upload("CS101_2026-10-01_1400.mp4", lt(2026, 10, 20, 9, 2))
			setup(e)
			before := e.courseRow().FilenameRule
			r := e.runFilenameRule(d)
			if r.err != nil {
				t.Fatal(r.err)
			}
			p := e.proposal()
			if p == nil {
				t.Fatal("no proposal")
			}
			var rule calendar.FilenameRule
			e.must(json.Unmarshal(p.Rule, &rule))
			if rule.Regex != ruleCS101 || rule.Source != calendar.RuleSourceLLM || p.Explanation == "" || p.CreatedAt == "" {
				t.Errorf("proposal %+v / %+v", p, rule)
			}
			if ptrStr(e.courseRow().FilenameRule) != ptrStr(before) {
				t.Error("course rule changed without the user")
			}
			if l := e.lec(a.ID); l.RecordedAt != nil || l.Title != a.Title {
				t.Errorf("lecture changed: %s %q", ptrStr(l.RecordedAt), l.Title)
			}
			if e.kv(model.KVFilenameUndo+e.course.ID) != "" {
				t.Error("undo record without an automatic apply")
			}
			if len(e.samplesUsed()) < 3 {
				t.Errorf("samples used %v", e.samplesUsed())
			}
		})
	}
}

// TestFilenameRuleValidation is P13.3.5 规则校验: compile failure, missing
// groups, too few matches, impossible dates and dates after the upload all
// fail the task after one retry; the course keeps having no rule.
func TestFilenameRuleValidation(t *testing.T) {
	dated := []string{"CS101_2026-10-06_1000.mp4", "CS101_2026-09-29_1000.mp4", "CS101_2026-10-01_1400.mp4"}
	cases := map[string]struct {
		regex string
		names []string
	}{
		"does not compile": {`^CS101_(?P<month>\d{2}`, dated},
		"lookbehind":       {`(?<=CS101_)(?P<month>\d{2})-(?P<day>\d{2})`, dated},
		"no day group":     {`^CS101_\d{4}-(?P<month>\d{2})-\d{2}`, dated},
		"too few matches":  {`^CS101_(?P<year>\d{4})-(?P<month>10)-(?P<day>06)`, dated},
		// day and month swapped: "2026-09-29" gives month 29
		"invalid date": {`^CS101_(?P<year>\d{4})-(?P<day>\d{2})-(?P<month>\d{2})`, dated},
		"after upload": {`^CS101_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`,
			[]string{"CS101_2026-10-06_1000.mp4", "CS101_2026-11-29_1000.mp4", "CS101_2026-12-01_1400.mp4"}},
	}
	for name, c := range cases {
		regex, names := c.regex, c.names
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			up := lt(2026, 10, 20, 9, 0)
			for i, n := range names {
				e.upload(n, up.Add(time.Duration(i)*time.Minute))
			}
			answer, _ := json.Marshal(map[string]string{"regex": regex, "explanation": "x"})
			e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return string(answer) })
			r := e.runFilenameRule(d)
			if r.err == nil {
				t.Fatal("rule accepted")
			}
			if code := errCode(r.err); code != "filename_rule.rejected" {
				t.Errorf("code %q (%v)", code, r.err)
			}
			if !strings.Contains(r.err.Error(), regex) {
				t.Errorf("error does not name the regex (D5): %v", r.err)
			}
			if n := e.fake.Count(llmtest.RoleFilenameRule); n != 2 {
				t.Errorf("%d requests, want 2 (one retry)", n)
			}
			if e.courseRow().FilenameRule != nil || e.proposal() != nil {
				t.Error("rejected rule saved or proposed")
			}
			if r.job.Status != model.StatusFailed {
				t.Errorf("job %s", r.job.Status)
			}
			if got := e.samplesUsed(); len(got) != len(names) {
				t.Errorf("names asked about are not marked as used: %v", got)
			}
		})
	}
}

// TestFilenameRuleRetryOnce: an unusable first answer is asked again once.
func TestFilenameRuleRetryOnce(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	e.upload("CS101_2026-10-06_1000.mp4", lt(2026, 10, 20, 9, 0))
	e.fake.FailNext(llmtest.RoleFilenameRule, 1, llmtest.FailNotJSON)
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	if n := e.fake.Count(llmtest.RoleFilenameRule); n != 2 {
		t.Errorf("%d requests", n)
	}
	if e.courseRow().FilenameRule == nil {
		t.Error("rule not applied after the retry")
	}

	e2 := newEnv(t)
	e2.organizerRoles()
	e2.upload("CS101_2026-10-06_1000.mp4", lt(2026, 10, 20, 9, 0))
	e2.fake.FailNext(llmtest.RoleFilenameRule, 2, llmtest.FailNotJSON)
	r := e2.runFilenameRule(e2.deps(nil))
	if errCode(r.err) != "filename_rule.invalid_output" {
		t.Errorf("two unusable answers: %v", r.err)
	}
}

// TestFilenameRuleNoSamples: no file names → fails without a request.
func TestFilenameRuleNoSamples(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	e.lecture("old lecture", "x.mp4") // created before v2: no original file name
	r := e.runFilenameRule(e.deps(nil))
	if errCode(r.err) != "filename_rule.no_samples" {
		t.Errorf("err %v", r.err)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// TestFilenameRuleSampleSelection: at most the 50 newest names; with an old
// rule, names it matches are kept among them (P13.3.3: the new rule must
// stay compatible with the old files).
func TestFilenameRuleSampleSelection(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	var cands []model.Lecture // newest first
	for i := 59; i >= 0; i-- {
		name := fmt.Sprintf("new format %02d.mp4", i)
		if i < 15 {
			name = fmt.Sprintf("CS101_2026-09-%02d_1000.mp4", i+1)
		}
		cands = append(cands, model.Lecture{ID: fmt.Sprint(i), OriginalFilename: name, CreatedAt: db.FormatTime(base.Add(time.Duration(i) * time.Hour))})
	}
	got := selectSamples(cands, nil, 50, 10)
	if len(got) != 50 || got[0].ID != "59" || got[49].ID != "10" {
		t.Fatalf("newest 50: %d, %s…%s", len(got), got[0].ID, got[len(got)-1].ID)
	}
	old := &calendar.FilenameRule{Regex: ruleCS101}
	got = selectSamples(cands, old, 50, 10)
	if len(got) != 50 {
		t.Fatalf("%d samples", len(got))
	}
	matched := 0
	for i, l := range got {
		if strings.HasPrefix(l.OriginalFilename, "CS101_") {
			matched++
		}
		if i > 0 && l.CreatedAt > got[i-1].CreatedAt {
			t.Fatal("not newest first")
		}
	}
	if matched != 10 {
		t.Errorf("%d old-format samples, want 10", matched)
	}
	if got[0].ID != "59" {
		t.Error("the newest name was dropped")
	}
	if few := selectSamples(cands[:20], old, 50, 10); len(few) != 20 {
		t.Errorf("under the limit: %d", len(few))
	}
}

// TestFilenameRuleRetryFeedback: the retry repeats the request with why the
// regex was rejected appended (lead ruling after the phase 2 acceptance,
// where deepseek-chat sent the same wrong regex four times): the regex, how
// many names it matched and which not. The prompt is unchanged.
func TestFilenameRuleRetryFeedback(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	names := []string{
		"CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4",
		"CSC1003-L01-02 Wed Oct 14 09-35-00 BST 2026.mp4",
		"CSC1003-L01-03 Wed Oct 21 09-35-00 BST 2026.mp4",
	}
	for i, n := range names {
		e.upload(n, lt(2026, 10, 22, 9, i))
	}
	// The acceptance's rejected answer: day before month.
	wrong := `CSC1003-L\d+-\d+ (?P<day>\d{2}) (?P<month>Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) (?P<year>\d{4})`
	right := `CSC1003-L\d+-\d+ \w{3} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3} (?P<year>\d{4})`
	n := 0
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string {
		n++
		regex := right
		if n == 1 {
			regex = wrong
		}
		b, _ := json.Marshal(map[string]string{"regex": regex, "explanation": "x"})
		return string(b)
	})
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleFilenameRule)
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[0].System != reqs[1].System {
		t.Error("the retry changed the system prompt")
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	if fb == reqs[1].User || strings.Contains(reqs[0].User, "REJECTED") {
		t.Fatalf("requests:\n%s\n---\n%s", reqs[0].User, reqs[1].User)
	}
	for _, want := range append([]string{
		"YOUR PREVIOUS ANSWER WAS REJECTED",
		"- Your regex was: " + wrong,
		"- It matches only 0 of 3 file names; at least 60% must match.",
		"- File names it does not match:",
	}, names...) {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback lacks %q:\n%s", want, fb)
		}
	}
	if rule := e.courseRow().FilenameRule; rule == nil || !strings.Contains(*rule, "hour") {
		t.Errorf("the second regex was not applied: %v", rule)
	}
}

// TestFilenameRejection: the feedback says in English why each kind of
// rejected rule failed (P13.3.2 rules 1–3), with the captured groups of the
// names whose date is wrong. Rule 4 since lecture kinds (lead ruling 1.1):
// a date more than 30 days before the term start is a past lecture, not a
// failure — ValidateRule accepts it and the feedback never names the term.
func TestFilenameRejection(t *testing.T) {
	up := lt(2026, 10, 20, 9, 0)
	samples := func(names ...string) []calendar.Sample {
		out := make([]calendar.Sample, len(names))
		for i, n := range names {
			out[i] = calendar.Sample{Filename: n, UploadedAt: up}
		}
		return out
	}
	dated := samples("CS101_2026-10-06_1000.mp4", "CS101_2026-09-29_1000.mp4", "CS101_2026-10-01_1400.mp4")
	term := lt(2026, 12, 1, 0, 0)
	cases := []struct {
		name, regex string
		samples     []calendar.Sample
		termStart   *time.Time
		want        []string
	}{
		{"lookbehind", `(?<=CS101_)(?P<month>\d{2})-(?P<day>\d{2})`, dated, nil,
			[]string{"It does not compile as a Go RE2 regular expression: error parsing regexp: ", "RE2 has no lookbehind"}},
		{"no day group", `^CS101_\d{4}-(?P<month>\d{2})-\d{2}`, dated, nil,
			[]string{"It lacks the required named group(s) day;"}},
		{"too few matches", `^CS101_(?P<year>\d{4})-(?P<month>10)-(?P<day>06)`, dated, nil,
			[]string{"It matches only 1 of 3 file names", "File names it does not match:\n  CS101_2026-09-29_1000.mp4\n  CS101_2026-10-01_1400.mp4"}},
		{"invalid date", `^CS101_(?P<year>\d{4})-(?P<day>\d{2})-(?P<month>\d{2})`, dated, nil,
			[]string{"CS101_2026-09-29_1000.mp4 → year=2026 month=29 day=09: the captured groups do not form a valid date and time"}},
		{"after upload", `^CS101_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})_(?P<hour>\d{2})(?P<minute>\d{2})`,
			samples("CS101_2026-10-06_1000.mp4", "CS101_2026-11-29_1000.mp4", "CS101_2026-12-01_1400.mp4"), nil,
			[]string{"CS101_2026-11-29_1000.mp4 → year=2026 month=11 day=29 hour=10 minute=00: 2026-11-29 is after the upload date 2026-10-20",
				"CS101_2026-12-01_1400.mp4 → year=2026 month=12 day=01 hour=14 minute=00: 2026-12-01 is after the upload date 2026-10-20"}},
		// rule 4 no longer fails: the other problems are still told, and
		// no name is reported for being before the term
		{"before the term with an invalid date", `^CS101_(?P<year>\d{4})-(?P<day>\d{2})-(?P<month>\d{2})`, dated, &term,
			[]string{"CS101_2026-09-29_1000.mp4 → year=2026 month=29 day=09: the captured groups do not form a valid date and time"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, matched, ok, _ := calendar.ValidateRule(calendar.FilenameRule{Regex: c.regex}, c.samples, c.termStart)
			if ok {
				t.Fatal("rule accepted")
			}
			fb := strings.Join(ctxbuild.FilenameRuleProblems(filenameRejection(c.regex, c.samples, parsed, matched, nil)), "\n")
			if strings.Contains(fb, "term start") {
				t.Errorf("feedback names the term start:\n%s", fb)
			}
			if !strings.HasPrefix(fb, "Your regex was: "+c.regex+"\n") {
				t.Errorf("feedback does not start with the regex:\n%s", fb)
			}
			for _, w := range c.want {
				if !strings.Contains(fb, w) {
					t.Errorf("feedback lacks %q:\n%s", w, fb)
				}
			}
		})
	}
	// before_the_term (changed by lecture kinds): the rule that used to be
	// rejected because 2026-09-29 is more than 30 days before the term start
	// 2026-11-30 is accepted; every row keeps its date and is a past lecture.
	t.Run("before the term", func(t *testing.T) {
		regex := `^CS101_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
		parsed, matched, ok, problems := calendar.ValidateRule(calendar.FilenameRule{Regex: regex}, dated, &term)
		if !ok || matched != 3 || len(problems) != 0 {
			t.Fatalf("ok %v, matched %d, problems %v", ok, matched, problems)
		}
		for _, p := range parsed {
			if !p.Past || p.RecordedAt == nil || p.Status() != calendar.StatusOK {
				t.Errorf("%s: past %v, date %v, status %s", p.Filename, p.Past, p.RecordedAt, p.Status())
			}
		}
		near := lt(2026, 10, 20, 0, 0) // within 30 days of every date: none is past
		parsed, _, ok, _ = calendar.ValidateRule(calendar.FilenameRule{Regex: regex}, dated, &near)
		if !ok || parsed[0].Past || parsed[1].Past || parsed[2].Past {
			t.Errorf("ok %v, rows %+v", ok, parsed)
		}
	})
}
