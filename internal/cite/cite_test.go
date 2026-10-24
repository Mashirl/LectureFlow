// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package cite

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
)

var update = flag.Bool("update", false, "rewrite testdata/cite/*.golden.json")

func strp(s string) *string { return &s }

// golden compares got with ../../testdata/cite/{name} byte for byte.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "cite", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cite -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func encode(t *testing.T, d Doc) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := Encode(&b, d); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var genAt = time.Date(2026, 10, 23, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))

// numbered is a lecture's calendar number as calendar.LabelKinds returns it
// for a lecture of kind lecture.
func numbered(id string, week, n int) calendar.Numbered {
	return calendar.Numbered{ID: id, Week: week, N: n, OK: true, Label: calendar.FormatLabel(week, n, true),
		Kind: calendar.KindLecture, Code: "L", Group: calendar.GroupSession, KindN: n}
}

// unnumbered is a lecture without a calendar number ("W??").
func unnumbered(id string) calendar.Numbered {
	return calendar.Numbered{ID: id, Label: calendar.Unknown, Kind: calendar.KindLecture, Code: "L", Group: calendar.GroupSession}
}

// cryptoLecture is the lecture-level fixture: a course with a code, one
// lecture (W02L1) with every sentence case of A11.3, and the numbers of all
// the course's lectures.
func cryptoLecture() (model.Course, map[string]calendar.Numbered, []LectureInput, []model.Term) {
	course := model.Course{ID: "01JCOURSECRYPTO", Name: "Cryptography", Code: strp("CSC1003"), GlossaryVersion: 4}
	lec := model.Lecture{ID: "01JLECTURE03", CourseID: course.ID, Title: "Block Ciphers and Modes", Seq: 3, CreatedAt: "2026-10-01T09:00:00Z",
		RecordedAt: strp("2026-10-06T09:05"), RecordedAtSource: model.RecordedAtFilename, TitleZh: strp("分组密码与工作模式"), TitleSource: model.TitleSourceLLM,
		BlurbEn: strp("How block ciphers work and how modes chain blocks."), BlurbZh: strp("分组密码的原理，以及工作模式如何连接各个分组。"),
		OriginalFilename: "CSC1003_2026-10-06_0905.mp4"}
	ok := func(s string) (*string, string) { return strp(s), model.ZhOK }
	ss := []model.Sentence{
		// out of order on purpose: Build sorts by idx
		{Idx: 2, StartMS: 754300, EndMS: 756000, Text: "Same second, different sentence.", Zh: strp("同一秒的另一句。"), ZhStatus: model.ZhQAFailed},
		{Idx: 0, StartMS: 0, EndMS: 3800, Text: "Okay, let's begin with x < y && y > z.", ZhStatus: model.ZhOK},
		{Idx: 1, StartMS: 754000, EndMS: 758600, Text: "A block cipher works on fixed-size blocks.", HumanText: strp("A block cipher works on fixed-size blocks."), ZhStatus: model.ZhOK},
		{Idx: 3, StartMS: 3725000, EndMS: 3729999, Text: "Past the hour: AES rounds.", ZhStatus: model.ZhNone},
		{Idx: 4, StartMS: 3729999, EndMS: 3735000, Text: "A stale translation is not cited.", Zh: strp("旧译文。"), ZhStatus: model.ZhStale},
		{Idx: 5, StartMS: 3735000, EndMS: 3741200, Text: "Reset edit.", HumanText: strp(""), ZhStatus: model.ZhOK},
	}
	ss[1].Zh, ss[1].ZhStatus = ok("好，我们从 x < y && y > z 开始。")
	ss[2].Zh, ss[2].ZhStatus = ok("分组密码处理固定大小的分组。")
	ss[5].Zh = strp("重置的修改。")
	terms := []model.Term{
		{En: "block cipher", EnKey: "block cipher", Zh: "分组密码", Enabled: true},
		{En: "DES", EnKey: "des", Zh: "", Enabled: false},
		{En: "AES", EnKey: "aes", Zh: "", Enabled: true},
	}
	numbers := map[string]calendar.Numbered{
		lec.ID:          numbered(lec.ID, 2, 1),
		"01JLECTURE04":  numbered("01JLECTURE04", 2, 2),
		"01JLECTUREXX1": unnumbered("01JLECTUREXX1"),
	}
	return course, numbers, []LectureInput{{Lecture: lec, Summary: "The lecture introduces block ciphers.", Sentences: ss}}, terms
}

// TestLectureGolden: course with a code, calendar label, id and ref format
// of P13.6, the new lecture fields, same-second refs, h:mm:ss, edited, zh
// null for qa_failed / none / stale, enabled terms only, no HTML escaping.
func TestLectureGolden(t *testing.T) {
	course, numbers, lecs, terms := cryptoLecture()
	got := encode(t, Build(course, nil, numbers, lecs, terms, genAt))
	golden(t, "lecture.golden.json", got)

	var d Doc
	if err := json.Unmarshal(got, &d); err != nil {
		t.Fatal(err)
	}
	if d.Schema != "lectureflow.cite/v3" || !strings.Contains(d.HowToCite, "[CSC1003 W02L1@12:34]") {
		t.Errorf("schema %q how_to_cite %q", d.Schema, d.HowToCite)
	}
	l := d.Lectures[0]
	if l.Label != "W02L1" || l.Kind != "lecture" || l.Week == nil || *l.Week != 2 || l.N == nil || *l.N != 1 || l.RecordedAt == nil || *l.RecordedAt != "2026-10-06T09:05" ||
		l.TitleZh == nil || *l.TitleZh != "分组密码与工作模式" || l.BlurbEn == nil || l.BlurbZh == nil {
		t.Errorf("lecture fields %+v", l)
	}
	if strings.Contains(string(got), `"seq"`) {
		t.Error("P13.6: seq is removed from the lecture object")
	}
	ss := l.Sentences
	if ss[1].Ref != "CSC1003 W02L1@12:34" || ss[2].Ref != ss[1].Ref || ss[1].ID == ss[2].ID {
		t.Errorf("same-second refs: %q %q ids %q %q", ss[1].Ref, ss[2].Ref, ss[1].ID, ss[2].ID)
	}
	if ss[1].ID != "CSC1003.W02L1.s0002" || ss[3].Ref != "CSC1003 W02L1@1:02:05" {
		t.Errorf("id %q ref %q", ss[1].ID, ss[3].Ref)
	}
	if !ss[1].Edited || ss[0].Edited || ss[5].Edited {
		t.Error("edited must follow human_text being non-empty")
	}
	for _, i := range []int{2, 3, 4} {
		if ss[i].Zh != nil {
			t.Errorf("sentence %d: zh must be null unless zh_status is ok", i)
		}
	}
	if l.DurationMS != 3741200 || d.GeneratedAt != "2026-10-23T12:00:00Z" {
		t.Errorf("duration %d generated_at %q", l.DurationMS, d.GeneratedAt)
	}
	if !strings.Contains(string(got), "x < y && y > z") || !bytes.HasSuffix(got, []byte("}\n")) {
		t.Error("output must not escape HTML and must end with a newline")
	}
}

// courseFixture is the course-level fixture: no course code, lectures given
// out of order, numbered and unnumbered ones (before the term start, no
// recording time), and unnumbered lecture ids sharing their first 9
// characters with a lecture that is not exported.
func courseFixture() (model.Course, map[string]calendar.Numbered, []LectureInput) {
	course := model.Course{ID: "01JCOURSEARCH", Name: "Computer Architecture", Code: strp("  ")}
	l12 := model.Lecture{ID: "01JLEC12", CourseID: course.ID, Title: "Virtual Memory", Seq: 4, CreatedAt: "2026-10-02T09:00:00Z",
		RecordedAt: strp("2026-10-13T10:00"), OriginalFilename: "ARCH_13Oct.mp4"}
	l2a := model.Lecture{ID: "01JLEC02", CourseID: course.ID, Title: "Caches", Seq: 2, CreatedAt: "2026-10-03T09:00:00Z",
		RecordedAt: strp("2026-09-30T00:00"), OriginalFilename: "ARCH_30Sep_b.mp4"}
	l2b := model.Lecture{ID: "01JLEC01", CourseID: course.ID, Title: "Pipelines", Seq: 3, CreatedAt: "2026-10-04T09:00:00Z",
		RecordedAt: strp("2026-09-30T00:00"), OriginalFilename: "ARCH_30Sep_a.mp4"}
	early := model.Lecture{ID: "01JK8ZQ5BBBB", CourseID: course.ID, Title: "Welcome", Seq: 1, CreatedAt: "2026-10-05T09:00:00Z",
		RecordedAt: strp("2026-09-01T09:00"), OriginalFilename: "ARCH_01Sep.mp4"}
	undated := model.Lecture{ID: "01JK8ZQ5AAAA", CourseID: course.ID, Title: "Empty", Seq: 5, CreatedAt: "2026-10-01T09:00:00Z"}
	lecs := []LectureInput{
		{Lecture: l12, Summary: "Paging and TLBs.", Sentences: []model.Sentence{
			{Idx: 0, StartMS: 61000, EndMS: 64000, Text: "The TLB caches translations.", Zh: strp("TLB 缓存地址转换。"), ZhStatus: model.ZhOK},
		}},
		{Lecture: undated},
		{Lecture: l2a, Summary: "Cache basics.", Sentences: []model.Sentence{
			{Idx: 0, StartMS: 5000, EndMS: 9000, Text: "A cache is small and fast.", Zh: strp("缓存小而快。"), ZhStatus: model.ZhOK},
			{Idx: 1, StartMS: 9000, EndMS: 12000, Text: "Misses are expensive.", ZhStatus: model.ZhQAFailed, Zh: strp("缺失很贵")},
		}},
		{Lecture: early, Summary: "Course logistics.", Sentences: []model.Sentence{
			{Idx: 0, StartMS: 1000, EndMS: 2000, Text: "Welcome.", Zh: strp("欢迎。"), ZhStatus: model.ZhOK},
		}},
		{Lecture: l2b, Summary: "Pipelining.", Sentences: []model.Sentence{
			{Idx: 0, StartMS: 0, EndMS: 1000, Text: "Five stages.", Zh: strp("五级。"), ZhStatus: model.ZhOK},
		}},
	}
	numbers := map[string]calendar.Numbered{
		l2b.ID:         numbered(l2b.ID, 1, 1),
		l2a.ID:         numbered(l2a.ID, 1, 2),
		l12.ID:         numbered(l12.ID, 3, 1),
		early.ID:       unnumbered(early.ID),
		undated.ID:     unnumbered(undated.ID),
		"01JK8ZQ5AXYZ": unnumbered("01JK8ZQ5AXYZ"), // a draft that is not exported
	}
	return course, numbers, lecs
}

// TestCourseGolden: no course code (abbreviation, code null), lectures
// sorted by recorded_at (same day by file name, none last), W?? labels
// unique across the course, a lecture without sentences, empty glossary.
func TestCourseGolden(t *testing.T) {
	course, numbers, lecs := courseFixture()
	got := encode(t, Build(course, nil, numbers, lecs, nil, time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)))
	golden(t, "course.golden.json", got)

	var d Doc
	if err := json.Unmarshal(got, &d); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, l := range d.Lectures {
		labels = append(labels, l.Label)
	}
	// 01JK8ZQ5AAAA and the draft 01JK8ZQ5AXYZ share 9 characters: every W?? label grows to 10
	want := []string{"W??-01JK8ZQ5BB", "W01L1", "W01L2", "W03L1", "W??-01JK8ZQ5AA"}
	if !slices.Equal(labels, want) {
		t.Errorf("labels in export order %v, want %v", labels, want)
	}
	if d.Course.Code != nil || d.Lectures[1].Sentences[0].Ref != "CA W01L1@00:00" || d.Lectures[3].Sentences[0].ID != "CA.W03L1.s0001" {
		t.Errorf("course part without a code: %+v", d.Lectures[1].Sentences[0])
	}
	if d.Lectures[0].Sentences[0].ID != "CA.W??-01JK8ZQ5BB.s0001" || d.Lectures[0].Sentences[0].Ref != "CA W??-01JK8ZQ5BB@00:01" {
		t.Errorf("unnumbered lecture sentence %+v", d.Lectures[0].Sentences[0])
	}
	if l := d.Lectures[0]; l.Week != nil || l.N != nil || l.RecordedAt == nil || *l.RecordedAt != "2026-09-01T09:00" {
		t.Errorf("unnumbered lecture before the term: week %v n %v recorded_at %v", l.Week, l.N, l.RecordedAt)
	}
	if l := d.Lectures[1]; l.RecordedAt == nil || *l.RecordedAt != "2026-09-30" {
		t.Errorf("a date without a time is exported as a date: %v", l.RecordedAt)
	}
	if l := d.Lectures[4]; l.RecordedAt != nil || l.DurationMS != 0 || l.Sentences == nil || l.TitleZh != nil || d.Glossary == nil {
		t.Error("lecture without recording time / sentences: recorded_at null, duration 0, sentences and glossary []")
	}
	if !strings.Contains(d.HowToCite, "[CA W??-01JK8ZQ5BB@12:34]") {
		t.Errorf("how_to_cite %q", d.HowToCite)
	}
}

// lectureKindsTable is the "kinds" table of a document whose lectures are
// all of kind lecture (built-in catalog), as Encode writes it.
const lectureKindsTable = `  "kinds": [
    {
      "id": "lecture",
      "code": "L",
      "name": "Lecture"
    }
  ],
`

// v2Equivalent turns a v3 document of a course of lectures only into what
// v2 wrote: the schema name of v2, without the "kinds" table (exactly
// lectureKindsTable) and without the "kind" and "kind_code" lines of the
// lectures (the only additions).
func v2Equivalent(t *testing.T, v3 []byte) []byte {
	t.Helper()
	if !bytes.Contains(v3, []byte(`"schema": "lectureflow.cite/v3",`)) {
		t.Fatalf("not a v3 document:\n%s", v3)
	}
	out := bytes.Replace(v3, []byte(`"schema": "lectureflow.cite/v3",`), []byte(`"schema": "lectureflow.cite/v2",`), 1)
	if bytes.Count(out, []byte(lectureKindsTable)) != 1 {
		t.Fatalf("no kinds table of lectures only:\n%s", v3)
	}
	out = bytes.Replace(out, []byte(lectureKindsTable), nil, 1)
	var kept [][]byte
	for _, line := range bytes.SplitAfter(out, []byte("\n")) {
		if l := bytes.TrimSpace(line); bytes.Equal(l, []byte(`"kind": "lecture",`)) || bytes.Equal(l, []byte(`"kind_code": "L",`)) {
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, nil)
}

// TestSchema3KeepsLectureCourses (lecture kinds, kinds-brief 1.4): for a
// course whose lectures are all of kind lecture, the v3 document is the v2
// document byte for byte except the schema name, the "kinds" table and the
// added "kind" and "kind_code" (review item 7) —
// the v2 golden files (written by the code before lecture kinds) are the
// reference. The course fixture is numbered by calendar.LabelKinds itself
// from the recording times and the term start (week 1 = 28 Sep), with
// stored kinds from every source that leaves a lecture a lecture.
func TestSchema3KeepsLectureCourses(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cite", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	course, numbers, lecs, terms := cryptoLecture()
	if got := v2Equivalent(t, encode(t, Build(course, nil, numbers, lecs, terms, genAt))); !bytes.Equal(got, read("lecture.v2.golden.json")) {
		t.Errorf("lecture document differs from v2:\n%s", got)
	}

	course2, hand, lecs2 := courseFixture()
	term := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	sources := []struct{ kind, source string }{{"", calendar.KindSourceNone}, {calendar.KindLecture, calendar.KindSourceFilename},
		{calendar.KindLecture, calendar.KindSourceLLM}, {calendar.KindLecture, calendar.KindSourceUser}, {"", ""}}
	var times []calendar.LectureTime
	for i, in := range lecs2 {
		l := in.Lecture
		var rec *time.Time
		if t, ok := recordedAt(l); ok {
			rec = &t
		}
		src := sources[i%len(sources)]
		times = append(times, calendar.LectureTime{ID: l.ID, RecordedAt: rec, OriginalFilename: l.OriginalFilename, Kind: src.kind, KindSource: src.source})
	}
	times = append(times, calendar.LectureTime{ID: "01JK8ZQ5AXYZ"}) // the draft that is not exported
	numbers2 := map[string]calendar.Numbered{}
	for _, n := range calendar.LabelKinds(times, &term, nil, calendar.NumberOptions{}) {
		numbers2[n.ID] = n
		if h := hand[n.ID]; h.Label != n.Label || h.Week != n.Week || h.N != n.N || h.OK != n.OK || n.Kind != calendar.KindLecture {
			t.Errorf("%s: LabelKinds %+v, fixture %+v", n.ID, n, h)
		}
	}
	at := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	if got := v2Equivalent(t, encode(t, Build(course2, nil, numbers2, lecs2, nil, at))); !bytes.Equal(got, read("course.v2.golden.json")) {
		t.Errorf("course document differs from v2:\n%s", got)
	}
}

// TestSchema3Kinds: labels per kind (W02T1 beside W02L1; P1, M1 within the
// course), kind on every lecture, week for any kind with a teaching week,
// n = the number in the label; refs and ids use the new labels.
func TestSchema3Kinds(t *testing.T) {
	course := model.Course{ID: "01JCOURSEK", Name: "Cryptography", Code: strp("CSC1003")}
	term := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	mk := func(id, rec, file, kind, src string) (model.Lecture, calendar.LectureTime) {
		l := model.Lecture{ID: id, CourseID: course.ID, Title: id, CreatedAt: "2026-10-01T09:00:00Z", OriginalFilename: file}
		lt := calendar.LectureTime{ID: id, OriginalFilename: file, Kind: kind, KindSource: src}
		if rec != "" {
			l.RecordedAt = strp(rec)
			t0, err := calendar.ParseRecordedAt(rec, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			lt.RecordedAt = &t0
		}
		return l, lt
	}
	rows := [][5]string{
		{"01JL1", "2026-10-06T09:00", "L01-01.mp4", "", calendar.KindSourceNone},                        // W02L1
		{"01JT1", "2026-10-07T13:00", "T01-01.mp4", calendar.KindTutorial, calendar.KindSourceFilename}, // W02T1
		{"01JT2", "2026-10-08T13:00", "T01-02.mp4", calendar.KindTutorial, calendar.KindSourceUser},     // W02T2
		{"01JP1", "2024-10-07T09:00", "old.mp4", calendar.KindLecture, calendar.KindSourceFilename},     // P1: recorded two years before
		{"01JM1", "2026-10-09T10:00", "extra.mp4", calendar.KindMaterial, calendar.KindSourceUser},      // M1, in week 2
	}
	var inputs []LectureInput
	var times []calendar.LectureTime
	for _, r := range rows {
		l, lt := mk(r[0], r[1], r[2], r[3], r[4])
		inputs = append(inputs, LectureInput{Lecture: l, Sentences: []model.Sentence{{Idx: 0, StartMS: 1000, EndMS: 2000, Text: "Hi."}}})
		times = append(times, lt)
	}
	numbers := map[string]calendar.Numbered{}
	for _, n := range calendar.LabelKinds(times, &term, nil, calendar.NumberOptions{}) {
		numbers[n.ID] = n
	}
	d := Build(course, nil, numbers, inputs, nil, genAt)
	type want struct {
		label, kind, code string
		week, n           int // 0 = null
	}
	wants := map[string]want{
		"01JL1": {"W02L1", "lecture", "L", 2, 1},
		"01JT1": {"W02T1", "tutorial", "T", 2, 1},
		"01JT2": {"W02T2", "tutorial", "T", 2, 2},
		"01JP1": {"P1", "past_lecture", "P", 0, 1},
		"01JM1": {"M1", "material", "M", 2, 1},
	}
	ptr := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, l := range d.Lectures {
		w := wants[l.ID]
		if l.Label != w.label || l.Kind != w.kind || l.KindCode != w.code || ptr(l.Week) != w.week || ptr(l.N) != w.n {
			t.Errorf("%s: label %s kind %s/%s week %v n %v, want %+v", l.ID, l.Label, l.Kind, l.KindCode, l.Week, l.N, w)
		}
		if s := l.Sentences[0]; s.Ref != "CSC1003 "+w.label+"@00:01" || s.ID != "CSC1003."+w.label+".s0001" {
			t.Errorf("%s: ref %q id %q", l.ID, s.Ref, s.ID)
		}
	}
	if d.Schema != Schema || d.Lectures[0].ID != "01JP1" {
		t.Errorf("schema %s, first lecture %s (by recording time)", d.Schema, d.Lectures[0].ID)
	}
	// the kinds of the document's lectures, in catalog order (no seminar)
	wantKinds := []Kind{{"lecture", "L", "Lecture"}, {"tutorial", "T", "Tutorial"},
		{"past_lecture", "P", "Past lecture (teaching material)"}, {"material", "M", "Additional material"}}
	if !reflect.DeepEqual(d.Kinds, wantKinds) {
		t.Errorf("kinds %+v, want %+v", d.Kinds, wantKinds)
	}
}

// TestKindsTable (review item 7): "kinds" lists each kind of the exported
// lectures once, in catalog order (ord): a built-in kind by its English
// name whatever the catalog row says, a custom kind by the user's name
// (trimmed); a custom kind deleted in between keeps its code, named by its
// id; kind_code on every lecture, "W??-..." ones included.
func TestKindsTable(t *testing.T) {
	course := model.Course{ID: "01JCOURSEK", Name: "Cryptography", Code: strp("CSC1003")}
	term := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	lab := "  实验课 "
	catalog := []model.LectureKind{
		{ID: "01JKINDLAB", Code: "LAB", Name: &lab, Group: calendar.GroupSession, Order: 15},
		{ID: calendar.KindTutorial, Builtin: true, Code: "T", Group: calendar.GroupSession, Order: 20},
		{ID: calendar.KindLecture, Builtin: true, Code: "L", Group: calendar.GroupSession, Order: 10},
		{ID: calendar.KindMaterial, Builtin: true, Code: "M", Name: strp("ignored"), Group: calendar.GroupMaterial, Order: 50},
	}
	kinds := []calendar.Kind{{ID: "01JKINDLAB", Code: "LAB", Group: calendar.GroupSession}, {ID: "01JKINDOLD", Code: "X", Group: calendar.GroupSession}}
	for _, k := range catalog[1:] {
		kinds = append(kinds, calendar.Kind{ID: k.ID, Code: k.Code, Group: k.Group})
	}
	rec := func(s string) *time.Time {
		v, _ := calendar.ParseRecordedAt(s, time.UTC)
		return &v
	}
	times := []calendar.LectureTime{
		{ID: "01JA", RecordedAt: rec("2026-10-06T09:00"), Kind: "01JKINDLAB", KindSource: calendar.KindSourceUser},
		{ID: "01JB", RecordedAt: rec("2026-10-07T09:00"), Kind: calendar.KindMaterial, KindSource: calendar.KindSourceUser},
		{ID: "01JC", RecordedAt: rec("2026-10-08T09:00"), Kind: "01JKINDOLD", KindSource: calendar.KindSourceUser}, // deleted after numbering
		{ID: "01JD"}, // no recording time: W??
	}
	numbers := map[string]calendar.Numbered{}
	for _, n := range calendar.LabelKinds(times, &term, nil, calendar.NumberOptions{Kinds: kinds}) {
		numbers[n.ID] = n
	}
	var inputs []LectureInput
	for _, lt := range times {
		l := model.Lecture{ID: lt.ID, CourseID: course.ID, Title: lt.ID, CreatedAt: "2026-10-01T09:00:00Z"}
		if lt.RecordedAt != nil {
			l.RecordedAt = strp(lt.RecordedAt.Format(calendar.RecordedAtLayout))
		}
		inputs = append(inputs, LectureInput{Lecture: l})
	}
	inputs = append(inputs, LectureInput{Lecture: model.Lecture{ID: "01JE", CourseID: course.ID, Title: "not numbered"}}) // missing from numbers
	d := Build(course, catalog, numbers, inputs, nil, genAt)
	got := map[string]string{}
	for _, l := range d.Lectures {
		got[l.ID] = l.Label + " " + l.Kind + " " + l.KindCode
	}
	want := map[string]string{
		"01JA": "W02LAB1 01JKINDLAB LAB", "01JB": "M1 material M", "01JC": "W02X1 01JKINDOLD X",
		"01JD": "W??-01JD lecture L", "01JE": "W??-01JE lecture L",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lectures %v, want %v", got, want)
	}
	wantKinds := []Kind{{"lecture", "L", "Lecture"}, {"01JKINDLAB", "LAB", "实验课"}, {"material", "M", "Additional material"}, {"01JKINDOLD", "X", "01JKINDOLD"}}
	if !reflect.DeepEqual(d.Kinds, wantKinds) {
		t.Errorf("kinds %+v, want %+v", d.Kinds, wantKinds)
	}
	// without lectures: an empty table, not null
	if e := Build(course, catalog, numbers, nil, nil, genAt); e.Kinds == nil || len(e.Kinds) != 0 {
		t.Errorf("empty document kinds %#v", e.Kinds)
	}
	var b strings.Builder
	if err := Encode(&b, Build(course, nil, nil, nil, nil, genAt)); err != nil || !strings.Contains(b.String(), `"kinds": [],`) {
		t.Errorf("empty kinds encoded as %s (%v)", b.String(), err)
	}
	// every built-in kind has an English name
	for _, k := range calendar.BuiltinKinds() {
		if strings.TrimSpace(BuiltinKindNames[k.ID]) == "" {
			t.Errorf("built-in kind %s has no name", k.ID)
		}
	}
}

// TestCiteLabelsUniqueAndStable (P13.7): W?? labels and sentence ids are
// unique in the course document, and a lecture exported on its own carries
// the same label and ids as in the course document.
func TestCiteLabelsUniqueAndStable(t *testing.T) {
	course, numbers, lecs := courseFixture()
	full := Build(course, nil, numbers, lecs, nil, genAt)
	ids := map[string]bool{}
	labels := map[string]bool{}
	byID := map[string]Lecture{}
	for _, l := range full.Lectures {
		if labels[l.Label] {
			t.Errorf("duplicate label %s", l.Label)
		}
		labels[l.Label] = true
		byID[l.ID] = l
		for _, s := range l.Sentences {
			if ids[s.ID] {
				t.Errorf("duplicate sentence id %s", s.ID)
			}
			ids[s.ID] = true
		}
	}
	for _, in := range lecs {
		one := Build(course, nil, numbers, []LectureInput{in}, nil, genAt)
		got, want := one.Lectures[0], byID[in.Lecture.ID]
		if got.Label != want.Label || len(got.Sentences) != len(want.Sentences) {
			t.Errorf("%s: lecture-level label %q, course-level %q", in.Lecture.ID, got.Label, want.Label)
			continue
		}
		for i := range got.Sentences {
			if got.Sentences[i].ID != want.Sentences[i].ID || got.Sentences[i].Ref != want.Sentences[i].Ref {
				t.Errorf("%s: sentence %d differs: %+v vs %+v", in.Lecture.ID, i, got.Sentences[i], want.Sentences[i])
			}
		}
	}
	// a lecture missing from numbers is unnumbered, still unique
	extra := LectureInput{Lecture: model.Lecture{ID: "01JK8ZQ5BBBC", CourseID: course.ID, Title: "Late upload"}}
	d := Build(course, nil, numbers, append(lecs, extra), nil, genAt)
	seen := map[string]bool{}
	for _, l := range d.Lectures {
		if seen[l.Label] {
			t.Errorf("duplicate label %s with a lecture missing from numbers", l.Label)
		}
		seen[l.Label] = true
	}
}

// duration_ms is the media length when the caller knows it (the lecture
// page's Lecture.duration_ms), else the last sentence's end.
func TestDurationIsMediaLength(t *testing.T) {
	course, numbers, lecs, terms := cryptoLecture()
	lastEnd := Build(course, nil, numbers, lecs, terms, genAt).Lectures[0].DurationMS
	if lastEnd != 3741200 {
		t.Fatalf("without the media: %d", lastEnd)
	}
	lecs[0].MediaMS = 3900000 // the recording goes on after the last sentence
	if got := Build(course, nil, numbers, lecs, terms, genAt).Lectures[0].DurationMS; got != 3900000 {
		t.Errorf("with the media: %d", got)
	}
	lecs[0].Sentences = nil
	if got := Build(course, nil, numbers, lecs, terms, genAt).Lectures[0].DurationMS; got != 3900000 {
		t.Errorf("media without sentences: %d", got)
	}
}

func TestRefTimeAndLabel(t *testing.T) {
	cases := map[int64]string{
		0: "00:00", 999: "00:00", 59999: "00:59", 754000: "12:34", 3599999: "59:59",
		3600000: "1:00:00", 3725000: "1:02:05", 36000000: "10:00:00", -5: "00:00",
	}
	for ms, want := range cases {
		if got := RefTime(ms); got != want {
			t.Errorf("RefTime(%d) = %q, want %q", ms, got, want)
		}
	}
	for seq, want := range map[int]string{1: "L01", 3: "L03", 12: "L12", 105: "L105"} {
		if got := Label(seq); got != want { // deprecated v1.1 label, still used by API views
			t.Errorf("Label(%d) = %q", seq, got)
		}
	}
	if got := SentenceID("CSC1003", "W02L1", 122); got != "CSC1003.W02L1.s0123" {
		t.Errorf("SentenceID = %q", got)
	}
}

func TestCoursePart(t *testing.T) {
	cases := []struct {
		name string
		code *string
		want string
	}{
		{"Cryptography", strp("CSC1003"), "CSC1003"},
		{"Cryptography", strp(" CSC1003 "), "CSC1003"},
		{"Computer Architecture", nil, "CA"},
		{"computer architecture", strp(""), "CA"},
		{"Data Structures & Algorithms", nil, "DSA"},
		{"Object-Oriented Programming", nil, "OOP"},
		{"Machine Learning 2", nil, "ML2"},
		{"Women's Studies", nil, "WS"},
		{"计算机 体系结构", nil, "计体"},
		{"Écoles normales", nil, "ÉN"},
		{"  --  ", nil, fallbackCoursePart},
	}
	for _, c := range cases {
		if got := CoursePart(model.Course{Name: c.name, Code: c.code}); got != c.want {
			t.Errorf("CoursePart(%q, %v) = %q, want %q", c.name, c.code, got, c.want)
		}
	}
}

// TestBuildDoesNotMutateInput: Build sorts copies.
func TestBuildDoesNotMutateInput(t *testing.T) {
	course, numbers, lecs, terms := cryptoLecture()
	first := lecs[0].Sentences[0].Idx
	n := len(numbers)
	Build(course, nil, numbers, lecs, terms, genAt)
	if lecs[0].Sentences[0].Idx != first || terms[0].En != "block cipher" || len(numbers) != n {
		t.Error("Build reordered its input")
	}
}

// TestPackageIsPure: no I/O, clock or randomness (same rule as ctxbuild).
// "time" is imported for the time.Time parameter only; reading the clock is
// forbidden.
func TestPackageIsPure(t *testing.T) {
	forbidden := []string{
		"os", "os/exec", "io/ioutil", "io/fs", "net", "net/http", "math/rand", "math/rand/v2",
		"crypto/rand", "database/sql", "syscall", "log", "log/slog", "path/filepath", "runtime",
		"lectureflow/internal/db", "lectureflow/internal/llm", "lectureflow/internal/sched",
	}
	clock := []string{"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "Tick", "NewTimer", "NewTicker"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if slices.Contains(forbidden, p) || strings.HasPrefix(p, "net/") {
				t.Errorf("%s imports %s (no I/O in this package)", f, p)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && slices.Contains(clock, sel.Sel.Name) {
					t.Errorf("%s calls time.%s (the generation time is a parameter)", f, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
