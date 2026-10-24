// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Lecture kinds in the pipeline (user request 2026-10-23, lead freeze
// .claude/kinds-brief.md; K2): the filename_rule kind group and kind_map,
// the title task's kind, the order user > filename > llm, numbering by
// kind, the material-set check after recorded_at changes, the week-bound
// slides of the whisper prompt.

// setKind stores a lecture's kind directly.
func (e *env) setKind(id, kind, source string) {
	e.t.Helper()
	e.must(e.st.SetLectureKind(e.ctx, id, kind, source))
}

func (e *env) kindOf(id string) (string, string) {
	e.t.Helper()
	l := e.lec(id)
	return l.Kind, l.KindSource
}

// materialsChecks counts the materials invalidations requested.
func (e *env) materialsChecks() int {
	n := 0
	for _, c := range e.invalCauses() {
		if c == (state.StaleCause{Kind: state.StaleMaterials}) {
			n++
		}
	}
	return n
}

// jobSnapshot is the config_snapshot of the current job.
func (e *env) jobSnapshot(scope, id, stage string) map[string]any {
	e.t.Helper()
	var m map[string]any
	e.must(json.Unmarshal([]byte(e.job(scope, id, stage).ConfigSnapshot), &m))
	return m
}

// uploadPanopto uploads llmtest.PanoptoNames (one minute apart, 2026-10-20).
func (e *env) uploadPanopto() []model.Lecture {
	e.t.Helper()
	var ls []model.Lecture
	for i, n := range llmtest.PanoptoNames {
		ls = append(ls, e.upload(n, lt(2026, 10, 20, 9, i)))
	}
	return ls
}

// TestFilenameRuleKinds: the Panopto names L… / T… get the kinds lecture /
// tutorial (source filename) through the answer's kind group and kind_map;
// the request lists the kind catalog; the rule keeps kind_map; the numbers
// are per week and kind (W02L1, W02T1); the material sets are checked
// because recording times changed; the prompt version is recorded.
func TestFilenameRuleKinds(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	ls := e.uploadPanopto()
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return llmtest.PanoptoFilenameRuleAnswer })
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleFilenameRule)
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, s := range []string{
		"LECTURE KINDS (the ids for kind_map), one per line as: id | code | meaning\nlecture | L | a regular lecture of the course\ntutorial | T | ",
		"\npast_lecture | P | ", "\nmaterial | M | ",
	} {
		if !strings.Contains(reqs[0].User, s) {
			t.Errorf("request lacks %q:\n%s", s, reqs[0].User)
		}
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied (first exception)")
	}
	if rule.KindMap["L"] != "lecture" || rule.KindMap["T"] != "tutorial" || len(rule.KindMap) != 2 {
		t.Errorf("kind_map %v", rule.KindMap)
	}
	want := []struct{ kind, label string }{{"lecture", "W02L1"}, {"tutorial", "W02T1"}, {"lecture", "W03L1"}, {"tutorial", "W03T1"}}
	nums, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	for i, l := range ls {
		k, src := e.kindOf(l.ID)
		if k != want[i].kind || src != model.KindSourceFilename {
			t.Errorf("%s: kind %q (%s), want %s", l.OriginalFilename, k, src, want[i].kind)
		}
		if n := nums[l.ID]; n.Label != want[i].label || n.Kind != want[i].kind {
			t.Errorf("%s: number %s (%s), want %s", l.OriginalFilename, n.Label, n.Kind, want[i].label)
		}
	}
	if e.materialsChecks() != 1 {
		t.Errorf("materials checks %v", e.invalCauses())
	}
	if v := e.jobSnapshot(model.ScopeCourse, e.course.ID, model.StageFilenameRule)["prompt_version"]; v != float64(ctxbuild.FilenameRulePromptVersion) {
		t.Errorf("prompt_version %v", v)
	}
	// the undo record keeps the kinds before (none): undo clears them
	e.must(UndoFilenameRule(e.ctx, d, e.course.ID))
	for _, l := range ls {
		if k, src := e.kindOf(l.ID); k != "" || src != model.KindSourceNone {
			t.Errorf("after undo: %q (%s)", k, src)
		}
	}
	if e.materialsChecks() != 2 {
		t.Errorf("undo changed recording times without a materials check: %v", e.invalCauses())
	}
}

// TestFilenameRuleKindPrecedence: a rule never replaces a user kind (lead
// ruling 1.2); it replaces an llm kind and its own filename kind; a rule
// that no longer gives a kind clears a filename kind and leaves the others.
func TestFilenameRuleKindPrecedence(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	ls := e.uploadPanopto()
	e.setKind(ls[0].ID, model.KindSeminar, model.KindSourceUser)
	e.setKind(ls[1].ID, model.KindSeminar, model.KindSourceLLM)
	e.setKind(ls[2].ID, model.KindTutorial, model.KindSourceFilename)
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: llmtest.PanoptoRegex, KindMap: llmtest.PanoptoKindMap}))
	want := [][2]string{
		{model.KindSeminar, model.KindSourceUser},      // user kept
		{model.KindTutorial, model.KindSourceFilename}, // llm replaced
		{model.KindLecture, model.KindSourceFilename},  // filename replaced by the new rule
		{model.KindTutorial, model.KindSourceFilename}, // none set
	}
	for i, l := range ls {
		if k, src := e.kindOf(l.ID); k != want[i][0] || src != want[i][1] {
			t.Errorf("%s: %q (%s), want %v", l.OriginalFilename, k, src, want[i])
		}
	}
	// a rule without the kind group: filename kinds are cleared, user kept
	e.setKind(ls[1].ID, model.KindSeminar, model.KindSourceLLM)
	noKind := strings.Replace(llmtest.PanoptoRegex, "(?P<kind>[A-Z]+)", "[A-Z]+", 1)
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: noKind}))
	want = [][2]string{
		{model.KindSeminar, model.KindSourceUser},
		{model.KindSeminar, model.KindSourceLLM},
		{"", model.KindSourceNone},
		{"", model.KindSourceNone},
	}
	for i, l := range ls {
		if k, src := e.kindOf(l.ID); k != want[i][0] || src != want[i][1] {
			t.Errorf("no kind group, %s: %q (%s), want %v", l.OriginalFilename, k, src, want[i])
		}
	}
	// a kind_map naming a kind that is not in the catalog (a rule saved
	// before the kind was deleted) gives no kind
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: llmtest.PanoptoRegex, KindMap: map[string]string{"L": "lecture", "T": "01JGONE"}}))
	if k, src := e.kindOf(ls[3].ID); k != "" || src != model.KindSourceNone {
		t.Errorf("unknown kind id: %q (%s)", k, src)
	}
	if k, _ := e.kindOf(ls[2].ID); k != model.KindLecture {
		t.Errorf("L: %q", k)
	}
}

// TestFilenameRuleKindMapChecked: kind_map entries whose kind is not a
// catalog id or whose code is empty are dropped (an id in another case is
// corrected); the rule is accepted all the same; when the rule is asked
// again for another reason, the feedback says what was dropped.
func TestFilenameRuleKindMapChecked(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	ls := e.uploadPanopto()
	wrong := `^CSC1002-(?P<kind>[A-Z]+)\d{2}-\d{2} \w{3} (?P<day>\d{2}) (?P<month>[A-Z][a-z]{2})` // never matches
	km := map[string]string{"L": "Lecture", "T": "practical", "": "seminar"}
	n := 0
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string {
		n++
		if n == 1 {
			return llmtest.FilenameRuleAnswer(wrong, km)
		}
		return llmtest.FilenameRuleAnswer(llmtest.PanoptoRegex, km)
	})
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleFilenameRule)
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	for _, w := range []string{
		`- kind_map entry "T" → "practical" was dropped: "practical" is not one of the LECTURE KINDS ids.`,
		`- kind_map entry "" → "seminar" was dropped: the code is empty.`,
	} {
		if !strings.Contains(fb, w) {
			t.Errorf("feedback lacks %q:\n%s", w, fb)
		}
	}
	if strings.Contains(fb, `"L" → "Lecture"`) {
		t.Errorf("an id in another case is not a problem:\n%s", fb)
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if len(rule.KindMap) != 1 || rule.KindMap["L"] != model.KindLecture {
		t.Errorf("kind_map %v", rule.KindMap)
	}
	if k, _ := e.kindOf(ls[0].ID); k != model.KindLecture {
		t.Errorf("L: %q", k)
	}
	if k, src := e.kindOf(ls[1].ID); k != "" || src != model.KindSourceNone {
		t.Errorf("T (dropped): %q (%s)", k, src)
	}
}

// TestFilenameRuleKindMapWithoutGroup: a kind_map without a kind group, or
// one that is not an object, is ignored; the rule is accepted.
func TestFilenameRuleKindMapWithoutGroup(t *testing.T) {
	for name, answer := range map[string]string{
		"no group":        llmtest.FilenameRuleAnswer(strings.Replace(llmtest.PanoptoRegex, "(?P<kind>[A-Z]+)", "[A-Z]+", 1), llmtest.PanoptoKindMap),
		"not an object":   `{"regex": ` + mustJSON(llmtest.PanoptoRegex) + `, "kind_map": ["L", "lecture"], "explanation": "x"}`,
		"null":            `{"regex": ` + mustJSON(llmtest.PanoptoRegex) + `, "kind_map": null, "explanation": "x"}`,
		"absent":          llmtest.FilenameRuleAnswer(llmtest.PanoptoRegex, nil),
		"empty":           llmtest.FilenameRuleAnswer(llmtest.PanoptoRegex, map[string]string{}),
		"non-string kind": `{"regex": ` + mustJSON(llmtest.PanoptoRegex) + `, "kind_map": {"L": 1}, "explanation": "x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			ls := e.uploadPanopto()
			e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return answer })
			if r := e.runFilenameRule(e.deps(nil)); r.err != nil {
				t.Fatal(r.err)
			}
			if n := e.fake.Count(llmtest.RoleFilenameRule); n != 1 {
				t.Errorf("%d requests: a kind_map problem alone must not be asked again", n)
			}
			var rule calendar.FilenameRule
			if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
				t.Fatal("rule not applied")
			}
			if rule.KindMap != nil {
				t.Errorf("kind_map %v", rule.KindMap)
			}
			if raw := *e.courseRow().FilenameRule; strings.Contains(raw, "kind_map") {
				t.Errorf("an empty kind_map is stored: %s", raw)
			}
			for _, l := range ls {
				if k, src := e.kindOf(l.ID); k != "" || src != model.KindSourceNone {
					t.Errorf("%s: %q (%s)", l.OriginalFilename, k, src)
				}
			}
		})
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestFilenameRulePastSamplesAccepted: samples recorded more than 30 days
// before the term start no longer fail the rule (rule 4 since lecture
// kinds): it is applied and those lectures are numbered as past lectures.
func TestFilenameRulePastSamplesAccepted(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	old := e.upload("CS101_2025-10-08_1000_Old_Recording.mp4", lt(2026, 10, 20, 9, 0))
	cur := e.upload("CS101_2026-10-07_1000.mp4", lt(2026, 10, 20, 9, 1))
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatalf("rule with a past sample rejected: %v", r.err)
	}
	if n := e.fake.Count(llmtest.RoleFilenameRule); n != 1 {
		t.Errorf("%d requests", n)
	}
	if e.courseRow().FilenameRule == nil {
		t.Fatal("rule not applied")
	}
	if ptrStr(e.lec(old.ID).RecordedAt) != "2025-10-08T10:00" {
		t.Errorf("past recording dated %s", ptrStr(e.lec(old.ID).RecordedAt))
	}
	nums, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	if n := nums[old.ID]; n.Label != "P1" || n.Kind != model.KindPastLecture {
		t.Errorf("past recording numbered %s (%s)", n.Label, n.Kind)
	}
	if n := nums[cur.ID]; n.Label != "W02L1" {
		t.Errorf("current lecture numbered %s", n.Label)
	}
}

// TestTitleKind: the title answer's kind is written with source llm (and
// the request lists the catalog with user-defined names); an id in another
// case is matched; an id not in the catalog is dropped without failing the
// task; the prompt version is recorded.
func TestTitleKind(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	name := "Lab session"
	lab, err := e.st.InsertLectureKind(e.ctx, model.LectureKind{Code: "LAB", Name: &name, Group: model.KindGroupSession})
	e.must(err)
	l := e.titleLecture("CS101 tutorial 2.mp4")
	answer := llmtest.TitleAnswerWithKind("tutorial")
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return answer })

	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if k, src := e.kindOf(l.ID); k != model.KindTutorial || src != model.KindSourceLLM {
		t.Errorf("kind %q (%s)", k, src)
	}
	u := e.fake.Requests(llmtest.RoleTitle)[0].User
	for _, s := range []string{
		"\n\nLECTURE KINDS (answer one id), one per line as: id | meaning\nlecture | a regular lecture of the course\n",
		"\n" + lab.ID + ` | user-defined kind named "Lab session" (a class session)`,
	} {
		if !strings.Contains(u, s) {
			t.Errorf("request lacks %q:\n%s", s, u)
		}
	}
	if v := e.jobSnapshot(model.ScopeLecture, l.ID, model.StageTitle)["prompt_version"]; v != float64(ctxbuild.TitlePromptVersion) {
		t.Errorf("prompt_version %v", v)
	}

	answer = llmtest.TitleAnswerWithKind(strings.ToLower(lab.ID)) // the custom kind, in another case
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if k, src := e.kindOf(l.ID); k != lab.ID || src != model.KindSourceLLM {
		t.Errorf("custom kind: %q (%s)", k, src)
	}

	answer = llmtest.TitleAnswerWithKind("practical") // not in the catalog
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatalf("an unknown kind failed the task: %v", r.err)
	}
	if k, src := e.kindOf(l.ID); k != lab.ID || src != model.KindSourceLLM {
		t.Errorf("unknown kind changed the lecture: %q (%s)", k, src)
	}
	answer = strings.Replace(llmtest.DefaultTitleAnswer, "}", `, "kind": ["tutorial"]}`, 1) // not a string
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatalf("a non-string kind failed the task: %v", r.err)
	}
	if k, _ := e.kindOf(l.ID); k != lab.ID {
		t.Errorf("non-string kind changed the lecture: %q", k)
	}
	if n := e.fake.Count(llmtest.RoleTitle); n != 4 {
		t.Errorf("%d requests: an unknown or malformed kind must not be asked again", n)
	}
	if got := e.lec(l.ID); got.Title != "Introduction to Testing" || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("title %q (%s)", got.Title, got.TitleSource)
	}
}

// TestTitleKindPrecedence: the title task's kind never replaces a kind from
// the file name or the user (lead ruling 1.2), not even on a confirmed
// regeneration; an answer without a kind changes nothing.
func TestTitleKindPrecedence(t *testing.T) {
	cases := []struct {
		name, kind, source string
	}{
		{"filename", model.KindTutorial, model.KindSourceFilename},
		{"user", model.KindLecture, model.KindSourceUser},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			l := e.titleLecture("x.mp4")
			e.setKind(l.ID, c.kind, c.source)
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind(model.KindSeminar) })
			e.must(RequestTitleForce(e.ctx, e.st, l.ID))
			if r := e.runTitle(d, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			if k, src := e.kindOf(l.ID); k != c.kind || src != c.source {
				t.Errorf("kind %q (%s), want %s (%s)", k, src, c.kind, c.source)
			}
			if got := e.lec(l.ID); got.TitleSource != model.TitleSourceLLM {
				t.Errorf("the title was not regenerated: %s", got.TitleSource)
			}
		})
	}
	// no kind in the answer (testdata/llm/title.json): an llm kind stays
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	e.setKind(l.ID, model.KindSeminar, model.KindSourceLLM)
	if r := e.runTitle(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if k, src := e.kindOf(l.ID); k != model.KindSeminar || src != model.KindSourceLLM {
		t.Errorf("answer without kind: %q (%s)", k, src)
	}
}

// TestTitleKindWithBlurbsOnly: when both answers' titles are too long, the
// valid descriptions are written (P13.5) and so is that answer's kind.
func TestTitleKindWithBlurbsOnly(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	long := `{"title_en": "One Two Three Four Five Six Seven Eight Nine Ten", "title_zh": "标题", "blurb_en": "What a student learns.", "blurb_zh": "学生学到什么。", "kind": "tutorial"}`
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return long })
	r := e.runTitle(e.deps(nil), l.ID)
	if errCode(r.err) != "title.invalid_output" {
		t.Fatalf("err %v", r.err)
	}
	got := e.lec(l.ID)
	if ptrStr(got.BlurbEn) != "What a student learns." || got.TitleSource != model.TitleSourceFilename {
		t.Errorf("blurb %s, title source %s", ptrStr(got.BlurbEn), got.TitleSource)
	}
	if got.Kind != model.KindTutorial || got.KindSource != model.KindSourceLLM {
		t.Errorf("kind %q (%s)", got.Kind, got.KindSource)
	}
}

// TestCourseLabelsKindsAndNumbering: CourseLabels numbers by week and kind
// with the catalog (a user-defined code) and the course's numbering mode.
func TestCourseLabelsKindsAndNumbering(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	name := "Lab"
	lab, err := e.st.InsertLectureKind(e.ctx, model.LectureKind{Code: "LAB", Name: &name, Group: model.KindGroupSession})
	e.must(err)
	a := e.upload("a.mp4", lt(2026, 10, 20, 9, 0))
	b := e.upload("b.mp4", lt(2026, 10, 20, 9, 1))
	c := e.upload("c.mp4", lt(2026, 10, 20, 9, 2))
	for id, at := range map[string]string{a.ID: "2026-10-05T09:00", b.ID: "2026-10-06T09:00", c.ID: "2026-10-07T09:00"} {
		e.must(e.st.SetLectureRecordedAt(e.ctx, id, &at, model.RecordedAtUser))
	}
	e.setKind(b.ID, lab.ID, model.KindSourceUser)
	nums, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	for id, want := range map[string]string{a.ID: "W02L1", b.ID: "W02LAB1", c.ID: "W02L2"} {
		if n := nums[id]; n.Label != want {
			t.Errorf("%s, want %s", n.Label, want)
		}
	}
	if !nums[a.ID].ShowN || nums[b.ID].ShowN {
		t.Errorf("auto: show_n %v %v", nums[a.ID].ShowN, nums[b.ID].ShowN)
	}
	e.must(e.st.SetCourseNumbering(e.ctx, e.course.ID, model.NumberingAlways))
	nums, err = CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	if !nums[b.ID].ShowN {
		t.Error("always: the lab's number is hidden")
	}
	if l := ExportLabel(nums, b.ID); l != "W02LAB1" {
		t.Errorf("export label %q", l)
	}
}

// TestResolveRecordedAtMaterialsCheck: a recorded_at change (from the media
// creation_time, from a rule) requests the material-set check; a call that
// changes nothing, or only the kind, does not.
func TestResolveRecordedAtMaterialsCheck(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.upload("T01 2026-10-07.mp4", lt(2026, 10, 20, 9, 0))
	ct := lt(2026, 10, 6, 10, 0)
	e.must(ResolveRecordedAt(e.ctx, d, l.ID, &ct))
	if ptrStr(e.lec(l.ID).RecordedAt) != "2026-10-06T10:00" || e.materialsChecks() != 1 {
		t.Fatalf("recorded_at %s, checks %v", ptrStr(e.lec(l.ID).RecordedAt), e.invalCauses())
	}
	e.must(ResolveRecordedAt(e.ctx, d, l.ID, nil))
	if e.materialsChecks() != 1 {
		t.Errorf("nothing changed, checks %v", e.invalCauses())
	}
	// a rule: the date from the file name → a second check
	regex := `^(?P<kind>[A-Z])\d+ (?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})`
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: regex}))
	if ptrStr(e.lec(l.ID).RecordedAt) != "2026-10-07T00:00" || e.materialsChecks() != 2 {
		t.Fatalf("recorded_at %s, checks %v", ptrStr(e.lec(l.ID).RecordedAt), e.invalCauses())
	}
	// the same rule with a kind_map: only the kind changes, no check
	b, err := json.Marshal(calendar.FilenameRule{Regex: regex, Source: calendar.RuleSourceUser, KindMap: map[string]string{"T": model.KindTutorial}})
	e.must(err)
	raw := string(b)
	e.must(e.st.SetCourseFilenameRule(e.ctx, e.course.ID, &raw))
	e.must(ResolveRecordedAt(e.ctx, d, l.ID, nil))
	if k, src := e.kindOf(l.ID); k != model.KindTutorial || src != model.KindSourceFilename {
		t.Errorf("kind %q (%s)", k, src)
	}
	if e.materialsChecks() != 2 {
		t.Errorf("a kind change requested a materials check: %v", e.invalCauses())
	}
}

// TestASRPromptWeekMaterials: the whisper prompt is ordered by the slides
// bound to the lecture and to its teaching week (lead ruling for K2), not
// by another week's; the asr input hash ignores them.
func TestASRPromptWeekMaterials(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	e.addTierTerm("TLB", "", model.SourceMaterial, 9, model.TierEnforce, false, true)
	e.addTierTerm("Belady", "", model.SourceMaterial, 2, model.TierEnforce, false, true)
	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	l := e.lecture("L1", "m.mp4")
	e.must(e.st.SetLectureRecordedAt(e.ctx, l.ID, strp("2026-10-07T10:00"), model.RecordedAtUser)) // week 2
	weekMaterial := func(week int, filename string, pages []string) {
		m := model.Material{ID: db.NewID(), CourseID: e.course.ID, BindWeek: &week, Filename: filename, Path: filename, CreatedAt: db.Now()}
		e.must(e.st.CreateMaterial(e.ctx, m))
		e.must(e.st.SetMaterialPages(e.ctx, m.ID, pages))
	}
	weekMaterial(2, "week2.pdf", []string{"Belady's anomaly", "Belady and LRU"})
	weekMaterial(3, "week3.pdf", []string{"TLB TLB TLB TLB"})
	e.writeWAV(l, "wav")
	stg := e.stage(e.deps(nil), model.StageASR)
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	var job map[string]any
	b, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), "job.json"))
	e.must(err)
	e.must(json.Unmarshal(b, &job))
	if got, want := job["initial_prompt"], "Belady, TLB"; got != want {
		t.Errorf("initial_prompt %q, want %q (week 2's slides first)", got, want)
	}
	h, err := stg.InputHash(e.ctx, l.ID)
	if err != nil || h == "" {
		t.Fatalf("InputHash %q %v", h, err)
	}
	weekMaterial(2, "more.pdf", []string{"TLB TLB TLB TLB TLB"})
	if h2, _ := stg.InputHash(e.ctx, l.ID); h2 != h {
		t.Error("a week-bound material changed the asr input hash")
	}
}

// renumberCourse is a course with a term start and three lectures in week 2
// (W02L1, W02L2, W02L3), each with a summary and a title job; exported
// marks its citations as exported (P13.6).
func (e *env) renumberCourse(exported bool) []model.Lecture {
	e.t.Helper()
	e.organizerRoles()
	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	var ls []model.Lecture
	for i, at := range []string{"2026-10-05T09:00", "2026-10-06T09:00", "2026-10-07T09:00"} {
		l := e.titleLecture(string(rune('a'+i)) + ".mp4")
		e.must(e.st.SetLectureRecordedAt(e.ctx, l.ID, &at, model.RecordedAtUser))
		ls = append(ls, l)
	}
	if exported {
		e.must(e.st.SetKV(e.ctx, model.KVCiteExportPrefix+e.course.ID, "2026-10-20T09:00:00Z"))
	}
	return ls
}

// TestTitleKindRenumberNotice (lead ruling on K2's report): a kind set by
// the title task is applied as usual; when the course's citations were
// exported and another lecture's citation label changed, a notice
// calendar.renumbered_auto {lecture, n, changes: [{lecture_id, from, to}]}
// is recorded; never when only the lecture's own label changed or when
// nothing was exported. The renumbered lectures and the course are
// announced (SSE changed) either way.
func TestTitleKindRenumberNotice(t *testing.T) {
	cases := []struct {
		name     string
		exported bool
		target   int // the lecture made a tutorial
		notice   bool
	}{
		{"exported, others renumbered", true, 1, true},
		{"exported, only itself", true, 2, false},
		{"not exported, others renumbered", false, 1, false},
		{"not exported, only itself", false, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ls := e.renumberCourse(c.exported)
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind(model.KindTutorial) })
			if r := e.runTitle(e.deps(nil), ls[c.target].ID); r.err != nil {
				t.Fatal(r.err)
			}
			if k, _ := e.kindOf(ls[c.target].ID); k != model.KindTutorial {
				t.Fatalf("kind not applied: %q", k)
			}
			ns, err := RenumberNotices(e.ctx, e.st, e.course.ID)
			e.must(err)
			if !c.notice {
				if len(ns) != 0 {
					t.Errorf("notice recorded: %+v", ns)
				}
			} else {
				if len(ns) != 1 {
					t.Fatalf("notices %+v", ns)
				}
				n := ns[0]
				b, _ := json.Marshal(n.Params)
				want := `{"changes":[{"from":"W02L3","lecture_id":"` + ls[2].ID + `","to":"W02L2"}],"lecture":"` + ls[1].ID + `","n":1}`
				if n.Code != CodeRenumberedAuto || string(b) != want || n.At == "" {
					t.Errorf("notice %s %s at %q, want %s", n.Code, b, n.At, want)
				}
				if n.Detail != "自动识别讲座类型后，另有 1 节讲座的编号发生变化（W02L3 → W02L2），之前导出的引用将与新编号不一致" {
					t.Errorf("detail %q", n.Detail)
				}
			}
			if !e.ev.has("course", e.course.ID) {
				t.Error("course not announced")
			}
			if c.target == 1 && !e.ev.has("lecture", ls[2].ID) {
				t.Error("the renumbered lecture was not announced")
			}
		})
	}

	// notices accumulate (oldest first) until dismissed
	e := newEnv(t)
	ls := e.renumberCourse(true)
	kind := model.KindTutorial
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string { return llmtest.TitleAnswerWithKind(kind) })
	for _, i := range []int{0, 1} {
		if r := e.runTitle(e.deps(nil), ls[i].ID); r.err != nil {
			t.Fatal(r.err)
		}
	}
	ns, err := RenumberNotices(e.ctx, e.st, e.course.ID)
	e.must(err)
	if len(ns) != 2 || ns[0].Params["lecture"] != ls[0].ID || ns[1].Params["lecture"] != ls[1].ID {
		t.Fatalf("notices %+v", ns)
	}
	// a's run: b W02L2 → W02L1, c W02L3 → W02L2; b's run: c W02L2 → W02L1
	if n0, n1 := ns[0].Params["n"], ns[1].Params["n"]; n0 != float64(2) || n1 != float64(1) {
		t.Errorf("notice n %v, %v", n0, n1)
	}
	e.must(DismissRenumberNotices(e.ctx, e.st, e.course.ID))
	if ns, _ := RenumberNotices(e.ctx, e.st, e.course.ID); len(ns) != 0 {
		t.Errorf("after dismiss: %+v", ns)
	}
}

// TestFilenameRuleKindMapOnlyCapturedCodes (lead ruling after the real
// check of prompt version 3): kind_map entries for codes that the kind
// group captured in no sample are dropped (a later "P01" meaning practical
// must not become a past lecture); the retry feedback says so; without a
// kind group the map is still ignored as a whole.
func TestFilenameRuleKindMapOnlyCapturedCodes(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	ls := e.uploadPanopto() // codes L and T only
	km := map[string]string{"L": "lecture", "T": "tutorial", "S": "seminar", "P": "past_lecture"}
	onlyL := strings.Replace(llmtest.PanoptoRegex, "(?P<kind>[A-Z]+)", "(?P<kind>L)", 1) // 2 of 4 names: rejected
	n := 0
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string {
		n++
		if n == 1 {
			return llmtest.FilenameRuleAnswer(onlyL, km)
		}
		return llmtest.FilenameRuleAnswer(llmtest.PanoptoRegex, km)
	})
	if r := e.runFilenameRule(e.deps(nil)); r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleFilenameRule)
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	for _, c := range []string{`"P" → "past_lecture"`, `"S" → "seminar"`, `"T" → "tutorial"`} {
		want := "- kind_map entry " + c + ` was dropped: no file name has the code "` + c[1:2] + `" in its kind group.`
		if !strings.Contains(fb, want) {
			t.Errorf("feedback lacks %q:\n%s", want, fb)
		}
	}
	if strings.Contains(fb, `"L" → "lecture" was dropped`) {
		t.Errorf("a captured code was dropped:\n%s", fb)
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if len(rule.KindMap) != 2 || rule.KindMap["L"] != model.KindLecture || rule.KindMap["T"] != model.KindTutorial {
		t.Errorf("kind_map %v, want only L and T", rule.KindMap)
	}
	if k, _ := e.kindOf(ls[1].ID); k != model.KindTutorial {
		t.Errorf("T: %q", k)
	}

	// the pure function: case-insensitive match, nil when nothing is left
	rows := []calendar.Parsed{{KindCode: "l"}, {KindCode: ""}, {KindCode: "T"}}
	got, notes := dropUncapturedCodes(map[string]string{"L": "lecture", "T": "tutorial", "P": "past_lecture"}, rows)
	if len(got) != 2 || got["L"] != "lecture" || got["T"] != "tutorial" || len(notes) != 1 || !strings.Contains(notes[0], `"P"`) {
		t.Errorf("got %v, notes %v", got, notes)
	}
	if got, notes := dropUncapturedCodes(map[string]string{"P": "past_lecture"}, nil); got != nil || len(notes) != 1 {
		t.Errorf("nothing captured: %v, %v", got, notes)
	}

	// without a kind group the map is ignored as a whole (unchanged)
	e2 := newEnv(t)
	e2.organizerRoles()
	e2.uploadPanopto()
	noGroup := strings.Replace(llmtest.PanoptoRegex, "(?P<kind>[A-Z]+)", "[A-Z]+", 1)
	e2.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return llmtest.FilenameRuleAnswer(noGroup, km) })
	if r := e2.runFilenameRule(e2.deps(nil)); r.err != nil {
		t.Fatal(r.err)
	}
	if raw := e2.courseRow().FilenameRule; raw == nil || strings.Contains(*raw, "kind_map") {
		t.Errorf("rule %v", raw)
	}
}
