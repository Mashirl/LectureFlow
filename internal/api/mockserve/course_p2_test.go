// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// course_p2.go: calendar numbers, file-name rules, titles, tariffs, holidays.

func fastP2(t *testing.T) {
	t.Helper()
	oldFn, oldTitle := fnTiming, titleTiming
	fnTiming.queued, fnTiming.run, titleTiming = 20*time.Millisecond, 60*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { fnTiming, titleTiming = oldFn, oldTitle })
}

func lecturesOf(t *testing.T, base, course string) map[string]api.Lecture {
	t.Helper()
	var ls []api.Lecture
	doJSON(t, "GET", base+"/api/courses/"+courseID(course)+"/lectures", nil, 200, &ls)
	out := map[string]api.Lecture{}
	for _, l := range ls {
		out[l.ID] = l
	}
	return out
}

func lectureOf(t *testing.T, base, id string) api.Lecture {
	t.Helper()
	var l api.Lecture
	doJSON(t, "GET", base+"/api/lectures/"+id, nil, 200, &l)
	return l
}

func TestP13FixtureNumbers(t *testing.T) {
	srv := testServer(t)
	var ls []api.Lecture
	doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(c3631)+"/lectures", nil, 200, &ls)
	var labels []string
	for _, l := range ls {
		labels = append(labels, l.Label)
	}
	// lecture kinds (kinds.go): per-kind numbers; last year's recording (P1)
	// comes first by date, the additional video (M1) is numbered in the course
	if got := strings.Join(labels, " "); got != "P1 W01L1 W01L2 W02L1 W02L2 W02T1 W03L1 M1 W03S1 W04L1 W04L2" {
		t.Errorf("CSC1003 labels in list order: %s", got)
	}
	os := lecturesOf(t, srv.URL, c2120)
	if l := os[lecID(c2120, 6)]; l.Label != "W??" || l.CalendarHint != "skipped_week" || l.Numbered {
		t.Errorf("reading-week lecture: %s %q", l.Label, l.CalendarHint)
	}
	if l := os[lecID(c2120, 7)]; l.Label != "W03L1" || !l.RecordedHasTime {
		t.Errorf("the week after the reading week: %s", l.Label)
	}
	if l := os[lecID(c2120, 8)]; l.Label != "W??" || l.CalendarHint != "recorded_at" || l.RecordedAtSource != model.RecordedAtNone {
		t.Errorf("unknown file name: %s %q %s", l.Label, l.CalendarHint, l.RecordedAtSource)
	}
	for _, l := range lecturesOf(t, srv.URL, c3311) {
		if l.Label != "W??" || l.CalendarHint != "term_start" {
			t.Errorf("no term start: %s %q", l.Label, l.CalendarHint)
		}
	}
}

func TestCourseCalendarRenumber(t *testing.T) {
	srv := testServer(t)
	cu := srv.URL + "/api/courses/" + courseID(c3631)
	ts := "2026-10-07"
	var conflict api.RenumberConflict
	doJSON(t, "PATCH", cu, api.CoursePatch{TermStart: &ts}, http.StatusConflict, &conflict)
	// (the material kinds P1 and M1 are numbered in the course: unchanged)
	if conflict.Error.Code != "calendar.renumber" || len(conflict.Changes) != 9 || conflict.Changes[0].From != "W01L1" || conflict.Changes[0].To != "W??" || conflict.Changes[2].To != "W01L1" {
		t.Fatalf("conflict %+v", conflict)
	}
	var c api.Course
	doJSON(t, "GET", cu, nil, 200, &c)
	if c.TermStart == nil || *c.TermStart != "2026-09-28" {
		t.Fatalf("saved without confirm: %v", c.TermStart)
	}
	doJSON(t, "PATCH", cu, api.CoursePatch{TermStart: &ts, Confirm: true}, 200, &c)
	if c.TermStart == nil || *c.TermStart != "2026-10-05" {
		t.Errorf("term start not aligned to Monday: %v", c.TermStart)
	}
	if l := lectureOf(t, srv.URL, lecID(c3631, 1)); l.Label != "W??" || l.CalendarHint != "before_term" {
		t.Errorf("before the term: %s %q", l.Label, l.CalendarHint)
	}
	bad := "5 Oct"
	var e api.ErrorBody
	doJSON(t, "PATCH", cu, api.CoursePatch{TermStart: &bad}, 400, &e)
	if e.Error.Code != "course.term_start_invalid" || e.Error.Params["value"] != bad {
		t.Errorf("invalid date: %+v", e)
	}
	// No citation export: saved at once; the reading week counts again.
	none := []string{}
	doJSON(t, "PATCH", srv.URL+"/api/courses/"+courseID(c2120), api.CoursePatch{SkipWeeks: &none}, 200, &c)
	if l := lectureOf(t, srv.URL, lecID(c2120, 6)); l.Label != "W03L1" {
		t.Errorf("reading week counted: %s", l.Label)
	}
}

func TestLectureRecordedAt(t *testing.T) {
	srv := testServer(t)
	lu := srv.URL + "/api/lectures/" + lecID(c3631, 1)
	at := "2026-10-02T09:00"
	var conflict api.RenumberConflict
	doJSON(t, "PATCH", lu, api.LecturePatch{RecordedAt: &at}, http.StatusConflict, &conflict)
	if len(conflict.Changes) != 2 || conflict.Changes[0].From != "W01L2" || conflict.Changes[0].To != "W01L1" {
		t.Fatalf("conflict %+v", conflict)
	}
	var l api.Lecture
	doJSON(t, "PATCH", lu, api.LecturePatch{RecordedAt: &at, Confirm: true}, 200, &l)
	if l.Label != "W01L2" || l.RecordedAtSource != model.RecordedAtUser || l.RecordedAt == nil || *l.RecordedAt != at {
		t.Errorf("after confirm: %s %s %v", l.Label, l.RecordedAtSource, l.RecordedAt)
	}
	if o := lectureOf(t, srv.URL, lecID(c3631, 2)); o.Label != "W01L1" {
		t.Errorf("the other lecture moved up: %s", o.Label)
	}
	// "" = back to automatic: the course's file-name rule
	empty := ""
	doJSON(t, "PATCH", lu, api.LecturePatch{RecordedAt: &empty, Confirm: true}, 200, &l)
	if l.RecordedAtSource != model.RecordedAtFilename || l.RecordedAt == nil || *l.RecordedAt != "2026-09-29T12:35" || l.Label != "W01L1" {
		t.Errorf("automatic again: %s %v %s", l.RecordedAtSource, l.RecordedAt, l.Label)
	}
	zh := "密码学导论"
	doJSON(t, "PATCH", lu, api.LecturePatch{TitleZh: &zh}, 200, &l)
	if l.TitleZh == nil || *l.TitleZh != zh || l.TitleSource != model.TitleSourceUser {
		t.Errorf("title_zh: %v %s", l.TitleZh, l.TitleSource)
	}
}

func TestFilenameRuleProposal(t *testing.T) {
	fastP2(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3121) + "/filename-rule"
	var v api.FilenameRuleView
	// CSC1002 has a rule (with its kind_map); recognizing the format again
	// gives a proposal: the lectures already have recording times, so it is
	// not applied by itself (P13.3.3 first-time exception)
	doJSON(t, "GET", base, nil, 200, &v)
	if v.Rule == nil || v.Rule.Source != "llm" || v.KindMap["T"] != "tutorial" || v.Proposal != nil {
		t.Fatalf("saved rule %+v kind_map %v", v.Rule, v.KindMap)
	}
	doJSON(t, "POST", base+"/generate", nil, http.StatusAccepted, &v)
	eventually(t, "the proposal", func() bool {
		doJSON(t, "GET", base, nil, 200, &v)
		return v.Proposal != nil
	})
	if v.Proposal.Matched != 6 || v.Proposal.Total != 7 || !v.Proposal.OK || v.Proposal.Explanation == "" || v.Proposal.KindMap["L"] != "lecture" {
		t.Fatalf("proposal %+v", v.Proposal)
	}
	rows := map[string]api.FilenamePreviewRow{}
	for _, r := range v.Proposal.Preview {
		rows[r.Filename] = r
	}
	if r := rows["CSC1002-T01-02 Fri Oct 16 12-35-00 BST 2026.mp4"]; r.Status != "ok" || r.KindCode != "T" || r.Kind != "tutorial" || !r.HasTime {
		t.Errorf("tutorial row %+v", r)
	}
	if r := rows["RISC-V guest talk.mp4"]; r.Status != "unmatched" || r.Kind != "" {
		t.Errorf("guest talk row %+v", r)
	}
	// live preview of an edited rule; a broken one is shown, not rejected
	var p api.FilenamePreviewResult
	doJSON(t, "POST", base+"/preview", api.FilenamePreviewInput{Regex: `^CSC1002-L\d{2}-\d{2} [A-Z][a-z]{2} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2})`}, 200, &p)
	if p.Matched != 3 || p.Total != 7 {
		t.Errorf("edited preview %d/%d", p.Matched, p.Total)
	}
	// a code without an entry in kind_map: shown, mapped to nothing
	doJSON(t, "POST", base+"/preview", api.FilenamePreviewInput{Regex: v.Proposal.Rule.Regex, KindMap: map[string]string{"L": "lecture"}}, 200, &p)
	for _, r := range p.Preview {
		if r.KindCode == "T" && r.Kind != "" {
			t.Errorf("unmapped code %+v", r)
		}
	}
	doJSON(t, "POST", base+"/preview", api.FilenamePreviewInput{Regex: `(?P<day>\d{2})(`}, 200, &p)
	if p.OK || p.Matched != 0 || len(p.Preview) != 7 || p.Preview[0].Error == "" {
		t.Errorf("broken regex %+v", p)
	}
	var e api.ErrorBody
	doJSON(t, "PUT", base, api.FilenamePreviewInput{Regex: `(?P<day>\d{2})(`}, 400, &e)
	if e.Error.Code != "filename_rule.invalid" {
		t.Errorf("apply broken: %+v", e)
	}
	// 应用: the proposal's rule and kind_map (source llm)
	var c api.Course
	doJSON(t, "PUT", base, api.FilenamePreviewInput{Regex: v.Proposal.Rule.Regex, KindMap: v.Proposal.KindMap}, 200, &c)
	if c.FilenameRule == nil || c.FilenameRule.Source != "llm" || c.FilenameProposal || c.FilenameRule.SamplesMatched != 6 {
		t.Fatalf("applied %+v", c.FilenameRule)
	}
	ls := lecturesOf(t, srv.URL, c3121)
	if l := ls[lecID(c3121, 2)]; l.Label != "W02L1" || l.RecordedAtSource != model.RecordedAtFilename || !l.RecordedHasTime || l.Kind != "lecture" || l.KindSource != api.KindSourceFilename {
		t.Errorf("lecture 2: %s %s %s/%s", l.Label, l.RecordedAtSource, l.Kind, l.KindSource)
	}
	doJSON(t, "DELETE", base+"/proposal", nil, 404, &e)
	if e.Error.Code != "filename_rule.no_proposal" {
		t.Errorf("no proposal: %+v", e)
	}
	// a hand-written regex is the user's; without a time of day the order
	// of a day's recordings is guessed from the file names (P13.3.4), and
	// the edited kind_map changes the kinds
	user := `^CSC1002-(?P<kind>[A-Z])\d{2}-\d{2} [A-Z][a-z]{2} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2})`
	doJSON(t, "PUT", base, api.FilenamePreviewInput{Regex: user, KindMap: map[string]string{"L": "lecture", "T": "seminar"}}, 200, &c)
	if c.FilenameRule.Source != "user" {
		t.Errorf("edited rule source %q", c.FilenameRule.Source)
	}
	ls = lecturesOf(t, srv.URL, c3121)
	if l := ls[lecID(c3121, 5)]; !l.SameDayByName || l.Kind != "seminar" || l.Label != "W03S2" {
		t.Errorf("same day by name: %v %s %s", l.SameDayByName, l.Kind, l.Label)
	}
	doJSON(t, "GET", base, nil, 200, &v)
	if v.KindMap["T"] != "seminar" || v.Proposal != nil {
		t.Errorf("saved kind_map %v", v.KindMap)
	}
}

func TestFilenameRuleGenerateAndUndo(t *testing.T) {
	fastP2(t)
	srv := testServer(t)
	cs := srv.URL + "/api/courses/" + courseID(c3631)
	var v api.FilenameRuleView
	doJSON(t, "POST", cs+"/filename-rule/generate", nil, http.StatusAccepted, &v)
	if v.Task == nil || v.Task.Stage != model.StageFilenameRule || v.Task.Status != model.StatusQueued {
		t.Fatalf("task %+v", v.Task)
	}
	eventually(t, "the proposal", func() bool {
		var c api.Course
		doJSON(t, "GET", cs, nil, 200, &c)
		return c.FilenameProposal && c.FilenameTask != nil && c.FilenameTask.Status == model.StatusDone
	})
	var e api.ErrorBody
	doJSON(t, "DELETE", cs+"/filename-rule/proposal", nil, 200, &api.Course{})
	// the organizer role is not configured in ?mock=unset
	doJSON(t, "POST", cs+"/filename-rule/generate?mock=unset", nil, http.StatusConflict, &e)
	if e.Error.Code != "filename_rule.not_configured" {
		t.Errorf("unset organizer: %+v", e)
	}
	// COMP2120: the first automatic apply can be undone once
	cu := srv.URL + "/api/courses/" + courseID(c2120)
	var c api.Course
	doJSON(t, "POST", cu+"/filename-rule/undo", nil, 200, &c)
	if c.FilenameRule != nil || c.FilenameUndo {
		t.Errorf("after undo %+v %v", c.FilenameRule, c.FilenameUndo)
	}
	if l := lectureOf(t, srv.URL, lecID(c2120, 1)); l.RecordedAt != nil || l.Label != "W??" {
		t.Errorf("undo kept the time: %v %s", l.RecordedAt, l.Label)
	}
	doJSON(t, "POST", cu+"/filename-rule/undo", nil, 404, &e)
	if e.Error.Code != "filename_rule.no_undo" {
		t.Errorf("second undo %+v", e)
	}
}

func TestRegenerateTitle(t *testing.T) {
	fastP2(t)
	srv := testServer(t)
	var e api.ErrorBody
	// a failed title task is retried by the user (lead ruling 1)
	l4 := lecID(c3311, 4)
	if l := lectureOf(t, srv.URL, l4); len(l.Aux) != 1 || l.Aux[0].Status != model.StatusFailed || l.TitleSource != model.TitleSourceFilename {
		t.Fatalf("fixture %+v", l.Aux)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/"+l4+"/stages/title/retry", nil, 200, &api.Lecture{})
	eventually(t, "the title", func() bool {
		l := lectureOf(t, srv.URL, l4)
		return l.TitleSource == model.TitleSourceLLM && l.BlurbEn != nil && l.Aux[0].Status == model.StatusDone
	})
	// a title the user set needs force
	lu := srv.URL + "/api/lectures/" + lecID(c3631, 1)
	title := "My own title"
	doJSON(t, "PATCH", lu, api.LecturePatch{Title: &title}, 200, &api.Lecture{})
	doJSON(t, "POST", lu+"/title", nil, http.StatusConflict, &e)
	if e.Error.Code != "title.user_set" {
		t.Errorf("user title: %+v", e)
	}
	doJSON(t, "POST", lu+"/title", api.TitleRegenerateInput{Force: true}, http.StatusAccepted, &api.Lecture{})
	eventually(t, "the forced title", func() bool { return lectureOf(t, srv.URL, lecID(c3631, 1)).TitleSource == model.TitleSourceLLM })
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecID(c3631, 7)+"/title", nil, http.StatusConflict, &e)
	if e.Error.Code != "title.summary_not_ready" {
		t.Errorf("no summary: %+v", e)
	}
}

func TestTariffAndHolidays(t *testing.T) {
	srv := testServer(t)
	for _, sc := range []string{"peak", "offpeak"} {
		var r api.Resources
		doJSON(t, "GET", srv.URL+"/api/resources?mock="+sc, nil, 200, &r)
		ds, an := r.Providers[0], r.Providers[1]
		if ds.TariffState == nil || ds.TariffState.Peak != (sc == "peak") || ds.TariffState.Timezone != "Asia/Shanghai" || ds.TariffState.Multiplier != 0.5 || an.TariffState != nil {
			t.Fatalf("%s: %+v %+v", sc, ds.TariffState, an.TariffState)
		}
		at, _ := time.Parse(time.RFC3339, r.SampledAt)
		next, err := time.Parse(time.RFC3339, ds.TariffState.NextChange)
		if err != nil || !next.After(at) {
			t.Errorf("%s: next change %q after %q", sc, ds.TariffState.NextChange, r.SampledAt)
		}
	}
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, 200, &s)
	if ds := findProvider(s.Providers, pidDeepSeek); ds == nil || !strings.Contains(string(ds.Tariff), "Asia/Shanghai") || ds.TariffState == nil {
		t.Fatalf("settings tariff %+v", ds)
	}
	// a new DeepSeek provider gets the preset; null clears; invalid → coded 400
	name, typ, url := "DS2", model.ProviderOpenAI, "https://api.deepseek.com/v1"
	var p api.ProviderView
	doJSON(t, "POST", srv.URL+"/api/providers", api.ProviderInput{Name: &name, Type: &typ, BaseURL: &url}, http.StatusCreated, &p)
	if !strings.Contains(string(p.Tariff), "deepseek-flash") || p.TariffState == nil {
		t.Fatalf("preset %s", p.Tariff)
	}
	on := true
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+p.ID, api.ProviderInput{OffpeakOnly: &on}, 200, &p)
	if !p.OffpeakOnly || !p.TariffState.OffpeakOnly {
		t.Errorf("offpeak_only %+v", p.TariffState)
	}
	var e api.ErrorBody
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+p.ID, api.ProviderInput{Tariff: json.RawMessage(`{"timezone":"Mars/Base","peak_windows":[],"offpeak_multiplier":0.5}`)}, 400, &e)
	if e.Error.Code != "tariff.invalid_timezone" {
		t.Errorf("invalid tariff %+v", e)
	}
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pidAnthropic, api.ProviderInput{OffpeakOnly: &on}, 400, &e)
	if e.Error.Code != "settings.offpeak_needs_peak" {
		t.Errorf("off-peak without a tariff %+v", e)
	}
	var cleared api.ProviderView // (decoding into p would keep its old tariff_state)
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+p.ID, api.ProviderInput{Tariff: json.RawMessage(`null`)}, 200, &cleared)
	// (the off-peak switch goes with the tariff, like settings.applyTariff)
	if len(cleared.Tariff) != 0 && string(cleared.Tariff) != "null" || cleared.TariffState != nil || cleared.OffpeakOnly {
		t.Errorf("cleared %s %+v", cleared.Tariff, cleared.TariffState)
	}

	// holidays: built-in, edited, restored
	var h api.HolidaysView
	doJSON(t, "GET", srv.URL+"/api/settings/holidays", nil, 200, &h)
	if h.Edited || len(h.Years["2026"]) != 33 || h.Source == "" {
		t.Fatalf("built-in %+v", h)
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/holidays", map[string]any{"years": map[string][]string{"2027": {"2027-13-01"}}}, 400, &e)
	if e.Error.Code != "tariff.invalid_holiday_data" {
		t.Errorf("invalid holidays %+v", e)
	}
	var edited, restored api.HolidaysView
	doJSON(t, "PUT", srv.URL+"/api/settings/holidays", map[string]any{"years": map[string][]string{"2027": {"2027-01-01"}}}, 200, &edited)
	if !edited.Edited || len(edited.Years) != 1 || edited.Source != "" {
		t.Errorf("edited %+v", edited)
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/holidays", map[string]any{"years": nil}, 200, &restored)
	if restored.Edited || len(restored.Years["2026"]) != 33 {
		t.Errorf("restored %+v", restored)
	}
	var ui api.UISettings
	doJSON(t, "PUT", srv.URL+"/api/settings/ui", api.UISettings{Locale: "en", ZhDisplay: "tw"}, 200, &ui)
	doJSON(t, "GET", srv.URL+"/api/settings/ui", nil, 200, &ui)
	if ui.Locale != "en" || ui.ZhDisplay != "tw" {
		t.Errorf("ui %+v", ui)
	}
}
