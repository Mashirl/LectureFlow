// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// Tests of lecture kinds, per-kind numbering, the file-name kind group,
// material sets and week suggestions (lead freeze .claude/kinds-brief.md, K1).

// The calendar constants mirror model's (calendar may not import model).
func TestModelConstants(t *testing.T) {
	pairs := [][2]string{
		{KindLecture, model.KindLecture}, {KindTutorial, model.KindTutorial}, {KindSeminar, model.KindSeminar},
		{KindPastLecture, model.KindPastLecture}, {KindMaterial, model.KindMaterial},
		{GroupSession, model.KindGroupSession}, {GroupMaterial, model.KindGroupMaterial},
		{KindSourceNone, model.KindSourceNone}, {KindSourceLLM, model.KindSourceLLM},
		{KindSourceFilename, model.KindSourceFilename}, {KindSourceUser, model.KindSourceUser},
		{NumberingAuto, model.NumberingAuto}, {NumberingAlways, model.NumberingAlways}, {NumberingNever, model.NumberingNever},
		{BindLecture, model.BindLecture}, {BindWeek, model.BindWeek}, {BindCourse, model.BindCourse},
		{SourceUser, model.RecordedAtUser}, {SourceFilename, model.RecordedAtFilename},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("calendar %q != model %q", p[0], p[1])
		}
	}
	var ids []string
	for _, k := range BuiltinKinds() {
		ids = append(ids, k.ID)
		if !model.ValidKindCode(k.Code) || !model.ValidKindGroup(k.Group) {
			t.Errorf("built-in kind %+v", k)
		}
	}
	if !slices.Equal(ids, model.BuiltinKindIDs) {
		t.Errorf("BuiltinKinds %v, model %v", ids, model.BuiltinKindIDs)
	}
	if MaxSuggestedWeek > model.MaxBindWeek {
		t.Errorf("a suggested week may exceed MaxBindWeek")
	}
}

// labCatalog is the built-in catalog plus a custom session kind.
func labCatalog() []Kind {
	return append(BuiltinKinds(), Kind{ID: "lab", Code: "LAB", Group: GroupSession})
}

func TestEffectiveKind(t *testing.T) {
	termStart := day(2026, 9, 28)
	inTerm := at(2026, 10, 7, 9, 35)
	d30 := at(2026, 8, 29, 9, 35) // 30 days before the Monday: not past
	d31 := at(2026, 8, 28, 9, 35) // 31 days before: past
	lastYear := at(2025, 10, 7, 9, 35)
	cases := []struct {
		name         string
		kind, source string
		rec, term    *time.Time
		kinds        []Kind
		want         string
	}{
		{"undecided", "", KindSourceNone, &inTerm, &termStart, nil, KindLecture},
		{"unknown id", "deleted", KindSourceFilename, &inTerm, &termStart, nil, KindLecture},
		{"tutorial", KindTutorial, KindSourceFilename, &inTerm, &termStart, nil, KindTutorial},
		{"llm seminar", KindSeminar, KindSourceLLM, &inTerm, &termStart, nil, KindSeminar},
		{"custom", "lab", KindSourceUser, &inTerm, &termStart, labCatalog(), "lab"},
		{"custom missing from catalog", "lab", KindSourceUser, &inTerm, &termStart, nil, KindLecture},
		{"30 days before", "", KindSourceNone, &d30, &termStart, nil, KindLecture},
		{"31 days before", "", KindSourceNone, &d31, &termStart, nil, KindPastLecture},
		{"last year's tutorial", KindTutorial, KindSourceFilename, &lastYear, &termStart, nil, KindPastLecture},
		{"last year's llm lecture", KindLecture, KindSourceLLM, &lastYear, &termStart, nil, KindPastLecture},
		{"the user's lecture stays", KindLecture, KindSourceUser, &lastYear, &termStart, nil, KindLecture},
		{"material kinds are not affected", KindMaterial, KindSourceLLM, &lastYear, &termStart, nil, KindMaterial},
		{"no term start", KindTutorial, KindSourceFilename, &lastYear, nil, nil, KindTutorial},
		{"no recording time", KindTutorial, KindSourceFilename, nil, &termStart, nil, KindTutorial},
		{"zero term start", KindTutorial, KindSourceFilename, &lastYear, &time.Time{}, nil, KindTutorial},
		{"a custom session kind becomes past too", "lab", KindSourceFilename, &lastYear, &termStart, labCatalog(), KindPastLecture},
		// A catalog without the built-in entries still resolves them.
		{"bare catalog", "", KindSourceNone, &lastYear, &termStart, []Kind{{ID: "lab", Code: "LAB", Group: GroupSession}}, KindPastLecture},
		{"invalid group", "odd", KindSourceUser, &inTerm, &termStart, []Kind{{ID: "odd", Code: "O", Group: "other"}}, KindLecture},
	}
	for _, c := range cases {
		got := EffectiveKind(c.kind, c.source, c.rec, c.term, c.kinds)
		if got.ID != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
		if got.Code == "" || (got.Group != GroupSession && got.Group != GroupMaterial) {
			t.Errorf("%s: incomplete kind %+v", c.name, got)
		}
	}
	if !PastRecording(&d31, &termStart) || PastRecording(&d30, &termStart) || PastRecording(nil, &termStart) || PastRecording(&d31, nil) {
		t.Error("PastRecording")
	}
	// The threshold counts from the Monday of the term start's week.
	if wed := day(2026, 9, 30); PastRecording(&d30, &wed) {
		t.Error("PastRecording must align the term start to its Monday")
	}
}

func TestKindOverrides(t *testing.T) {
	order := []string{KindSourceNone, KindSourceLLM, KindSourceFilename, KindSourceUser}
	for i, newer := range order {
		for j, older := range order {
			if got, want := KindOverrides(newer, older), i >= j; got != want {
				t.Errorf("KindOverrides(%s, %s) = %v", newer, older, got)
			}
		}
	}
	if KindSourceRank("") != 0 || KindSourceRank("guess") != 0 {
		t.Error("unknown sources rank 0")
	}
}

func TestKindLabel(t *testing.T) {
	cases := []struct {
		k       Kind
		week, n int
		want    string
	}{
		{Kind{Code: "L", Group: GroupSession}, 2, 1, "W02L1"},
		{Kind{Code: "T", Group: GroupSession}, 11, 3, "W11T3"},
		{Kind{Code: "LAB", Group: GroupSession}, 3, 1, "W03LAB1"},
		{Kind{Code: "L", Group: GroupSession}, 0, 1, Unknown},
		{Kind{Code: "L", Group: GroupSession}, 2, 0, Unknown},
		{Kind{Code: "P", Group: GroupMaterial}, 0, 1, "P1"},
		{Kind{Code: "M", Group: GroupMaterial}, 4, 12, "M12"},
		{Kind{Code: "M", Group: GroupMaterial}, 4, 0, Unknown},
	}
	for _, c := range cases {
		if got := KindLabel(c.k, c.week, c.n); got != c.want {
			t.Errorf("KindLabel(%+v, %d, %d) = %s, want %s", c.k, c.week, c.n, got, c.want)
		}
	}
	// The lecture kind's label is P13.4's.
	for _, w := range []int{1, 9, 10, 100} {
		for _, n := range []int{1, 2, 10} {
			if KindLabel(Kind{Code: "L", Group: GroupSession}, w, n) != FormatLabel(w, n, true) {
				t.Errorf("KindLabel lecture %d/%d differs from FormatLabel", w, n)
			}
		}
	}
}

// mixedCourse is a CSC1002-like course with every kind: lectures,
// tutorials (three on one Friday, file-name suffixes against time order), a
// seminar set by the user, a custom kind, past lectures (by the rule and by
// the user), additional material with and without a week, a lecture in the
// skipped week, a user's lecture from long before the term, and a lecture
// whose stored kind no longer exists.
func mixedCourse() []LectureTime {
	l := func(id, kind, src string, rec *time.Time, name string) LectureTime {
		return LectureTime{ID: id, Kind: kind, KindSource: src, RecordedAt: rec, OriginalFilename: name}
	}
	return []LectureTime{
		l("a1", KindLecture, KindSourceFilename, tp(at(2026, 9, 29, 13, 35)), "CSC1002-L01-01 Tue Sep 29 13-35-00 BST 2026.mp4"),
		l("a2", KindLecture, KindSourceFilename, tp(at(2026, 10, 6, 13, 35)), "CSC1002-L01-01 Tue Oct 06 13-35-00 BST 2026.mp4"),
		l("a3", KindLecture, KindSourceFilename, tp(at(2026, 10, 13, 13, 35)), "CSC1002-L01-01 Tue Oct 13 13-35-00 BST 2026.mp4"),
		l("t1", KindTutorial, KindSourceFilename, tp(at(2026, 10, 16, 13, 35)), "CSC1002-T01-01 Fri Oct 16 13-35-00 BST 2026.mp4"),
		l("t2", KindTutorial, KindSourceFilename, tp(at(2026, 10, 16, 12, 35)), "CSC1002-T01-02 Fri Oct 16 12-35-00 BST 2026.mp4"),
		l("t3", KindTutorial, KindSourceFilename, tp(at(2026, 10, 16, 9, 35)), "CSC1002-T01-03 Fri Oct 16 09-35-00 BST 2026.mp4"),
		l("s1", KindSeminar, KindSourceUser, tp(at(2026, 10, 14, 15, 0)), "RISC-V guest talk.mp4"),
		l("lab1", "lab", KindSourceLLM, tp(at(2026, 10, 15, 10, 0)), "lab session.mp4"),
		l("p1", KindLecture, KindSourceFilename, tp(at(2025, 10, 7, 13, 35)), "CSC1002-L01-01 Tue Oct 07 13-35-00 BST 2025.mp4"),
		l("p2", KindPastLecture, KindSourceUser, tp(at(2026, 10, 27, 9, 0)), "2024 recording (re-uploaded).mp4"),
		l("m1", KindMaterial, KindSourceLLM, tp(at(2026, 10, 7, 16, 0)), "exam tips.mp4"),
		l("m2", KindMaterial, KindSourceUser, nil, "revision walkthrough.mp4"),
		l("x1", "", KindSourceNone, tp(at(2026, 10, 21, 13, 35)), "CSC1002-L01-01 Wed Oct 21 13-35-00 BST 2026.mp4"),
		l("x2", KindLecture, KindSourceUser, tp(at(2025, 9, 1, 10, 0)), "old intro.mp4"),
		l("a4", KindLecture, KindSourceFilename, tp(at(2026, 10, 27, 13, 35)), "CSC1002-L01-01 Tue Oct 27 13-35-00 GMT 2026.mp4"),
		l("a5", "", KindSourceNone, tp(at(2026, 10, 29, 13, 35)), "CSC1002-L01-01 Thu Oct 29 13-35-00 GMT 2026.mp4"),
		l("u1", "deleted-kind", KindSourceFilename, tp(at(2026, 11, 3, 13, 35)), "CSC1002-X01-01 Tue Nov 03 13-35-00 GMT 2026.mp4"),
	}
}

// renderKinds renders LabelKinds in list order, with ShowN under each mode.
func renderKinds(lectures []LectureTime, termStart *time.Time, skip []time.Time, kinds []Kind) string {
	byMode := map[string][]Numbered{}
	for _, m := range []string{NumberingAuto, NumberingAlways, NumberingNever} {
		byMode[m] = LabelKinds(lectures, termStart, skip, NumberOptions{Kinds: kinds, Numbering: m})
	}
	nums := byMode[NumberingAuto]
	var b strings.Builder
	for seq, i := range Order(lectures) {
		n := nums[i]
		show := ""
		for _, m := range []string{NumberingAuto, NumberingAlways, NumberingNever} {
			show += fmt.Sprintf(" %s=%v", m, byMode[m][i].ShowN)
		}
		fmt.Fprintf(&b, "%2d %-7s cite=%-8s kind=%s/%s/%s w=%d n=%d ok=%v kind_n=%d/%d show:%s  %s  %s\n",
			seq+1, n.Label, n.CiteLabel, n.Kind, n.Code, n.Group, n.Week, n.N, n.OK, n.KindN, n.KindCount, show,
			fmtTime(lectures[i].RecordedAt), lectures[i].OriginalFilename)
	}
	return b.String()
}

func TestLabelKindsGolden(t *testing.T) {
	lectures := mixedCourse()
	termStart := day(2026, 9, 30)
	skip := []time.Time{day(2026, 10, 19)}
	golden(t, "labelkinds_mixed.golden", renderKinds(lectures, &termStart, skip, labCatalog()))

	nums := LabelKinds(lectures, &termStart, skip, NumberOptions{Kinds: labCatalog()})
	want := map[string]string{
		"a1": "W01L1", "a2": "W02L1", "a3": "W03L1", "a4": "W04L1", "a5": "W04L2", "u1": "W05L1",
		"t3": "W03T1", "t2": "W03T2", "t1": "W03T3", // by time, not by the file-name suffix
		"s1": "W03S1", "lab1": "W03LAB1",
		"p1": "P1", "p2": "P2", "m1": "M1", "m2": "M2",
		"x1": Unknown, "x2": Unknown,
	}
	for i, n := range nums {
		if n.ID != lectures[i].ID {
			t.Fatalf("result %d is %s (results are parallel to the input)", i, n.ID)
		}
		if n.Label != want[n.ID] {
			t.Errorf("%s: %s, want %s", n.ID, n.Label, want[n.ID])
		}
		if n.OK != (n.Label != Unknown) {
			t.Errorf("%s: OK=%v with label %s", n.ID, n.OK, n.Label)
		}
	}
	byID := map[string]Numbered{}
	for _, n := range nums {
		byID[n.ID] = n
	}
	// Display counters and the numbering modes.
	if n := byID["t1"]; n.KindN != 3 || n.KindCount != 3 || !n.ShowN {
		t.Errorf("t1 %+v", n)
	}
	if n := byID["a3"]; n.KindN != 1 || n.KindCount != 1 || n.ShowN {
		t.Errorf("a3 %+v: alone in its week and kind", n)
	}
	if n := byID["p2"]; n.Week != 4 || n.KindN != 1 || n.KindCount != 1 || n.N != 2 {
		t.Errorf("p2 %+v: a material kind keeps its week and is numbered in the course", n)
	}
	if n := byID["x1"]; n.Week != 0 || n.KindN != 0 || n.KindCount != 0 || n.ShowN || n.CiteLabel != "W??-x1" {
		t.Errorf("x1 %+v", n)
	}
	always := LabelKinds(lectures, &termStart, skip, NumberOptions{Kinds: labCatalog(), Numbering: NumberingAlways})
	never := LabelKinds(lectures, &termStart, skip, NumberOptions{Kinds: labCatalog(), Numbering: NumberingNever})
	for i := range nums {
		if always[i].ShowN != (always[i].KindN > 0) || never[i].ShowN {
			t.Errorf("%s: always %v never %v", nums[i].ID, always[i].ShowN, never[i].ShowN)
		}
		// The numbering mode is display only: numbers never change.
		a, n := always[i], never[i]
		a.ShowN, n.ShowN = nums[i].ShowN, nums[i].ShowN
		if !reflect.DeepEqual(a, nums[i]) || !reflect.DeepEqual(n, nums[i]) {
			t.Errorf("%s: the numbering mode changed more than ShowN", nums[i].ID)
		}
	}
	// Without a term start: session kinds are W??, material kinds still numbered.
	for _, n := range LabelKinds(lectures, nil, nil, NumberOptions{Kinds: labCatalog()}) {
		material := n.Kind == KindMaterial || n.Kind == KindPastLecture
		if n.Week != 0 || n.OK != material || (material && n.Label == Unknown) || (!material && n.Label != Unknown) {
			t.Errorf("no term start: %+v", n)
		}
	}
}

// legacyLabelAll is LabelAll as it was before lecture kinds (P13.4, commit
// 150e0a6), kept verbatim to prove that a course of plain lectures is
// numbered exactly as before.
func legacyLabelAll(lectures []LectureTime, termStart *time.Time, skipWeeks []time.Time) []Numbered {
	out := make([]Numbered, len(lectures))
	for i, l := range lectures {
		out[i] = Numbered{ID: l.ID, Label: Unknown}
	}
	order, byName := sortLectures(lectures)
	for i, b := range byName {
		out[i].SameDayByName = b
	}
	if termStart == nil || termStart.IsZero() {
		return out
	}
	c := newTerm(*termStart, skipWeeks)
	count := map[int]int{}
	for _, i := range order {
		rec := lectures[i].RecordedAt
		if !dated(rec) {
			continue
		}
		w, ok := c.week(*rec)
		if !ok {
			continue
		}
		count[w]++
		out[i].Week, out[i].N, out[i].OK = w, count[w], true
		out[i].Label = FormatLabel(w, count[w], true)
	}
	return out
}

// A course of plain lectures gets exactly the P13.4 numbers, whatever the
// stored kind's source, under every term start and skipped week of the
// fixtures (no lecture more than 30 days before the term): the golden files
// written before lecture kinds still match.
func TestLabelKindsAllLecturesUnchanged(t *testing.T) {
	sameDay := []LectureTime{
		{ID: "c", RecordedAt: tp(day(2026, 10, 7)), OriginalFilename: "Wk2 Oct 07 part c.mp4"},
		{ID: "a", RecordedAt: tp(day(2026, 10, 7)), OriginalFilename: "Wk2 Oct 07 part a.mp4"},
		{ID: "tA", RecordedAt: tp(at(2026, 10, 14, 14, 0)), OriginalFilename: "A afternoon.mp4"},
		{ID: "tZ", RecordedAt: tp(at(2026, 10, 14, 9, 0)), OriginalFilename: "Z morning.mp4"},
		{ID: "uM", RecordedAt: tp(day(2026, 10, 14)), OriginalFilename: "M unknown.mp4"},
		{ID: "s2", RecordedAt: tp(at(2026, 10, 21, 9, 35)), OriginalFilename: "same b.mp4"},
		{ID: "s1", RecordedAt: tp(at(2026, 10, 21, 9, 35)), OriginalFilename: "same a.mp4"},
		{ID: "n", OriginalFilename: "undated.mp4"},
	}
	fixtures := map[string][]LectureTime{"csc3121": csc3121(), "same_day": sameDay}
	terms := []*time.Time{nil, tp(day(2026, 9, 28)), tp(day(2026, 10, 1)), tp(day(2026, 10, 8)), tp(day(2026, 10, 22))}
	skips := [][]time.Time{nil, {day(2026, 10, 19)}, {day(2026, 9, 28), day(2026, 10, 12)}}
	sources := []string{"", KindSourceNone, KindSourceFilename, KindSourceLLM, KindSourceUser}
	for name, base := range fixtures {
		for ti, ts := range terms {
			for si, sk := range skips {
				want := legacyLabelAll(base, ts, sk)
				wantCite := CiteLabels(want)
				for _, src := range sources {
					in := slices.Clone(base)
					for i := range in {
						in[i].KindSource = src
						if src != "" {
							in[i].Kind = KindLecture
						}
					}
					for _, mode := range []string{"", NumberingAuto, NumberingAlways, NumberingNever} {
						got := LabelKinds(in, ts, sk, NumberOptions{Numbering: mode})
						for i := range got {
							g, w := got[i], want[i]
							if g.ID != w.ID || g.Week != w.Week || g.N != w.N || g.OK != w.OK || g.Label != w.Label ||
								g.SameDayByName != w.SameDayByName || g.CiteLabel != wantCite[i] || g.Kind != KindLecture || g.Code != "L" {
								t.Errorf("%s term %d skip %d source %q mode %q: %s = %+v, want %+v (cite %s)",
									name, ti, si, src, mode, base[i].ID, g, w, wantCite[i])
							}
						}
					}
				}
			}
		}
	}
	// And through LabelAll, the very golden files of P13.4.
	termStart := day(2026, 10, 1)
	lectures := csc3121()
	for i := range lectures {
		lectures[i].Kind, lectures[i].KindSource = KindLecture, KindSourceFilename
	}
	golden(t, "labelall_csc3121.golden", renderNumbered(lectures, LabelAll(lectures, &termStart, []time.Time{day(2026, 10, 19)})))
}

func TestFilenameKindGroup(t *testing.T) {
	up := at(2026, 11, 10, 21, 0)
	rule := FilenameRule{
		Regex:   `CSC1002-(?P<kind>[A-Za-z]+)\d\d-\d\d [A-Z][a-z]{2} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3} (?P<year>\d{4})`,
		KindMap: map[string]string{"L": KindLecture, "T": KindTutorial, "Sem": KindSeminar},
	}
	cases := []struct {
		name, code, kind string
	}{
		{"CSC1002-L01-01 Tue Oct 06 13-35-00 BST 2026.mp4", "L", KindLecture},
		{"CSC1002-T01-03 Fri Oct 16 09-35-00 BST 2026.mp4", "T", KindTutorial},
		{"CSC1002-t01-02 Fri Oct 16 12-35-00 BST 2026.mp4", "t", KindTutorial}, // the same code in another case
		{"CSC1002-SEM01-01 Wed Oct 14 15-00-00 BST 2026.mp4", "SEM", KindSeminar},
		{"CSC1002-X01-01 Tue Nov 03 13-35-00 GMT 2026.mp4", "X", ""}, // not in the map
	}
	r, err := Compile(rule)
	if err != nil {
		t.Fatal(err)
	}
	var samples []Sample
	for _, c := range cases {
		p := r.Parse(Sample{Filename: c.name, UploadedAt: up})
		if p.Status() != StatusOK || p.KindCode != c.code || p.Kind != c.kind {
			t.Errorf("%s: status %s code %q kind %q, want %q %q", c.name, p.Status(), p.KindCode, p.Kind, c.code, c.kind)
		}
		samples = append(samples, Sample{Filename: c.name, UploadedAt: up})
	}
	// ValidateRule rows carry the kind; Validate (a bare regex) has no map.
	parsed, matched, ok, _ := ValidateRule(rule, samples, nil)
	if !ok || matched != len(cases) {
		t.Fatalf("ValidateRule ok=%v matched=%d", ok, matched)
	}
	for i, p := range parsed {
		if p.Kind != cases[i].kind {
			t.Errorf("row %d kind %q", i, p.Kind)
		}
	}
	plain, _, _, _ := Validate(rule.Regex, samples, nil)
	if plain[0].KindCode != "L" || plain[0].Kind != "" {
		t.Errorf("Validate without a map: %+v", plain[0])
	}
	// A rule without a kind group gives no kind.
	if p := Parse(FilenameRule{Regex: csc3631Rule, KindMap: rule.KindMap}, Sample{Filename: cases[0].name, UploadedAt: up}); p.KindCode != "" || p.Kind != "" {
		t.Errorf("no kind group: %+v", p)
	}
	if LookupKind(nil, "L") != "" || LookupKind(rule.KindMap, "") != "" || LookupKind(rule.KindMap, " T ") != KindTutorial ||
		LookupKind(map[string]string{"l": "a", "L": "b"}, "L") != "b" || LookupKind(map[string]string{"l": "a", "Lx": "b"}, "L") != "a" {
		t.Error("LookupKind")
	}

	// kind_map is part of the stored rule; a rule without one keeps its JSON.
	old := `{"regex":"x","year_default":null,"source":"llm","created_at":"2026-10-23T20:00:00Z","samples_matched":8,"samples_total":8}`
	var fr FilenameRule
	if err := json.Unmarshal([]byte(old), &fr); err != nil || fr.KindMap != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(fr); string(b) != old {
		t.Errorf("round trip %s", b)
	}
	fr.KindMap = map[string]string{"T": KindTutorial}
	b, _ := json.Marshal(fr)
	var back FilenameRule
	json.Unmarshal(b, &back)
	if !strings.Contains(string(b), `"kind_map":{"T":"tutorial"}`) || !reflect.DeepEqual(back, fr) {
		t.Errorf("with a kind map: %s", b)
	}
}

// P13.3.2 rule 4 since lecture kinds (lead ruling 1.1): a sample dated more
// than 30 days before the term start is a past lecture, not a failure
// (ValidateRule). The transitional Validate keeps the old rule.
func TestValidatePastSamples(t *testing.T) {
	up := at(2026, 10, 20, 21, 10)
	termStart := day(2026, 9, 28)
	names := []string{
		"CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4",
		"CSC1003-L01-01 Fri Aug 28 09-35-00 BST 2026.mp4", // 31 days before
		"CSC1003-L01-01 Sat Aug 29 09-35-00 BST 2026.mp4", // 30 days before
		"CSC1003-L01-01 Wed Oct 08 09-35-00 BST 2025.mp4", // last year
	}
	var samples []Sample
	for _, n := range names {
		samples = append(samples, Sample{Filename: n, UploadedAt: up})
	}
	rule := FilenameRule{Regex: csc3631Rule}
	parsed, matched, ok, problems := ValidateRule(rule, samples, &termStart)
	if !ok || matched != 4 || len(problems) != 0 {
		t.Fatalf("ok=%v matched=%d problems=%v", ok, matched, problems)
	}
	wantPast := []bool{false, true, false, true}
	for i, p := range parsed {
		if p.Past != wantPast[i] || p.RecordedAt == nil || p.Problem != nil || p.Status() != StatusOK {
			t.Errorf("%s: past=%v status=%s problem=%+v", p.Filename, p.Past, p.Status(), p.Problem)
		}
		if p.Problem != nil && p.Problem.Code == CodeBeforeTerm {
			t.Errorf("%s: CodeBeforeTerm is no longer produced", p.Filename)
		}
	}
	// Without a term start nothing is past.
	parsed, _, _, _ = ValidateRule(rule, samples, nil)
	for _, p := range parsed {
		if p.Past {
			t.Errorf("%s: past without a term start", p.Filename)
		}
	}
	// The transitional Validate still rejects them (rule 4 before lecture
	// kinds) and never marks a row Past.
	legacy, _, ok, problems := Validate(csc3631Rule, samples, &termStart)
	if ok || len(problems) != 2 {
		t.Fatalf("Validate: ok=%v problems=%v", ok, problems)
	}
	for i, p := range legacy {
		before := p.Problem != nil && p.Problem.Code == CodeBeforeTerm
		if p.Past || before != wantPast[i] {
			t.Errorf("Validate %s: past=%v problem=%+v", p.Filename, p.Past, p.Problem)
		}
	}
	// The same lecture is numbered as a past lecture.
	rec := parsed[3].RecordedAt
	if k := EffectiveKind("", KindSourceNone, rec, &termStart, nil); k.ID != KindPastLecture {
		t.Errorf("effective kind %s", k.ID)
	}
	if !slices.Contains(ErrorCodes, CodeBeforeTerm) {
		t.Error("CodeBeforeTerm stays listed while Validate produces it")
	}
}

func TestMaterialSet(t *testing.T) {
	ms := []MaterialBinding{
		{ID: "all1"},
		{ID: "w3a", Week: 3},
		{ID: "lecA", LectureID: "A"},
		{ID: "w4", Week: 4},
		{ID: "lecB", LectureID: "B"},
		{ID: "w3b", Week: 3},
		{ID: "all2"},
	}
	ids := func(us []MaterialUse) string {
		var s []string
		for _, u := range us {
			s = append(s, u.ID+":"+u.Via)
		}
		return strings.Join(s, " ")
	}
	cases := []struct {
		lecture string
		week    int
		want    string
	}{
		{"A", 3, "lecA:lecture w3a:week w3b:week all1:course all2:course"},
		{"B", 4, "lecB:lecture w4:week all1:course all2:course"},
		{"C", 3, "w3a:week w3b:week all1:course all2:course"},
		{"A", 0, "lecA:lecture all1:course all2:course"}, // no week: no week-bound material
		{"D", 9, "all1:course all2:course"},
	}
	for _, c := range cases {
		if got := ids(MaterialSet(ms, c.lecture, c.week)); got != c.want {
			t.Errorf("MaterialSet(%s, %d) = %s, want %s", c.lecture, c.week, got, c.want)
		}
	}
	if got := MaterialSet(nil, "A", 3); got == nil || len(got) != 0 {
		t.Errorf("empty set %#v", got)
	}
	// A binding with both (never stored) counts as the lecture's.
	if got := ids(MaterialSet([]MaterialBinding{{ID: "x", LectureID: "A", Week: 3}}, "B", 3)); got != "" {
		t.Errorf("lecture wins: %s", got)
	}

	// MaterialSets uses each lecture's week from the numbering.
	termStart := day(2026, 9, 28)
	lecs := []LectureTime{
		{ID: "A", RecordedAt: tp(at(2026, 10, 14, 9, 35))},                                                // week 3
		{ID: "C", RecordedAt: tp(at(2026, 10, 16, 9, 35))},                                                // week 3
		{ID: "P", RecordedAt: tp(at(2025, 10, 14, 9, 35))},                                                // last year: no week
		{ID: "M", Kind: KindMaterial, KindSource: KindSourceUser, RecordedAt: tp(at(2026, 10, 21, 9, 0))}, // week 4, material kind
	}
	sets := MaterialSets(ms, LabelAll(lecs, &termStart, nil))
	want := map[string]string{
		"A": "lecA:lecture w3a:week w3b:week all1:course all2:course",
		"C": "w3a:week w3b:week all1:course all2:course",
		"P": "all1:course all2:course",
		"M": "w4:week all1:course all2:course",
	}
	for id, w := range want {
		if got := ids(sets[id]); got != w {
			t.Errorf("set of %s = %s, want %s", id, got, w)
		}
	}

	// SetDiff by id.
	before := MaterialSet(ms, "A", 3)
	after := MaterialSet(ms, "A", 4)
	added, removed := SetDiff(before, after)
	if !slices.Equal(added, []string{"w4"}) || !slices.Equal(removed, []string{"w3a", "w3b"}) {
		t.Errorf("SetDiff = %v, %v", added, removed)
	}
	if a, r := SetDiff(before, before); a != nil || r != nil {
		t.Errorf("same set: %v %v", a, r)
	}
	// Moving a material from the week to the lecture changes only its reason.
	moved := []MaterialUse{{ID: "w3a", Via: BindLecture}, {ID: "w3b", Via: BindWeek}, {ID: "lecA", Via: BindLecture}, {ID: "all1", Via: BindCourse}, {ID: "all2", Via: BindCourse}}
	if a, r := SetDiff(before, moved); a != nil || r != nil {
		t.Errorf("a changed reason is not a change of the set: %v %v", a, r)
	}
}

func TestWeekFromFilename(t *testing.T) {
	names := []string{
		// weeks
		"Week3-Slides.pdf", "week 3.pdf", "Week_03 Block ciphers.pdf", "WEEK-12.pdf", "Week.5 notes.pdf",
		"wk 3.pdf", "Wk03.pdf", "wk_10_tutorial.pdf", "Lecture 2 (Week 4).pdf",
		"W03 - Block ciphers.pdf", "Slides_W3.pdf", "w3.pdf", "W 7 summary.pdf", "CSC1003 W11.pdf",
		"第3周 课件.pdf", "第 03 周.pdf",
		"Week3Slides.pdf", "Week10.pdf", "Week 30.pdf",
		// not weeks
		"StreamCiphers-2.pdf", "Lecture-2.pdf", "ch2.pdf", "Chapter 2.pdf", "SHA-2.pdf", "AES256.pdf",
		"Weekly quiz 3.pdf", "weeks 3-4.pdf", "Network3.pdf", "W3L1.pdf", "w3c-validator.pdf",
		"2026-W40.pdf", "Week 31.pdf", "Week 0.pdf", "Week 100.pdf", "W2026 overview.pdf", "Lectureweek3.pdf",
		"slides.pdf", "",
	}
	var b strings.Builder
	for _, n := range names {
		w, ok := WeekFromFilename(n)
		if ok {
			fmt.Fprintf(&b, "%-28q week %d\n", n, w)
		} else {
			fmt.Fprintf(&b, "%-28q -\n", n)
		}
		if ok != (w > 0) || w > MaxSuggestedWeek {
			t.Errorf("%q: %d %v", n, w, ok)
		}
	}
	golden(t, "week_from_filename.golden", b.String())

	// The examples of the request: Week3, W03, wk 3 are weeks; "-2" and "ch2" are not.
	for name, want := range map[string]int{"Week3.pdf": 3, "W03.pdf": 3, "wk 3.pdf": 3, "StreamCiphers-2.pdf": 0, "ch2.pdf": 0} {
		if w, _ := WeekFromFilename(name); w != want {
			t.Errorf("%s: week %d, want %d", name, w, want)
		}
	}
}

func TestTermParsing(t *testing.T) {
	ts, skips := CourseTerm(sp("2026-09-30"), []string{"2026-10-19", "soon", " 2026-12-21 "}, bst)
	if ts == nil || !ts.Equal(day(2026, 9, 30)) || len(skips) != 2 || !skips[1].Equal(day(2026, 12, 21)) {
		t.Fatalf("CourseTerm = %v %v", ts, skips)
	}
	if ts, skips := CourseTerm(nil, nil, bst); ts != nil || skips == nil || len(skips) != 0 {
		t.Errorf("empty CourseTerm = %v %v", ts, skips)
	}
	if ts, _ := CourseTerm(sp(" "), nil, bst); ts != nil {
		t.Error("blank term start")
	}
	if r := RecordedTime(sp("2026-10-07T09:35"), bst); r == nil || !r.Equal(at(2026, 10, 7, 9, 35)) {
		t.Errorf("RecordedTime = %v", r)
	}
	if RecordedTime(nil, bst) != nil || RecordedTime(sp("yesterday"), bst) != nil || RecordedTime(sp(""), bst) != nil {
		t.Error("RecordedTime of unset or unreadable values")
	}
	term := day(2026, 9, 30)
	skip := []time.Time{day(2026, 10, 19)}
	for _, c := range []struct {
		rec  *time.Time
		week int
		ok   bool
	}{
		{tp(at(2026, 10, 14, 9, 35)), 3, true},
		{tp(at(2026, 10, 21, 9, 35)), 0, false}, // skipped
		{tp(at(2026, 10, 28, 9, 35)), 4, true},
		{tp(at(2026, 9, 27, 9, 35)), 0, false}, // before
		{nil, 0, false},
	} {
		if w, ok := TeachingWeek(c.rec, &term, skip); w != c.week || ok != c.ok {
			t.Errorf("TeachingWeek(%v) = %d %v", c.rec, w, ok)
		}
	}
	if _, ok := TeachingWeek(tp(at(2026, 10, 14, 9, 35)), nil, nil); ok {
		t.Error("TeachingWeek without a term start")
	}
}

func sp(s string) *string { return &s }
