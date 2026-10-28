// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package calendar

import (
	"reflect"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// Week overrides (user request 2026-10-27, course-ux).

func TestOverrideConstants(t *testing.T) {
	if OverrideMakeup != model.WeekOverrideMakeup || OverrideSupplement != model.WeekOverrideSupplement {
		t.Fatalf("calendar %q/%q, model %q/%q", OverrideMakeup, OverrideSupplement, model.WeekOverrideMakeup, model.WeekOverrideSupplement)
	}
}

// overrideCourse: term start Mon 2026-09-28 (week 1), no skipped weeks.
// Week 3 is 12–18 Oct, week 5 is 26 Oct–1 Nov.
func overrideCourse() []LectureTime {
	return []LectureTime{
		{ID: "w3a", RecordedAt: tp(at(2026, time.October, 13, 12, 35)), Kind: KindLecture, KindSource: KindSourceFilename},
		{ID: "w3b", RecordedAt: tp(at(2026, time.October, 14, 9, 35)), Kind: KindLecture, KindSource: KindSourceFilename},
		{ID: "w3t", RecordedAt: tp(at(2026, time.October, 16, 14, 0)), Kind: KindTutorial, KindSource: KindSourceFilename},
		{ID: "w5a", RecordedAt: tp(at(2026, time.October, 27, 12, 35)), Kind: KindLecture, KindSource: KindSourceFilename},
		{ID: "w5x", RecordedAt: tp(at(2026, time.October, 27, 15, 0)), Kind: KindLecture, KindSource: KindSourceFilename},
		{ID: "w5b", RecordedAt: tp(at(2026, time.October, 28, 9, 35)), Kind: KindLecture, KindSource: KindSourceFilename},
		{ID: "m3", RecordedAt: tp(at(2026, time.October, 14, 16, 20)), Kind: KindMaterial, KindSource: KindSourceUser},
		{ID: "p24", RecordedAt: tp(at(2024, time.October, 16, 9, 35)), Kind: KindPastLecture, KindSource: KindSourceLLM},
	}
}

func overrideTerm() *time.Time { return tp(day(2026, time.September, 30)) }

func byID(nums []Numbered) map[string]Numbered {
	out := map[string]Numbered{}
	for _, n := range nums {
		out[n.ID] = n
	}
	return out
}

func set(ls []LectureTime, id, ov string, week int) []LectureTime {
	out := append([]LectureTime(nil), ls...)
	for i := range out {
		if out[i].ID == id {
			out[i].Override, out[i].OverrideWeek = ov, week
		}
	}
	return out
}

func labels(nums []Numbered) map[string]string {
	out := map[string]string{}
	for _, n := range nums {
		out[n.ID] = n.Label
	}
	return out
}

func TestOverrideNone(t *testing.T) {
	got := byID(LabelKinds(overrideCourse(), overrideTerm(), nil, NumberOptions{}))
	want := map[string]string{"w3a": "W03L1", "w3b": "W03L2", "w3t": "W03T1", "w5a": "W05L1", "w5x": "W05L2", "w5b": "W05L3", "m3": "M1", "p24": "P1"}
	for id, l := range want {
		if got[id].Label != l || got[id].Override != "" {
			t.Errorf("%s: %s %q, want %s", id, got[id].Label, got[id].Override, l)
		}
	}
}

// A make-up is numbered in its week after the week's own lectures of its
// kind — also when it was recorded earlier (in advance) — and leaves the
// week it was recorded in; the week's own numbers do not move.
func TestOverrideMakeup(t *testing.T) {
	ls := set(overrideCourse(), "w5x", OverrideMakeup, 3)
	got := byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{}))
	want := map[string]string{"w3a": "W03L1", "w3b": "W03L2", "w3t": "W03T1", "w5x": "W03L3", "w5a": "W05L1", "w5b": "W05L2", "m3": "M1", "p24": "P1"}
	if l := labels(mapVals(got)); !reflect.DeepEqual(l, want) {
		t.Fatalf("labels %v, want %v", l, want)
	}
	x := got["w5x"]
	if x.Week != 3 || x.N != 3 || !x.OK || x.Override != OverrideMakeup || x.Kind != KindLecture || x.KindN != 3 || x.KindCount != 3 || !x.ShowN || x.CiteLabel != "W03L3" {
		t.Fatalf("make-up %+v", x)
	}
	if w := got["w3a"]; w.KindCount != 3 || w.Override != "" {
		t.Fatalf("week 3 lecture %+v", w)
	}

	// recorded in week 1 (in advance): still after week 3's own lectures
	early := []LectureTime{{ID: "adv", RecordedAt: tp(at(2026, time.September, 29, 9, 0)), Kind: KindLecture, KindSource: KindSourceUser, Override: OverrideMakeup, OverrideWeek: 3}}
	got = byID(LabelKinds(append(overrideCourse(), early...), overrideTerm(), nil, NumberOptions{}))
	if got["adv"].Label != "W03L3" || got["w3a"].Label != "W03L1" || got["w3b"].Label != "W03L2" {
		t.Fatalf("advance make-up %v", labels(mapVals(got)))
	}

	// two make-ups of one week: in list order, after the week's own
	ls = set(set(overrideCourse(), "w5b", OverrideMakeup, 3), "w5x", OverrideMakeup, 3)
	got = byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{}))
	if got["w5x"].Label != "W03L3" || got["w5b"].Label != "W03L4" || got["w5a"].Label != "W05L1" {
		t.Fatalf("two make-ups %v", labels(mapVals(got)))
	}

	// a tutorial make-up counts with the week's tutorials
	ls = append(overrideCourse(), LectureTime{ID: "t", RecordedAt: tp(at(2026, time.October, 30, 10, 0)), Kind: KindTutorial, KindSource: KindSourceFilename, Override: OverrideMakeup, OverrideWeek: 3})
	if got := byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{})); got["t"].Label != "W03T2" || got["w3t"].Label != "W03T1" {
		t.Fatalf("tutorial make-up %v", labels(mapVals(got)))
	}
}

// A make-up is a session of its week: a past lecture or a material becomes
// a lecture, and the past-lecture rule does not apply to it.
func TestOverrideMakeupKinds(t *testing.T) {
	ls := set(set(overrideCourse(), "p24", OverrideMakeup, 5), "m3", OverrideMakeup, 2)
	got := byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{}))
	if p := got["p24"]; p.Kind != KindLecture || p.Label != "W05L4" || p.Week != 5 || p.Override != OverrideMakeup {
		t.Fatalf("past lecture make-up %+v", p)
	}
	if m := got["m3"]; m.Kind != KindLecture || m.Label != "W02L1" || m.Week != 2 {
		t.Fatalf("material make-up %+v", m)
	}
	// an old recording stored as a lecture (llm): the past-lecture rule
	// would make it P1; as a make-up it is week 5's lecture
	old := []LectureTime{{ID: "old", RecordedAt: tp(at(2025, time.October, 1, 9, 0)), Kind: KindLecture, KindSource: KindSourceLLM}}
	if n := byID(LabelKinds(old, overrideTerm(), nil, NumberOptions{}))["old"]; n.Kind != KindPastLecture {
		t.Fatalf("without the override the past-lecture rule applies: %+v", n)
	}
	old[0].Override, old[0].OverrideWeek = OverrideMakeup, 5
	if n := byID(LabelKinds(old, overrideTerm(), nil, NumberOptions{}))["old"]; n.Kind != KindLecture || n.Label != "W05L1" {
		t.Fatalf("old recording as make-up %+v", n)
	}
	// a custom session kind keeps its code
	kinds := append(BuiltinKinds(), Kind{ID: "lab", Code: "B", Group: GroupSession})
	lab := []LectureTime{{ID: "lab1", RecordedAt: tp(at(2026, time.November, 20, 9, 0)), Kind: "lab", KindSource: KindSourceUser, Override: OverrideMakeup, OverrideWeek: 4}}
	if n := byID(LabelKinds(lab, overrideTerm(), nil, NumberOptions{Kinds: kinds}))["lab1"]; n.Label != "W04B1" || n.Kind != "lab" {
		t.Fatalf("custom kind make-up %+v", n)
	}
}

// A supplement keeps its material number, moves to its week (KindN with
// that week's materials), and takes no number among the week's sessions.
func TestOverrideSupplement(t *testing.T) {
	ls := set(set(overrideCourse(), "m3", OverrideSupplement, 5), "p24", OverrideSupplement, 5)
	got := byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{}))
	want := map[string]string{"w3a": "W03L1", "w3b": "W03L2", "w3t": "W03T1", "w5a": "W05L1", "w5x": "W05L2", "w5b": "W05L3", "m3": "M1", "p24": "P1"}
	if l := labels(mapVals(got)); !reflect.DeepEqual(l, want) {
		t.Fatalf("labels %v, want %v", l, want)
	}
	m := got["m3"]
	if m.Week != 5 || m.Override != OverrideSupplement || m.Kind != KindMaterial || !m.OK || m.N != 1 || m.KindN != 1 || m.KindCount != 1 {
		t.Fatalf("material supplement %+v", m)
	}
	p := got["p24"]
	if p.Week != 5 || p.Kind != KindPastLecture || p.Label != "P1" || p.KindN != 1 || p.KindCount != 1 || p.CiteLabel != "P1" {
		t.Fatalf("past lecture supplement %+v", p)
	}
	// a lecture whose kind is a session kind (changed after it was hung
	// under a week) is numbered as a material: material numbers follow the
	// list order (m3 was recorded first), KindN counts week 3's materials
	ls = set(overrideCourse(), "w5x", OverrideSupplement, 3)
	got = byID(LabelKinds(ls, overrideTerm(), nil, NumberOptions{}))
	if x := got["w5x"]; x.Kind != KindMaterial || x.Label != "M2" || x.Week != 3 || x.KindN != 2 || x.KindCount != 2 || got["m3"].Label != "M1" || got["w5b"].Label != "W05L2" {
		t.Fatalf("session supplement %+v, others %v", x, labels(mapVals(got)))
	}
}

// Without a term start there are no teaching weeks: an override changes
// nothing. Invalid overrides are ignored.
func TestOverrideNotApplied(t *testing.T) {
	plain := LabelKinds(overrideCourse(), nil, nil, NumberOptions{})
	ls := set(set(overrideCourse(), "w5x", OverrideMakeup, 3), "m3", OverrideSupplement, 5)
	if got := LabelKinds(ls, nil, nil, NumberOptions{}); !reflect.DeepEqual(got, plain) {
		t.Fatalf("without a term start:\n%+v\n%+v", got, plain)
	}
	withTerm := LabelKinds(overrideCourse(), overrideTerm(), nil, NumberOptions{})
	for _, bad := range [][2]any{{OverrideMakeup, 0}, {"moved", 3}, {"", 3}} {
		ls := set(overrideCourse(), "w5x", bad[0].(string), bad[1].(int))
		if got := LabelKinds(ls, overrideTerm(), nil, NumberOptions{}); !reflect.DeepEqual(got, withTerm) {
			t.Errorf("override %v applied", bad)
		}
	}
}

// Every lecture keeps a unique citation label with overrides.
func TestOverrideCiteLabelsUnique(t *testing.T) {
	ls := set(set(set(overrideCourse(), "w5x", OverrideMakeup, 3), "w5b", OverrideMakeup, 3), "p24", OverrideSupplement, 3)
	seen := map[string]bool{}
	for _, n := range LabelKinds(ls, overrideTerm(), nil, NumberOptions{}) {
		if seen[n.CiteLabel] {
			t.Fatalf("duplicate %s", n.CiteLabel)
		}
		seen[n.CiteLabel] = true
	}
}

func mapVals(m map[string]Numbered) []Numbered {
	out := make([]Numbered, 0, len(m))
	for _, n := range m {
		out = append(out, n)
	}
	return out
}
