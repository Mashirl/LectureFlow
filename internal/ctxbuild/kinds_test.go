// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// Lecture kinds (user request 2026-10-23, lead freeze .claude/kinds-brief.md;
// K2): the kind catalog in the organizer requests, the kind_map feedback and
// the week-bound slides of the whisper prompt.

func intp(n int) *int { return &n }

// testKinds is the built-in catalog plus one user-defined kind of each group.
func testKinds() []model.LectureKind {
	lab, archive := "Lab  session", "Exam archive"
	return []model.LectureKind{
		{ID: model.KindLecture, Builtin: true, Code: "L", Group: model.KindGroupSession, Order: 10},
		{ID: model.KindTutorial, Builtin: true, Code: "T", Group: model.KindGroupSession, Order: 20},
		{ID: model.KindSeminar, Builtin: true, Code: "S", Group: model.KindGroupSession, Order: 30},
		{ID: model.KindPastLecture, Builtin: true, Code: "P", Group: model.KindGroupMaterial, Order: 40},
		{ID: model.KindMaterial, Builtin: true, Code: "M", Group: model.KindGroupMaterial, Order: 50},
		{ID: "01JLAB", Code: "LAB", Name: &lab, Group: model.KindGroupSession, Order: 60},
		{ID: "01JARC", Code: "EA", Name: &archive, Group: model.KindGroupMaterial, Order: 70},
	}
}

func TestKindMeaning(t *testing.T) {
	ks := testKinds()
	want := []string{
		"a regular lecture of the course",
		"a tutorial, problem class or exercise session",
		"a seminar or discussion session",
		"a recording of this course from an earlier year",
		"supplementary material, not a class session",
		`user-defined kind named "Lab session" (a class session)`,
		`user-defined kind named "Exam archive" (supplementary material)`,
	}
	for i, k := range ks {
		if got := KindMeaning(k); got != want[i] {
			t.Errorf("%s: %q, want %q", k.ID, got, want[i])
		}
	}
	// a user-defined kind without a name is described by its code; a
	// custom kind never takes a built-in meaning
	if got := KindMeaning(model.LectureKind{ID: "lecture", Code: "X", Group: model.KindGroupSession}); got != `user-defined kind named "X" (a class session)` {
		t.Errorf("unnamed: %q", got)
	}
}

// TestTitleRequestKinds: the catalog follows the other sections as
// "id | meaning" lines; without a catalog the request is that of P13.5.
func TestTitleRequestKinds(t *testing.T) {
	req := TitleRequest(TitleInput{Summary: "S.", OriginalFilename: "CSC1002-T01-02.mp4", Kinds: testKinds()[:2]})
	want := "SUMMARY:\nS.\n\nORIGINAL FILE NAME:\nCSC1002-T01-02.mp4\n\n" +
		"LECTURE KINDS (answer one id), one per line as: id | meaning\n" +
		"lecture | a regular lecture of the course\n" +
		"tutorial | a tutorial, problem class or exercise session"
	if req.User != want {
		t.Errorf("user:\n got %q\nwant %q", req.User, want)
	}
	if req.System != TitleSystem {
		t.Error("system is not the title prompt")
	}
	all := TitleRequest(TitleInput{Summary: "S.", Kinds: testKinds()})
	for _, s := range []string{"\n01JLAB | user-defined kind named \"Lab session\" (a class session)", "\npast_lecture | a recording of this course from an earlier year"} {
		if !strings.Contains(all.User, s) {
			t.Errorf("user lacks %q:\n%s", s, all.User)
		}
	}
	if strings.Contains(all.User, " | LAB | ") {
		t.Error("the title request shows the codes")
	}
}

// TestFilenameRuleRequestKinds: the catalog with codes stands between the
// term start and the files.
func TestFilenameRuleRequestKinds(t *testing.T) {
	req := FilenameRuleRequest(FilenameRuleInput{
		CourseCode: "CSC1002",
		Samples:    []FilenameSample{{Filename: "CSC1002-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4", UploadedAt: "2026-10-20 14:03"}},
		Kinds:      testKinds(),
	})
	want := "COURSE CODE: CSC1002\nTERM START (Monday of teaching week 1): (none)\n\n" +
		"LECTURE KINDS (the ids for kind_map), one per line as: id | code | meaning\n" +
		"lecture | L | a regular lecture of the course\n" +
		"tutorial | T | a tutorial, problem class or exercise session\n" +
		"seminar | S | a seminar or discussion session\n" +
		"past_lecture | P | a recording of this course from an earlier year\n" +
		"material | M | supplementary material, not a class session\n" +
		"01JLAB | LAB | user-defined kind named \"Lab session\" (a class session)\n" +
		"01JARC | EA | user-defined kind named \"Exam archive\" (supplementary material)\n\n" +
		"FILES (1), one per line as"
	if !strings.HasPrefix(req.User, want) {
		t.Errorf("user:\n got %q\nwant prefix %q", req.User, want)
	}
	if strings.HasSuffix(req.User, "\n") {
		t.Error("trailing newline")
	}
	if bare := FilenameRuleRequest(FilenameRuleInput{Samples: req2Samples()}); strings.Contains(bare.User, "LECTURE KINDS") {
		t.Errorf("an empty catalog must be left out: %q", bare.User)
	}
}

func req2Samples() []FilenameSample {
	return []FilenameSample{{Filename: "a.mp4", UploadedAt: "2026-10-20 14:03"}}
}

// TestPromptVersions: the organizer prompts changed for lecture kinds are
// version 2, the filename rule's version 3 (v2 G5.5: version 1 is the
// SPEC-v2 text); version 3 asks for a kind group that accepts any code.
func TestPromptVersions(t *testing.T) {
	if FilenameRulePromptVersion != 3 || TitlePromptVersion != 2 {
		t.Errorf("versions %d, %d", FilenameRulePromptVersion, TitlePromptVersion)
	}
	if !strings.Contains(FilenameRuleSystem, "accepts any such code (for example (?P<kind>[A-Z]+))") {
		t.Error("the filename rule prompt does not ask for a general kind group")
	}
	if strings.Contains(FilenameRuleSystem, "extension") {
		t.Error("the file extension is not part of the approved change")
	}
	for name, p := range map[string]string{"FilenameRuleSystem": FilenameRuleSystem, "TitleSystem": TitleSystem} {
		if !strings.Contains(p, "kind") || !strings.Contains(p, "LECTURE KINDS") {
			t.Errorf("%s does not ask for the kind", name)
		}
	}
}

// TestFilenameRuleProblemsKindMap: dropped kind_map entries are told after
// the other problems of a rejected rule.
func TestFilenameRuleProblemsKindMap(t *testing.T) {
	r := FilenameRuleRejection{Regex: "x", Matched: 1, Total: 3, MinMatchPercent: 60, Unmatched: []string{"b.mp4", "c.mp4"},
		KindMap: []string{FilenameKindMapUnknownKind("P", "practical"), FilenameKindMapEmptyCode("lecture"), FilenameKindMapUnmapped([]string{"W", "X"})}}
	got := FilenameRuleProblems(r)
	want := []string{
		"Your regex was: x",
		"It matches only 1 of 3 file names; at least 60% must match.",
		"File names it does not match:\n  b.mp4\n  c.mp4",
		`kind_map entry "P" → "practical" was dropped: "practical" is not one of the LECTURE KINDS ids.`,
		`kind_map entry "" → "lecture" was dropped: the code is empty.`,
		`kind_map does not map the captured code(s) "W", "X".`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if s := FilenameKindMapNoGroup(); !strings.Contains(s, "(?P<kind>…)") {
		t.Errorf("no group: %q", s)
	}
	if s := FilenameKindMapNotObject(); !strings.Contains(s, "JSON object") {
		t.Errorf("not an object: %q", s)
	}
}

// TestBoundMaterialTextWeek: the whisper prompt orders by the slides bound
// to the lecture and to its teaching week (lead ruling for K2), never by
// other weeks' or course-wide slides; without a week only the lecture's.
func TestBoundMaterialTextWeek(t *testing.T) {
	ms := []model.Material{
		{ID: "m2", BindWeek: intp(3), CreatedAt: "2026-01-02T00:00:00Z", PagesMD: []string{"Week three"}},
		{ID: "m0", CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"Course-wide"}},
		{ID: "m5", BindWeek: intp(4), CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"Week four"}},
		{ID: "m1", LectureID: strp("L1"), CreatedAt: "2026-01-03T00:00:00Z", PagesMD: []string{"Own"}},
		{ID: "m3", LectureID: strp("L2"), CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{"Other lecture"}},
	}
	if got, want := BoundMaterialTextWeek(ms, "L1", 3), "Week three\nOwn\n"; got != want {
		t.Errorf("week 3: got %q want %q", got, want)
	}
	if got, want := BoundMaterialTextWeek(ms, "L1", 0), "Own\n"; got != want {
		t.Errorf("no week: got %q want %q", got, want)
	}
	if got := BoundMaterialText(ms, "L1"); got != "Own\n" {
		t.Errorf("BoundMaterialText: %q", got)
	}
	if got := BoundMaterialTextWeek(ms, "L9", 5); got != "" {
		t.Errorf("nothing bound: %q", got)
	}
}
