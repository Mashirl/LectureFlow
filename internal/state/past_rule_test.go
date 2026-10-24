// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Tests of the one-time renumbering check of the past-lecture rule
// (lecture kinds review, item 3; past_rule.go).

// pastCourse makes f.course a course with term start 28 Sep 2026: a lecture
// in week 2, a recording of 7 Oct 2025 (P1 by the rule, W??-… before) and
// one of 8 Oct 2025 the user set as a past lecture (P1 before, P2 now: the
// rule renumbers it too). It returns the three.
func (f *fixture) pastCourse() (week2, past, userPast model.Lecture) {
	f.t.Helper()
	f.setTerm("2026-09-28")
	week2, past, userPast = f.lecture(), f.lecture(), f.lecture()
	f.recorded(week2.ID, "2026-10-06T09:00")
	f.recorded(past.ID, "2025-10-07T09:00")
	f.recorded(userPast.ID, "2025-10-08T09:00")
	if err := f.st.SetLectureKind(f.ctx, userPast.ID, model.KindPastLecture, model.KindSourceUser); err != nil {
		f.t.Fatal(err)
	}
	return week2, past, userPast
}

func TestPastRuleCiteChanges(t *testing.T) {
	f := newFixture(t)
	_, past, userPast := f.pastCourse()
	c, _ := f.st.GetCourse(f.ctx, f.course.ID)
	ls, _ := f.st.ListLectures(f.ctx, f.course.ID)
	kinds, _ := f.st.ListLectureKinds(f.ctx)
	got := PastRuleCiteChanges(c, ls, kinds)
	// ordered by the new label; the week-2 lecture keeps W02L1
	if len(got) != 2 || got[0].LectureID != past.ID || got[0].To != "P1" || !strings.HasPrefix(got[0].From, "W??-") ||
		got[1] != (CiteChange{LectureID: userPast.ID, From: "P1", To: "P2"}) {
		t.Fatalf("changes %+v", got)
	}
	info := PastRuleNotice(got)
	if info.Code != CodeRenumberedUpgrade || info.Params["n"] != 2 || !strings.Contains(info.Detail, got[0].From+" → P1、P1 → P2") {
		t.Errorf("notice %+v", info)
	}
	if ch := info.Params["changes"].([]map[string]any); len(ch) != 2 || ch[0]["lecture_id"] != past.ID || ch[0]["from"] != got[0].From || ch[0]["to"] != "P1" ||
		ch[1]["lecture_id"] != userPast.ID {
		t.Errorf("notice changes %+v", ch)
	}
	// without a term start the rule changes nothing
	c.TermStart = nil
	if got := PastRuleCiteChanges(c, ls, kinds); len(got) != 0 {
		t.Errorf("no term start: %+v", got)
	}
}

func TestCheckPastRuleRenumbering(t *testing.T) {
	f := newFixture(t)
	f.pastCourse()
	exported := f.course
	// a course with the same kind of recording, never exported
	other := model.Course{ID: db.NewID(), Name: "Other"}
	if err := f.st.CreateCourse(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	f.course = other
	f.pastCourse()
	// an exported course the rule does not touch
	plain := model.Course{ID: db.NewID(), Name: "Plain"}
	if err := f.st.CreateCourse(f.ctx, plain); err != nil {
		t.Fatal(err)
	}
	f.course = plain
	f.setTerm("2026-09-28")
	f.recorded(f.lecture().ID, "2026-10-06T09:00")
	for _, id := range []string{exported.ID, plain.ID} {
		if err := f.st.SetKV(f.ctx, model.KVCiteExportPrefix+id, "2026-10-20T10:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.SetKV(f.ctx, model.KVCiteExportPrefix+other.ID, ""); err != nil { // cleared
		t.Fatal(err)
	}

	// a failing record leaves nothing done: the next start tries again
	boom := errors.New("boom")
	if _, err := CheckPastRuleRenumbering(f.ctx, f.st, func(context.Context, db.Store, string, model.ErrorInfo) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if v := f.kvValue(KVPastRuleChecked); v != "" {
		t.Fatalf("guard set after a failure: %q", v)
	}

	got := map[string]model.ErrorInfo{}
	record := func(ctx context.Context, tx db.Store, courseID string, info model.ErrorInfo) error {
		got[courseID] = info
		return tx.SetKV(ctx, "test_notice:"+courseID, info.Code) // written in the same transaction
	}
	n, err := CheckPastRuleRenumbering(f.ctx, f.st, record)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(got) != 1 || got[exported.ID].Code != CodeRenumberedUpgrade || got[exported.ID].Params["n"] != 2 {
		t.Fatalf("recorded %d: %+v", n, got)
	}
	if v := f.kvValue("test_notice:" + exported.ID); v != CodeRenumberedUpgrade {
		t.Errorf("record's write %q", v)
	}
	if v := f.kvValue(KVPastRuleChecked); v != "1" {
		t.Errorf("guard %q", v)
	}
	// the second start does nothing
	got = map[string]model.ErrorInfo{}
	if n, err := CheckPastRuleRenumbering(f.ctx, f.st, record); err != nil || n != 0 || len(got) != 0 {
		t.Errorf("second run: %d %v %+v", n, err, got)
	}
}
