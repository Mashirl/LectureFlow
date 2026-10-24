// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// SPEC-v2 phase 2 in the mock server (web-course-p2): lecture numbers from
// the course calendar (P13.4, computed on every change like the real
// internal/api/v2_calendar.go), the term start and skipped weeks of PATCH
// /api/courses/{id}, recording times and titles of PATCH /api/lectures/{id},
// the 409 RenumberConflict of P13.6, the file-name rule (P13.3: view,
// simulated generation, live preview, apply, ignore, undo), "重新生成标题与
// 简介" (P13.5), the peak / off-peak state of providers (P12.4), the public
// holidays (P12.2) and the UI settings (P14.6, P14.7).
//
// Scenarios "peak" and "offpeak" are the default data with the tariff clock
// moved ahead into that period for DeepSeek (screenshots of both tags; the
// resources' sampled_at tells the browser clock where to be).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/tariff"
)

// ---------- per-store state the fixtures do not hold ----------

type p2World struct {
	proposals map[string]*api.FilenameProposal // course id → a generated rule waiting for the user
	undo      map[string]map[string]recSnap    // course id → lecture id → before the automatic first apply
	gen       map[string]int                   // course id → bumps when a filename_rule run is replaced
	titleGen  map[string]int                   // lecture id → bumps when a title run is replaced
	holidays  map[string][]string              // the user's holidays (nil = the built-in file)
	ui        api.UISettings
}

// recSnap is a lecture's recording time and title before a rule changed them.
type recSnap struct {
	at       *string
	src      string
	title    string
	titleSrc string
}

var p2Worlds sync.Map // *store → *p2World

// p2 returns the store's phase-2 state; must be called with s.mu held. On
// first use CSC1002 gets the pending proposal of its fixture.
func (s *store) p2() *p2World {
	if v, ok := p2Worlds.Load(s); ok {
		return v.(*p2World)
	}
	w := &p2World{proposals: map[string]*api.FilenameProposal{}, undo: map[string]map[string]recSnap{}, gen: map[string]int{}, titleGen: map[string]int{}}
	for i := range s.courses {
		c := &s.courses[i]
		if c.FilenameProposal {
			if p := s.proposalFor(c, time.Now()); p != nil {
				w.proposals[c.ID] = p
			} else {
				c.FilenameProposal = false
			}
		}
	}
	p2Worlds.Store(s, w)
	return w
}

// codedErr is a v2 error body {"error": {"code", "params", "detail"}} (P14.3).
func codedErr(status int, code string, params map[string]any, detail string) error {
	return &httpError{status: status, msg: code, body: api.ErrorBody{Error: model.ErrorInfo{Code: code, Params: params, Detail: detail}}}
}

// ---------- numbering (P13.4) ----------

func recordedOf(l *api.Lecture) *time.Time {
	if l.RecordedAt == nil || strings.TrimSpace(*l.RecordedAt) == "" {
		return nil
	}
	t, err := calendar.ParseRecordedAt(*l.RecordedAt, time.Local)
	if err != nil {
		return nil
	}
	return &t
}

func termOf(c *api.Course) (*time.Time, []time.Time) {
	var ts *time.Time
	if c.TermStart != nil && *c.TermStart != "" {
		if t, err := calendar.ParseDate(*c.TermStart, time.Local); err == nil {
			ts = &t
		}
	}
	skips := []time.Time{}
	for _, w := range c.SkipWeeks {
		if t, err := calendar.ParseDate(w, time.Local); err == nil {
			skips = append(skips, t)
		}
	}
	return ts, skips
}

// numberCourse computes week, n, label, seq, the W?? hint and the lecture
// kinds of c's lectures in place, with the rules of the real views
// (lectures in created_at order, as ListLectures; seq = position in
// calendar.Order + 1; numbers by calendar.LabelKinds as state.NumberCourse).
// Lecture.kind / kind_source hold the stored kind between passes (the view
// is written back: the effective kind, and kind_source "date" when the
// past-lecture rule decided, which resolveKind reads as "not decided").
func numberCourse(c *api.Course, lectures []api.Lecture, kc *kindCtx) {
	if c.SkipWeeks == nil {
		c.SkipWeeks = []string{}
	}
	if c.Numbering == "" {
		c.Numbering = api.NumberingAuto
	}
	var idx []int
	for i := range lectures {
		if lectures[i].CourseID == c.ID {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return lectures[idx[a]].CreatedAt < lectures[idx[b]].CreatedAt })
	ts, skips := termOf(c)
	in := make([]calendar.LectureTime, len(idx))
	recs := make([]*time.Time, len(idx))
	for k, i := range idx {
		l := &lectures[i]
		if l.RecordedAtSource == "" {
			l.RecordedAtSource = model.RecordedAtNone
		}
		if l.TitleSource == "" {
			l.TitleSource = model.TitleSourceFilename
		}
		resolveKind(c, l, kc)
		recs[k] = recordedOf(l)
		in[k] = calendar.LectureTime{ID: l.ID, RecordedAt: recs[k], OriginalFilename: l.OriginalFilename, Kind: l.Kind, KindSource: l.KindSource}
	}
	nums := calendar.LabelKinds(in, ts, skips, calendar.NumberOptions{Kinds: kc.calendarKinds(), Numbering: c.Numbering})
	for pos, k := range calendar.Order(in) {
		l := &lectures[idx[k]]
		n := nums[k]
		l.Seq = pos + 1
		l.Week, l.N, l.Numbered, l.Label, l.SameDayByName, l.CiteLabel = n.Week, n.N, n.OK, n.Label, n.SameDayByName, n.CiteLabel
		l.Kind, l.KindSource = n.Kind, api.ViewKindSource(l.Kind, l.KindSource, n.Kind)
		l.KindN, l.KindCount, l.ShowN = n.KindN, n.KindCount, n.ShowN
		l.RecordedHasTime = recs[k] != nil && calendar.HasClock(*recs[k])
		l.CalendarHint = ""
		if !n.OK {
			switch {
			case ts == nil:
				l.CalendarHint = "term_start"
			case recs[k] == nil:
				l.CalendarHint = "recorded_at"
			case calendar.AlignMonday(*recs[k]).Before(calendar.AlignMonday(*ts)):
				l.CalendarHint = "before_term"
			default:
				l.CalendarHint = "skipped_week"
			}
		}
	}
	materialsCourse(c, lectures, idx) // kinds.go: the slides of each lecture
}

// renumberCheck is P13.6: when c's citations were exported and mutate (run
// on copies) changes any lecture number, the change needs confirm → 409
// RenumberConflict. Must hold s.mu; mutate must assign new pointers.
func (s *store) renumberCheck(c *api.Course, confirm bool, mutate func(c *api.Course, lecs []api.Lecture)) error {
	if !c.CiteExported || confirm {
		return nil
	}
	cc := *c
	cc.SkipWeeks = append([]string(nil), c.SkipWeeks...)
	lecs := append([]api.Lecture(nil), s.lectures...)
	mutate(&cc, lecs)
	numberCourse(&cc, lecs, s.kctx())
	type change struct {
		api.RenumberChange
		seq int
	}
	var list []change
	for i := range lecs {
		before, after := s.lectures[i], lecs[i]
		if after.CourseID == c.ID && before.Label != after.Label {
			list = append(list, change{api.RenumberChange{LectureID: after.ID, Title: after.Title, From: before.Label, To: after.Label}, after.Seq})
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].seq < list[b].seq })
	changes := make([]api.RenumberChange, len(list))
	for i, x := range list {
		changes[i] = x.RenumberChange
	}
	return &httpError{status: http.StatusConflict, msg: "calendar.renumber", body: api.RenumberConflict{
		Error:   model.ErrorInfo{Code: "calendar.renumber", Params: map[string]any{"count": len(changes)}},
		Changes: changes,
	}}
}

// ---------- PATCH /api/courses/{id}: term start and skipped weeks ----------

// patchCalendar validates the P13.4 fields of p and applies them (after the
// renumbering check). Must hold s.mu. (Called by patchCourseChecked.)
func (s *store) patchCalendar(c *api.Course, p api.CoursePatch) error {
	if p.TermStart == nil && p.SkipWeeks == nil {
		return nil
	}
	termStart := c.TermStart
	if p.TermStart != nil {
		switch v := strings.TrimSpace(*p.TermStart); v {
		case "":
			termStart = nil
		default:
			t, err := calendar.ParseDate(v, time.Local)
			if err != nil {
				return codedErr(http.StatusBadRequest, "course.term_start_invalid", map[string]any{"value": v}, err.Error())
			}
			m := calendar.FormatDate(calendar.AlignMonday(t))
			termStart = &m
		}
	}
	skips := c.SkipWeeks
	if p.SkipWeeks != nil {
		var ws []time.Time
		for _, v := range *p.SkipWeeks {
			t, err := calendar.ParseDate(v, time.Local)
			if err != nil {
				return codedErr(http.StatusBadRequest, "course.skip_week_invalid", map[string]any{"value": v}, err.Error())
			}
			ws = append(ws, t)
		}
		skips = []string{}
		for _, t := range calendar.NormalizeSkipWeeks(ws) {
			skips = append(skips, calendar.FormatDate(t))
		}
	}
	if err := s.renumberCheck(c, p.Confirm, func(cc *api.Course, _ []api.Lecture) {
		cc.TermStart, cc.SkipWeeks = termStart, skips
	}); err != nil {
		return err
	}
	c.TermStart, c.SkipWeeks = termStart, skips
	return nil
}

// ---------- PATCH /api/lectures/{id}: titles and recording time ----------

// patchLectureP2 applies the P13 fields of a LecturePatch (seq is read-only
// from v2). Must hold s.mu; the caller runs s.changed afterwards.
func (s *store) patchLectureP2(l *api.Lecture, p api.LecturePatch) error {
	if p.Seq != nil {
		return codedErr(http.StatusBadRequest, "lecture.seq_readonly", nil, "")
	}
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" {
			return codedErr(http.StatusBadRequest, "lecture.title_empty", nil, "")
		}
		l.Title, l.TitleSource = t, model.TitleSourceUser
	}
	if p.TitleZh != nil {
		t := strings.TrimSpace(*p.TitleZh)
		if t == "" {
			l.TitleZh = nil
		} else {
			l.TitleZh = &t
		}
		l.TitleSource = model.TitleSourceUser
	}
	if p.RecordedAt == nil {
		return nil
	}
	c, err := s.findCourse(l.CourseID)
	if err != nil {
		return err
	}
	at, src := (*string)(nil), model.RecordedAtUser
	if v := strings.TrimSpace(*p.RecordedAt); v != "" {
		t, err := calendar.ParseRecordedAt(v, time.Local)
		if err != nil {
			return codedErr(http.StatusBadRequest, "lecture.recorded_at_invalid", map[string]any{"value": v}, err.Error())
		}
		f := calendar.FormatRecordedAt(t)
		at = &f
	} else {
		// "" = back to automatic: the course's rule, else nothing
		at, src = s.automaticRecordedAt(c, l)
	}
	id := l.ID
	if err := s.renumberCheck(c, p.Confirm, func(_ *api.Course, lecs []api.Lecture) {
		for i := range lecs {
			if lecs[i].ID == id {
				lecs[i].RecordedAt, lecs[i].RecordedAtSource = at, src
			}
		}
	}); err != nil {
		return err
	}
	l.RecordedAt, l.RecordedAtSource = at, src
	return nil
}

// automaticRecordedAt is P13.3.4 without the user's value: the course rule.
func (s *store) automaticRecordedAt(c *api.Course, l *api.Lecture) (*string, string) {
	if rule := ruleOf(c); rule != nil {
		if p := calendar.Parse(*rule, sampleOf(l)); p.RecordedAt != nil {
			f := calendar.FormatRecordedAt(*p.RecordedAt)
			return &f, model.RecordedAtFilename
		}
	}
	return nil, model.RecordedAtNone
}

// ---------- file-name rules (P13.3) ----------

func ruleOf(c *api.Course) *calendar.FilenameRule {
	if c.FilenameRule == nil || strings.TrimSpace(c.FilenameRule.Regex) == "" {
		return nil
	}
	r := *c.FilenameRule
	return &r
}

func sampleOf(l *api.Lecture) calendar.Sample {
	up, err := time.Parse(time.RFC3339, l.CreatedAt)
	if err != nil {
		up = time.Now()
	}
	return calendar.Sample{Filename: l.OriginalFilename, UploadedAt: up.In(time.Local)}
}

func (s *store) samplesOf(courseID string) []calendar.Sample {
	var out []calendar.Sample
	for i := range s.lectures {
		if l := &s.lectures[i]; l.CourseID == courseID && l.OriginalFilename != "" {
			out = append(out, sampleOf(l))
		}
	}
	return out
}

// validateRule previews regex over the course's file names; kindMap fills
// each row's kind_code and kind (calendar.ValidateRule, as the real API).
func (s *store) validateRule(c *api.Course, regex string, kindMap map[string]string) api.FilenamePreviewResult {
	ts, _ := termOf(c)
	return api.ValidateRule(calendar.FilenameRule{Regex: regex, KindMap: kindMap}, s.samplesOf(c.ID), ts)
}

// cleanKindMap drops empty codes and kinds that are not in the catalog.
func (s *store) cleanKindMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	kc := s.kctx()
	for code, kind := range in {
		if code = strings.TrimSpace(code); code != "" && kc.kind(kind) != nil {
			out[code] = kind
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mockRule is what the simulated organizer model writes for a course: the
// regex, its one-sentence explanation and (lecture kinds, kinds.go) the
// kind_map of its "kind" group.
type mockRule struct {
	regex, explanation string
	kindMap            map[string]string
}

// panoptoRule is the rule for Panopto names such as
// "CSC1002-T01-02 Fri Oct 16 12-35-00 BST 2026.mp4": the session type
// letter and its timetable group, the recording number, then the start.
func panoptoRule(code string) mockRule {
	return mockRule{
		regex:       `^` + code + `-(?P<kind>[A-Z])\d{2}-\d{2} [A-Z][a-z]{2} (?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3,4} (?P<year>\d{4})`,
		explanation: "Panopto names: the course code, the session type letter with its group (L01, T01), the recording number, then the weekday, month, day, start time and year.",
		kindMap:     map[string]string{"L": "lecture", "T": "tutorial", "S": "seminar"},
	}
}

// mockRules is the simulated organizer model: one rule per example course
// (written for its file names), and a generic date pattern for new courses.
var mockRules = map[string]mockRule{
	"CSC1003": panoptoRule("CSC1003"),
	"CSC1002": panoptoRule("CSC1002"),
	"COMP2120": {
		regex:       `_(?P<kind>Lecture|Tutorial|Seminar)_(?P<day>\d{2})(?P<month>[A-Za-z]{3})(?P<year>\d{4})_(?P<hour>\d{2})(?P<minute>\d{2})`,
		explanation: "The names carry the session type as a word, then the day, the English month abbreviation and the year, followed by the start time as HHMM.",
		kindMap:     map[string]string{"Lecture": "lecture", "Tutorial": "tutorial", "Seminar": "seminar"},
	},
}

const genericRule = `(?P<year>20\d{2})[-_.]?(?P<month>\d{2})[-_.]?(?P<day>\d{2})`

// proposalFor runs the simulated model and validation for c (nil when the
// rule does not match at least one file name).
func (s *store) proposalFor(c *api.Course, now time.Time) *api.FilenameProposal {
	code := ""
	if c.Code != nil {
		code = *c.Code
	}
	g, ok := mockRules[code]
	if !ok {
		g.regex, g.explanation = genericRule, "The only consistent date in the names is a YYYY-MM-DD (or YYYYMMDD) block; there is no time of day."
	}
	res := s.validateRule(c, g.regex, g.kindMap)
	if res.Matched == 0 {
		return nil
	}
	created := now.UTC().Format(time.RFC3339)
	return &api.FilenameProposal{
		Rule:        calendar.FilenameRule{Regex: g.regex, Source: calendar.RuleSourceLLM, CreatedAt: created, SamplesMatched: res.Matched, SamplesTotal: res.Total},
		Explanation: g.explanation, CreatedAt: created,
		Preview: res.Preview, Matched: res.Matched, Total: res.Total, OK: res.OK, Problems: res.Problems,
		ProblemsInfo: res.ProblemsInfo, KindMap: g.kindMap,
	}
}

func (s *store) filenameView(c *api.Course) api.FilenameRuleView {
	v := api.FilenameRuleView{Rule: ruleOf(c), Preview: []api.FilenamePreviewRow{}, Proposal: s.p2().proposals[c.ID], Task: c.FilenameTask}
	if v.Rule != nil {
		v.KindMap = s.kctx().kindMaps[c.ID]
		v.Preview = s.validateRule(c, v.Rule.Regex, v.KindMap).Preview
	}
	return v
}

// applyRule re-resolves every lecture whose recording time is not the
// user's (P13.3.3 应用); a parsed title replaces a file-name title. Returns
// what it changed, for the undo of the first automatic apply.
func (s *store) applyRule(c *api.Course, rule calendar.FilenameRule, kindMap map[string]string) map[string]recSnap {
	if kindMap != nil {
		s.kctx().kindMaps[c.ID] = kindMap
	} else {
		delete(s.kctx().kindMaps, c.ID)
	}
	before := map[string]recSnap{}
	for i := range s.lectures {
		l := &s.lectures[i]
		if l.CourseID != c.ID || l.RecordedAtSource == model.RecordedAtUser {
			continue
		}
		before[l.ID] = recSnap{at: l.RecordedAt, src: l.RecordedAtSource, title: l.Title, titleSrc: l.TitleSource}
		p := calendar.Parse(rule, sampleOf(l))
		if p.RecordedAt != nil {
			f := calendar.FormatRecordedAt(*p.RecordedAt)
			l.RecordedAt, l.RecordedAtSource = &f, model.RecordedAtFilename
		} else if l.RecordedAtSource == model.RecordedAtFilename {
			l.RecordedAt, l.RecordedAtSource = nil, model.RecordedAtNone
		}
		if p.Title != "" && l.TitleSource == model.TitleSourceFilename {
			l.Title = p.Title
		}
	}
	r := rule
	c.FilenameRule = &r
	return before
}

func (s *store) fnChanged(c *api.Course) {
	finalizeCourse(c, s.lectures, s.kctx())
	for i := range s.lectures {
		if s.lectures[i].CourseID == c.ID {
			s.notify(api.ChangedEvent{Kind: "lecture", ID: s.lectures[i].ID})
		}
	}
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
}

// organizerConfigured: the organizer role has a provider that exists and a model.
func (s *store) organizerConfigured() bool {
	for _, rv := range s.settings.Roles {
		if rv.Role == model.RoleOrganizer && rv.ProviderID != nil && rv.Model != nil && *rv.Model != "" {
			_, ok := s.providerByID(*rv.ProviderID)
			return ok
		}
	}
	return false
}

// fnTiming is the simulated filename_rule run (shortened by tests).
var fnTiming = struct{ queued, run time.Duration }{1 * time.Second, 2500 * time.Millisecond}

func auxStage(stage, status string, now time.Time) *api.StageView {
	t := now.UTC().Format(time.RFC3339)
	st := &api.StageView{Stage: stage, Status: status, AllowedActions: stageActions(status), DisplayStatus: status}
	switch status {
	case model.StatusQueued:
		w := model.WaitAPISlot
		st.WaitingFor = &w
	case model.StatusRunning:
		st.StartedAt, st.Progress = &t, 0.3
	default:
		st.FinishedAt, st.Progress = &t, 1
	}
	return st
}

// startFilenameRule simulates the course's filename_rule task: queued →
// running → a proposal (or, P13.3.3 首次例外, applied at once with an undo
// notice; or failed when the rule matches under 60%). Must hold s.mu.
func (s *store) startFilenameRule(c *api.Course) {
	w := s.p2()
	w.gen[c.ID]++
	gen, id := w.gen[c.ID], c.ID
	c.FilenameTask = auxStage(model.StageFilenameRule, model.StatusQueued, time.Now())
	s.courseChanged(c)
	step := func(d time.Duration, fn func(c *api.Course, now time.Time)) {
		time.AfterFunc(d, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if w.gen[id] != gen {
				return
			}
			if c, err := s.findCourse(id); err == nil {
				fn(c, time.Now())
			}
		})
	}
	step(fnTiming.queued, func(c *api.Course, now time.Time) {
		c.FilenameTask = auxStage(model.StageFilenameRule, model.StatusRunning, now)
		s.courseChanged(c)
	})
	step(fnTiming.queued+fnTiming.run, func(c *api.Course, now time.Time) {
		p := s.proposalFor(c, now)
		if p == nil || !p.OK {
			st := auxStage(model.StageFilenameRule, model.StatusFailed, now)
			matched, total := 0, len(s.samplesOf(c.ID))
			if p != nil {
				matched, total = p.Matched, p.Total
			}
			info := model.ErrorInfo{Code: "filename_rule.rejected", Params: map[string]any{"matched": matched, "total": total}}
			msg := fmt.Sprintf("识别出的规则只匹配了 %d / %d 个文件名，没有采用", matched, total)
			st.Error, st.ErrorInfo = &msg, &info
			c.FilenameTask = st
			s.courseChanged(c)
			return
		}
		c.FilenameTask = auxStage(model.StageFilenameRule, model.StatusDone, now)
		// 首次例外: nothing has a recording time yet and every sample is fine
		first := true
		for i := range s.lectures {
			if s.lectures[i].CourseID == c.ID && s.lectures[i].RecordedAt != nil {
				first = false
			}
		}
		suspicious := false
		for _, r := range p.Preview {
			suspicious = suspicious || r.Status != calendar.StatusOK
		}
		if first && !suspicious {
			w.undo[c.ID] = s.applyRule(c, p.Rule, p.KindMap)
			delete(w.proposals, c.ID)
			c.FilenameProposal, c.FilenameUndo = false, true
		} else {
			w.proposals[c.ID] = p
			c.FilenameProposal = true
		}
		s.fnChanged(c)
	})
}

// ---------- handlers ----------

func (m *mock) registerCourseP2(mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("GET /api/courses/{id}/filename-rule", a(m.getFilenameRule))
	mux.HandleFunc("POST /api/courses/{id}/filename-rule/generate", a(m.generateFilenameRule))
	mux.HandleFunc("POST /api/courses/{id}/filename-rule/preview", a(m.previewFilenameRule))
	mux.HandleFunc("PUT /api/courses/{id}/filename-rule", a(m.putFilenameRule))
	mux.HandleFunc("DELETE /api/courses/{id}/filename-rule/proposal", a(m.dismissFilenameProposal))
	mux.HandleFunc("POST /api/courses/{id}/filename-rule/undo", a(m.undoFilenameRule))
	mux.HandleFunc("POST /api/lectures/{id}/title", a(m.regenerateTitle))
	mux.HandleFunc("GET /api/settings/holidays", a(m.getHolidays))
	mux.HandleFunc("PUT /api/settings/holidays", a(m.putHolidays))
	mux.HandleFunc("GET /api/settings/ui", a(m.getUI))
	mux.HandleFunc("PUT /api/settings/ui", a(m.putUI))
}

func (s *store) courseOr404(id string) (*api.Course, error) {
	c, err := s.findCourse(id)
	if err != nil {
		return nil, codedErr(http.StatusNotFound, "course.not_found", nil, "")
	}
	return c, nil
}

func (m *mock) getFilenameRule(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.filenameView(c))
}

func (m *mock) generateFilenameRule(w http.ResponseWriter, r *http.Request, s *store) {
	st, err := m.settingsStore(r, s) // ?mock=unset: the role assignment of that scenario
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	configured := st.organizerConfigured()
	st.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	switch {
	case !configured:
		writeErr(w, codedErr(http.StatusConflict, "filename_rule.not_configured", nil, ""))
		return
	case len(s.samplesOf(c.ID)) == 0:
		writeErr(w, codedErr(http.StatusConflict, "filename_rule.no_samples", nil, ""))
		return
	}
	s.startFilenameRule(c)
	writeJSON(w, http.StatusAccepted, s.filenameView(c))
}

func (m *mock) previewFilenameRule(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.FilenamePreviewInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Regex) == "" {
		writeErr(w, codedErr(http.StatusBadRequest, "filename_rule.regex_empty", nil, ""))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.validateRule(c, in.Regex, s.cleanKindMap(in.KindMap)))
}

// putFilenameRule is 应用 / 手动编辑: the proposal's regex keeps the model's
// rule, the current regex its source, anything else is the user's.
func (m *mock) putFilenameRule(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.FilenamePreviewInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Regex) == "" {
		writeErr(w, codedErr(http.StatusBadRequest, "filename_rule.regex_empty", nil, ""))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	wd := s.p2()
	rule := calendar.FilenameRule{Regex: in.Regex, Source: calendar.RuleSourceUser}
	kindMap := s.cleanKindMap(in.KindMap)
	sameKinds := func(m map[string]string) bool { return fmt.Sprint(m) == fmt.Sprint(kindMap) }
	switch cur := ruleOf(c); {
	case wd.proposals[c.ID] != nil && wd.proposals[c.ID].Rule.Regex == in.Regex && sameKinds(wd.proposals[c.ID].KindMap):
		rule = wd.proposals[c.ID].Rule
	case cur != nil && cur.Regex == in.Regex && sameKinds(s.kctx().kindMaps[c.ID]):
		rule = *cur
	}
	if _, err := calendar.Compile(rule); err != nil {
		writeErr(w, codedErr(http.StatusBadRequest, "filename_rule.invalid", nil, err.Error()))
		return
	}
	res := s.validateRule(c, rule.Regex, kindMap)
	rule.SamplesMatched, rule.SamplesTotal = res.Matched, res.Total
	if rule.CreatedAt == "" || rule.Source == calendar.RuleSourceUser {
		rule.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	oldKinds := s.kctx().kindMaps[c.ID]
	s.kctx().kindMaps[c.ID] = kindMap // seen by the renumber check (restored below when it fails)
	if err := s.renumberCheck(c, in.Confirm, func(cc *api.Course, lecs []api.Lecture) {
		cc.FilenameRule = &rule
		for i := range lecs {
			l := &lecs[i]
			if l.CourseID != cc.ID || l.RecordedAtSource == model.RecordedAtUser {
				continue
			}
			if p := calendar.Parse(rule, sampleOf(l)); p.RecordedAt != nil {
				f := calendar.FormatRecordedAt(*p.RecordedAt)
				l.RecordedAt, l.RecordedAtSource = &f, model.RecordedAtFilename
			} else if l.RecordedAtSource == model.RecordedAtFilename {
				l.RecordedAt, l.RecordedAtSource = nil, model.RecordedAtNone
			}
		}
	}); err != nil {
		if oldKinds != nil {
			s.kctx().kindMaps[c.ID] = oldKinds
		} else {
			delete(s.kctx().kindMaps, c.ID)
		}
		writeErr(w, err)
		return
	}
	s.applyRule(c, rule, kindMap)
	delete(wd.proposals, c.ID)
	delete(wd.undo, c.ID)
	c.FilenameProposal, c.FilenameUndo = false, false
	s.fnChanged(c)
	writeJSON(w, http.StatusOK, *c)
}

func (m *mock) dismissFilenameProposal(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	wd := s.p2()
	if wd.proposals[c.ID] == nil {
		writeErr(w, codedErr(http.StatusNotFound, "filename_rule.no_proposal", nil, ""))
		return
	}
	delete(wd.proposals, c.ID)
	c.FilenameProposal = false
	s.courseChanged(c)
	writeJSON(w, http.StatusOK, *c)
}

// undoFilenameRule restores what the first automatic apply changed (unless
// the user changed it since) and drops the rule.
func (m *mock) undoFilenameRule(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.courseOr404(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	wd := s.p2()
	before, ok := wd.undo[c.ID]
	if !ok && !c.FilenameUndo {
		writeErr(w, codedErr(http.StatusNotFound, "filename_rule.no_undo", nil, ""))
		return
	}
	for i := range s.lectures {
		l := &s.lectures[i]
		if l.CourseID != c.ID || l.RecordedAtSource == model.RecordedAtUser {
			continue
		}
		if b, ok := before[l.ID]; ok {
			l.RecordedAt, l.RecordedAtSource = b.at, b.src
			if l.TitleSource == b.titleSrc {
				l.Title = b.title
			}
		} else if l.RecordedAtSource == model.RecordedAtFilename {
			// fixture data: everything came from the rule
			l.RecordedAt, l.RecordedAtSource = nil, model.RecordedAtNone
		}
	}
	delete(wd.undo, c.ID)
	delete(s.kctx().kindMaps, c.ID)
	c.FilenameRule, c.FilenameUndo = nil, false
	s.fnChanged(c)
	writeJSON(w, http.StatusOK, *c)
}

// ---------- 重新生成标题与简介 (P13.5) ----------

// titleTiming is the simulated title run (shortened by tests).
var titleTiming = 2 * time.Second

var weekPrefix = regexp.MustCompile(`^(?i:week\s*\d+\s*[-–:]\s*)`)

func (m *mock) regenerateTitle(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.TitleRegenerateInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	st, err := m.settingsStore(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	configured := st.organizerConfigured()
	st.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		writeErr(w, codedErr(http.StatusNotFound, "lecture.not_found", nil, ""))
		return
	}
	switch sum := stageStatus(l, model.StageSummary); {
	case !configured:
		writeErr(w, codedErr(http.StatusConflict, "title.not_configured", nil, ""))
		return
	case sum != model.StatusDone && sum != model.StatusStale:
		writeErr(w, codedErr(http.StatusConflict, "title.summary_not_ready", nil, ""))
		return
	case l.TitleSource == model.TitleSourceUser && !in.Force:
		writeErr(w, codedErr(http.StatusConflict, "title.user_set", nil, ""))
		return
	}
	s.startTitle(l, in.Force)
	writeJSON(w, http.StatusAccepted, *l)
}

// startTitle simulates the lecture's title task (P13.5): running → the
// model's title (unless the user's, without force) and blurbs. Must hold s.mu.
func (s *store) startTitle(l *api.Lecture, force bool) {
	wd := s.p2()
	wd.titleGen[l.ID]++
	gen, id := wd.titleGen[l.ID], l.ID
	setAux(l, auxStage(model.StageTitle, model.StatusRunning, time.Now()))
	s.changed(l)
	time.AfterFunc(titleTiming, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if wd.titleGen[id] != gen {
			return
		}
		l, err := s.findLecture(id)
		if err != nil {
			return
		}
		en := strings.TrimSpace(weekPrefix.ReplaceAllString(l.Title, ""))
		if l.TitleSource == model.TitleSourceFilename || force {
			zh := en
			if l.TitleZh != nil && *l.TitleZh != "" {
				zh = *l.TitleZh
			}
			l.Title, l.TitleZh, l.TitleSource = en, &zh, model.TitleSourceLLM
		}
		be := "What this lecture covers, in one sentence written from its summary: " + en + "."
		bz := "根据摘要写成的一句话简介：" + en + "。"
		l.BlurbEn, l.BlurbZh = &be, &bz
		setAux(l, auxStage(model.StageTitle, model.StatusDone, time.Now()))
		s.changed(l)
	})
}

// auxAction is POST /api/lectures/{id}/stages/title/{retry|cancel} (lead
// ruling 1: auxiliary tasks are retried only by the user). Must hold s.mu.
func (s *store) auxAction(l *api.Lecture, stage, action string) error {
	var cur *api.StageView
	for i := range l.Aux {
		if l.Aux[i].Stage == stage {
			cur = &l.Aux[i]
		}
	}
	if stage != model.StageTitle || cur == nil || !containsStr(cur.AllowedActions, action) {
		return codedErr(http.StatusConflict, "state.conflict", nil, "")
	}
	switch action {
	case model.ActionRetry:
		s.startTitle(l, false)
	case model.ActionCancel:
		s.p2().titleGen[l.ID]++
		setAux(l, auxStage(model.StageTitle, model.StatusCancelled, time.Now()))
		s.changed(l)
	}
	return nil
}

func setAux(l *api.Lecture, st *api.StageView) {
	for i := range l.Aux {
		if l.Aux[i].Stage == st.Stage {
			l.Aux[i] = *st
			return
		}
	}
	l.Aux = append(l.Aux, *st)
}

// ---------- holidays (P12.2) and UI settings (P14.6, P14.7) ----------

func (s *store) holidaySet() tariff.HolidaySet {
	if y := s.p2().holidays; y != nil {
		b, _ := json.Marshal(y)
		if h, err := tariff.ParseHolidays(b); err == nil {
			return h
		}
	}
	return tariff.HolidaysCN()
}

func (s *store) holidaysView() api.HolidaysView {
	if y := s.p2().holidays; y != nil {
		return api.HolidaysView{Years: y, Edited: true}
	}
	h := tariff.HolidaysCN()
	v := api.HolidaysView{Years: map[string][]string{}, Source: tariff.HolidaysCNSource()}
	years := []string{}
	for _, y := range h.Years() {
		v.Years[fmt.Sprint(y)] = h.Dates(y)
		years = append(years, fmt.Sprint(y))
	}
	// P14.3: the coded source, as internal/api holidaysSourceInfo
	v.SourceInfo = &model.ErrorInfo{Code: "holidays.source_cn", Params: map[string]any{"years": strings.Join(years, ", ")}, Detail: v.Source}
	return v
}

func (m *mock) getHolidays(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.holidaysView())
}

// putHolidays: {years} replaces the built-in file; null or {} restores it.
func (m *mock) putHolidays(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		Years map[string][]string `json:"years"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if len(in.Years) > 0 {
		b, _ := json.Marshal(in.Years)
		if _, err := tariff.ParseHolidays(b); err != nil {
			info := model.ErrorInfo{Code: tariff.CodeInvalidHolidayData, Detail: err.Error()}
			if te, ok := err.(*tariff.Error); ok {
				info.Code, info.Params, info.Detail = te.Code, te.Params, te.Msg
			}
			writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: info})
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(in.Years) == 0 {
		s.p2().holidays = nil
	} else {
		s.p2().holidays = in.Years
	}
	s.notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, s.holidaysView())
}

func (m *mock) getUI(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.p2().ui)
}

func (m *mock) putUI(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.UISettings
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	switch in.ZhDisplay {
	case "", "simplified", "tw", "hk":
	default:
		writeErr(w, codedErr(http.StatusBadRequest, "settings.invalid", map[string]any{"text": "zh_display"}, ""))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p2().ui = in
	s.notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, in)
}

// ---------- peak / off-peak (P12.4) ----------

// tariffOffset is how far the tariff clock runs ahead in the scenarios
// "peak" and "offpeak": to the nearest moment (a little way into the
// period) at which the first provider with peak windows is in that period;
// 0 otherwise. The snapshot's sampled_at is that moment, so screenshots set
// the browser clock to it (scripts/shots/course_p2.py) and every time on the
// page agrees. Must hold s.mu.
func (s *store) tariffOffset(scenario string, now time.Time) time.Duration {
	if scenario != "peak" && scenario != "offpeak" {
		return 0
	}
	want := scenario == "peak"
	h := s.holidaySet()
	for _, pv := range s.settings.Providers {
		if len(pv.Tariff) == 0 || strings.TrimSpace(string(pv.Tariff)) == "null" {
			continue
		}
		raw := string(pv.Tariff)
		p := model.Provider{ID: pv.ID, Tariff: &raw}
		for d := time.Duration(0); d < 8*24*time.Hour; d += 15 * time.Minute {
			ts := resources.TariffStateOf(p, now.Add(d), h)
			if ts == nil {
				break // no peak windows: try the next provider
			}
			if ts.Peak != want {
				continue
			}
			if d > 0 {
				if ts2 := resources.TariffStateOf(p, now.Add(d+45*time.Minute), h); ts2 != nil && ts2.Peak == want {
					d += 45 * time.Minute
				}
			}
			return d
		}
	}
	return 0
}

// tariffStateOf is the TariffState of a provider view at the given moment
// (nil without a tariff with peak windows). Must hold s.mu.
func (s *store) tariffStateOf(p api.ProviderView, at time.Time) *resources.TariffState {
	if len(p.Tariff) == 0 || strings.TrimSpace(string(p.Tariff)) == "null" {
		return nil
	}
	raw := string(p.Tariff)
	return resources.TariffStateOf(model.Provider{ID: p.ID, Tariff: &raw, OffpeakOnly: p.OffpeakOnly}, at, s.holidaySet())
}

// withTariffStates fills tariff_state of the snapshot's provider rows from
// the settings' tariffs. Must hold s.mu.
func (s *store) withTariffStates(r resources.Snapshot, now time.Time, scenario string) resources.Snapshot {
	at := now.Add(s.tariffOffset(scenario, now))
	if at != now {
		r.SampledAt = at.UTC().Format(time.RFC3339)
	}
	for i := range r.Providers {
		r.Providers[i].TariffState = nil
		if p, ok := s.providerByID(r.Providers[i].ID); ok {
			r.Providers[i].TariffState = s.tariffStateOf(p, at)
		}
	}
	return r
}
