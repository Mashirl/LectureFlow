// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
)

// Week overrides (course-ux) from the database: numbering, the week of the
// material set and the snapshot's slides follow the override; without a
// term start nothing changes.
func TestWeekOverrideFromStore(t *testing.T) {
	f := newFixture(t)
	l3, mk, l5, mat := f.lecture(), f.lecture(), f.lecture(), f.lecture()
	f.recorded(l3.ID, "2026-10-13T12:35")  // week 3
	f.recorded(mk.ID, "2026-10-27T15:00")  // week 5, the make-up of week 3
	f.recorded(l5.ID, "2026-10-28T09:35")  // week 5
	f.recorded(mat.ID, "2026-10-14T16:20") // week 3, hung under week 5
	f.st.SetLectureKind(f.ctx, mat.ID, model.KindMaterial, model.KindSourceUser)
	f.boundMaterial("Week3.pdf", nil, ip(3))
	f.boundMaterial("Week5.pdf", nil, ip(5))
	if err := f.st.SetWeekOverride(f.ctx, mk.ID, model.WeekOverride{Week: 3, Kind: model.WeekOverrideMakeup}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetWeekOverride(f.ctx, mat.ID, model.WeekOverride{Week: 5, Kind: model.WeekOverrideSupplement}); err != nil {
		t.Fatal(err)
	}
	kinds, _ := f.st.ListLectureKinds(f.ctx)

	number := func() map[string]string {
		c, _ := f.st.GetCourse(f.ctx, f.course.ID)
		ls, _ := f.st.ListLectures(f.ctx, f.course.ID)
		out := map[string]string{}
		for _, n := range NumberCourse(c, ls, kinds) {
			out[n.ID] = n.Label + "/" + n.Override
		}
		return out
	}
	weeks := func() map[string]int {
		c, _ := f.st.GetCourse(f.ctx, f.course.ID)
		out := map[string]int{}
		for _, id := range []string{l3.ID, mk.ID, l5.ID, mat.ID} {
			l, _ := f.st.GetLecture(f.ctx, id)
			out[id] = LectureWeek(c, l)
		}
		return out
	}

	// no term start: overrides change nothing
	if got := number(); got[mk.ID] != "W??/" || got[mat.ID] != "M1/" {
		t.Fatalf("without a term start %v", got)
	}
	if w := weeks(); w[mk.ID] != 0 || w[mat.ID] != 0 {
		t.Fatalf("weeks without a term start %v", w)
	}

	f.setTerm("2026-09-28")
	got := number()
	want := map[string]string{l3.ID: "W03L1/", mk.ID: "W03L2/makeup", l5.ID: "W05L1/", mat.ID: "M1/supplement"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %s, want %s", id, got[id], w)
		}
	}
	if w := weeks(); w[l3.ID] != 3 || w[mk.ID] != 3 || w[l5.ID] != 5 || w[mat.ID] != 5 {
		t.Fatalf("LectureWeek %v", w)
	}

	sets, err := CourseMaterialSets(f.ctx, f.st, f.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := useIDs(sets[mk.ID]); s != "Week3.pdf:week" {
		t.Errorf("make-up set %s", s)
	}
	if s := useIDs(sets[mat.ID]); s != "Week5.pdf:week" {
		t.Errorf("supplement set %s", s)
	}
	snap, err := LoadSnapshot(f.ctx, f.st, mk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := ctxbuild.LectureSlides(snap); !strings.Contains(s, "slides of Week3.pdf") || strings.Contains(s, "Week5.pdf") {
		t.Errorf("make-up slides %q", s)
	}

	// undo: back to the recording week
	if err := f.st.DeleteWeekOverride(f.ctx, mk.ID); err != nil {
		t.Fatal(err)
	}
	if got := number(); got[mk.ID] != "W05L1/" || got[l5.ID] != "W05L2/" {
		t.Fatalf("after undo %v", got)
	}
}

func TestOverrideConflicts(t *testing.T) {
	sup := &model.WeekOverride{Week: 2, Kind: model.WeekOverrideSupplement}
	mk := &model.WeekOverride{Week: 2, Kind: model.WeekOverrideMakeup}
	cases := []struct {
		o     *model.WeekOverride
		group string
		want  bool
	}{
		{nil, model.KindGroupSession, false},
		{sup, model.KindGroupSession, true},
		{sup, model.KindGroupMaterial, false},
		{mk, model.KindGroupMaterial, true},
		{mk, model.KindGroupSession, false},
	}
	for _, c := range cases {
		if got := OverrideConflicts(c.o, c.group); got != c.want {
			t.Errorf("%+v %s: %v", c.o, c.group, got)
		}
	}
	kinds := []model.LectureKind{{ID: model.KindLecture, Group: model.KindGroupSession}, {ID: model.KindMaterial, Group: model.KindGroupMaterial}}
	if KindGroupOf(kinds, model.KindMaterial) != model.KindGroupMaterial || KindGroupOf(kinds, "gone") != model.KindGroupSession {
		t.Errorf("KindGroupOf")
	}
}
