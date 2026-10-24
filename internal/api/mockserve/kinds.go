// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Lecture kinds and per-kind numbering, and slides bound to a teaching week
// (internal/api/types_kinds.go, .claude/kinds-brief.md), as the real back
// end implements them:
//
//   - The kind catalog is global (GET/POST /api/lecture-kinds, PATCH/DELETE
//     /api/lecture-kinds/{id}): five built-in kinds plus the user's own.
//   - A lecture's kind: set by hand (user) > the "kind" group of the
//     course's file-name rule through its kind_map (filename) > the model's
//     judgement from the summary (llm, example data only) > none (a
//     lecture); the past-lecture rule of calendar.EffectiveKind on top.
//     PUT /api/lectures/{id}/kind sets it ("" = automatic again).
//   - Numbers: calendar.LabelKinds (course_p2.go numberCourse): a session
//     kind per teaching week and kind (W02T1), a material kind within the
//     course (P1, M1); KindN/KindCount/ShowN by the course's numbering.
//   - Slides: bind {scope: week|lecture|course}; week_suggested from the
//     file name (calendar.WeekFromFilename); Lecture.Materials lists what a
//     lecture uses and why.

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
)

// builtinKinds is the catalog migration 007 seeds (ord 10–50).
func builtinKinds() []api.LectureKind {
	return []api.LectureKind{
		{ID: "lecture", Builtin: true, Code: "L", Group: api.KindGroupSession, Order: 10},
		{ID: "tutorial", Builtin: true, Code: "T", Group: api.KindGroupSession, Order: 20},
		{ID: "seminar", Builtin: true, Code: "S", Group: api.KindGroupSession, Order: 30},
		{ID: "past_lecture", Builtin: true, Code: "P", Group: api.KindGroupMaterial, Order: 40},
		{ID: "material", Builtin: true, Code: "M", Group: api.KindGroupMaterial, Order: 50},
	}
}

// exampleKinds: the built-in kinds and one custom kind of the example data
// (not used by any lecture, so it can be deleted in the UI).
func exampleKinds() []api.LectureKind {
	name := "实验课"
	return append(builtinKinds(), api.LectureKind{ID: "custom-lab", Code: "LAB", Name: &name, Group: api.KindGroupSession, Order: 60})
}

// kindCtx is what the kind pass needs besides the lectures: the catalog and
// the kind_map of each course's saved rule (the real back end keeps it in
// course.filename_rule).
type kindCtx struct {
	catalog  []api.LectureKind
	kindMaps map[string]map[string]string // course id → kind_map
}

// fixtureKinds is the context of the example data: the example catalog and
// the kind maps of the example rules.
func fixtureKinds(courses []api.Course) *kindCtx {
	kc := &kindCtx{catalog: exampleKinds(), kindMaps: map[string]map[string]string{}}
	for _, c := range courses {
		if c.Code == nil || c.FilenameRule == nil {
			continue
		}
		if g, ok := mockRules[*c.Code]; ok && g.regex == c.FilenameRule.Regex && g.kindMap != nil {
			kc.kindMaps[c.ID] = g.kindMap
		}
	}
	return kc
}

func (s *store) kctx() *kindCtx {
	if s.kinds == nil {
		s.kinds = exampleKinds()
	}
	if s.kindMaps == nil {
		s.kindMaps = map[string]map[string]string{}
	}
	return &kindCtx{catalog: s.kinds, kindMaps: s.kindMaps}
}

// initKinds runs once after the fixtures are loaded.
func (s *store) initKinds() {
	kc := fixtureKinds(s.courses)
	s.kinds, s.kindMaps = kc.catalog, kc.kindMaps
}

func (kc *kindCtx) kind(id string) *api.LectureKind {
	if kc == nil {
		return nil
	}
	for i := range kc.catalog {
		if kc.catalog[i].ID == id {
			return &kc.catalog[i]
		}
	}
	return nil
}

// calendarKinds is the catalog for calendar.LabelKinds.
func (kc *kindCtx) calendarKinds() []calendar.Kind {
	out := make([]calendar.Kind, 0, len(kc.catalog))
	for _, k := range kc.catalog {
		out = append(out, calendar.Kind{ID: k.ID, Code: k.Code, Group: k.Group})
	}
	return out
}

func (kc *kindCtx) group(id string) string {
	if k := kc.kind(id); k != nil {
		return k.Group
	}
	return api.KindGroupSession
}

func (kc *kindCtx) code(id string) string {
	if k := kc.kind(id); k != nil {
		return k.Code
	}
	return "L"
}

// ruleKind applies a rule's "kind" group and kind_map to a file name: the
// captured code and the kind it maps to ("" when unmatched or unmapped),
// as calendar.Parse does it for the real back end.
func ruleKind(regex string, kindMap map[string]string, filename string) (code, kind string) {
	r, err := calendar.Compile(calendar.FilenameRule{Regex: regex, KindMap: kindMap})
	if err != nil {
		return "", ""
	}
	p := r.Parse(calendar.Sample{Filename: filename})
	if !p.Matched {
		return "", ""
	}
	return p.KindCode, p.Kind
}

// resolveKind sets the stored kind of l (user > filename > llm > none);
// l.Kind / l.KindSource hold the stored kind between passes, and a view's
// kind_source "date" (the past-lecture rule) reads as "not decided".
func resolveKind(c *api.Course, l *api.Lecture, kc *kindCtx) {
	if l.KindSource == api.KindSourceUser && kc.kind(l.Kind) != nil {
		return
	}
	if r := ruleOf(c); r != nil {
		if _, k := ruleKind(r.Regex, kc.kindMaps[c.ID], l.OriginalFilename); k != "" && kc.kind(k) != nil {
			l.Kind, l.KindSource = k, api.KindSourceFilename
			return
		}
	}
	if l.KindSource == api.KindSourceLLM && kc.kind(l.Kind) != nil {
		return
	}
	l.Kind, l.KindSource = "", api.KindSourceNone
}

// tagOf is the dashboard's short form of a lecture's kind and number.
func tagOf(l api.Lecture) api.KindTag {
	t := api.KindTag{Kind: l.Kind, KindN: l.KindN, ShowN: l.ShowN}
	if l.Numbered {
		t.Week = l.Week
	}
	return t
}

// ---------- slides by teaching week ----------

// weekFromFilename is Material.week_suggested (calendar.WeekFromFilename).
func weekFromFilename(name string) *int {
	if w, ok := calendar.WeekFromFilename(name); ok {
		return &w
	}
	return nil
}

// defaultBind is the binding of a new slide file: the lecture it was
// uploaded for, else the week in its name, else the whole course.
func defaultBind(lectureID *string, filename string) *api.MaterialBind {
	if lectureID != nil {
		return &api.MaterialBind{Scope: api.BindLecture, LectureID: lectureID}
	}
	if w := weekFromFilename(filename); w != nil {
		return &api.MaterialBind{Scope: api.BindWeek, Week: w}
	}
	return &api.MaterialBind{Scope: api.BindCourse}
}

// uploadBind is the binding of a slide file uploaded to course c: as
// defaultBind, but the week in its name only once the course has a term
// start (the real API's uploadWeek, review item 2); week_suggested still
// names it.
func uploadBind(c *api.Course, lectureID *string, filename string) *api.MaterialBind {
	if ts, _ := termOf(c); ts == nil && lectureID == nil {
		return &api.MaterialBind{Scope: api.BindCourse}
	}
	return defaultBind(lectureID, filename)
}

// materialsCourse keeps bind and lecture_id consistent and lists each
// lecture's slides (Lecture.Materials). idx: the course's lectures.
func materialsCourse(c *api.Course, lectures []api.Lecture, idx []int) {
	for i := range c.Materials {
		m := &c.Materials[i]
		m.WeekSuggested = weekFromFilename(m.Filename)
		if m.Bind == nil {
			m.Bind = defaultBind(m.LectureID, "")
		}
		if m.Bind.Scope == api.BindLecture {
			m.LectureID = m.Bind.LectureID
		} else {
			m.LectureID = nil
		}
	}
	for _, i := range idx {
		l := &lectures[i]
		own, week, course := []api.MaterialUse{}, []api.MaterialUse{}, []api.MaterialUse{}
		for _, m := range c.Materials {
			use := api.MaterialUse{ID: m.ID, Filename: m.Filename, Pages: m.Pages, Via: m.Bind.Scope}
			switch m.Bind.Scope {
			case api.BindLecture:
				if m.Bind.LectureID != nil && *m.Bind.LectureID == l.ID {
					own = append(own, use)
				}
			case api.BindWeek:
				if l.Week > 0 && m.Bind.Week != nil && *m.Bind.Week == l.Week {
					use.Week = l.Week
					week = append(week, use)
				}
			default:
				course = append(course, use)
			}
		}
		l.Materials = append(append(own, week...), course...)
	}
}

// ---------- HTTP ----------

func (m *mock) registerKinds(mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("GET /api/lecture-kinds", a(m.listKinds))
	mux.HandleFunc("POST /api/lecture-kinds", a(m.createKind))
	mux.HandleFunc("PATCH /api/lecture-kinds/{id}", a(m.patchKind))
	mux.HandleFunc("DELETE /api/lecture-kinds/{id}", a(m.deleteKind))
	mux.HandleFunc("PUT /api/lectures/{id}/kind", a(m.setLectureKind))
	mux.HandleFunc("DELETE /api/courses/{id}/renumber-notices", a(m.dismissRenumberNotices))
}

// dismissRenumberNotices is DELETE /api/courses/{id}/renumber-notices.
func (m *mock) dismissRenumberNotices(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	delete(s.renumNotices, c.ID)
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
	writeJSON(w, http.StatusOK, m.courseOut(r, s, *c))
}

// setupRenumbered is the scenario "renumbered": COMP3311 gets a term start
// and an exported citation JSON, a second lecture in week 2 (Normalisation,
// Thu) and the title task's judgement that the Tuesday session is a
// tutorial (llm), which moved Normalisation from W02L2 to W02L1 — the
// notice stages records (calendar.renumbered_auto). Before it, the
// one-time check after the past-lecture rule shipped (state
// CheckPastRuleRenumbering) recorded calendar.renumbered_upgrade for a
// 2025 recording that went from W??-01JD33 to P1.
func setupRenumbered(s *store, now time.Time) {
	var c *api.Course
	for i := range s.courses {
		if s.courses[i].Code != nil && *s.courses[i].Code == "COMP3311" {
			c = &s.courses[i]
		}
	}
	tue, err := s.findLecture("01JD3311000000000000000002")
	if c == nil || err != nil {
		return
	}
	term := "2026-09-28"
	c.TermStart, c.CiteExported = &term, true
	thu := *tue
	thu.ID, thu.Title, thu.OriginalFilename = "01JD3311000000000000000005", "Normalisation", "db-lecture-2b.mp4"
	rec := "2026-10-08T19:00"
	thu.RecordedAt = &rec
	zhTitle := "关系规范化"
	thu.TitleZh, thu.BlurbEn, thu.BlurbZh = &zhTitle, nil, nil
	thu.Stages = append([]api.StageView(nil), tue.Stages...)
	thu.Exports = append([]api.ExportFile(nil), tue.Exports...)
	thu.Kind, thu.KindSource = "lecture", api.KindSourceLLM
	tue.Kind, tue.KindSource = "tutorial", api.KindSourceLLM
	old := thu
	old.ID, old.Title, old.OriginalFilename = "01JD3311000000000000000006", "Relational Algebra (2025 recording)", "db-2025-lecture-3.mp4"
	oldRec, oldZh := "2025-10-14T09:00", "关系代数（2025 年录像）"
	old.RecordedAt, old.TitleZh = &oldRec, &oldZh
	old.Stages = append([]api.StageView(nil), tue.Stages...)
	old.Exports = append([]api.ExportFile(nil), tue.Exports...)
	old.Kind, old.KindSource = "", api.KindSourceNone // the past-lecture rule makes it P1
	s.lectures = append(s.lectures, thu, old)
	s.refreshAll()
	if s.renumNotices == nil {
		s.renumNotices = map[string][]api.RenumberNotice{}
	}
	s.renumNotices[c.ID] = []api.RenumberNotice{{
		ErrorInfo: model.ErrorInfo{Code: "calendar.renumbered_upgrade",
			Params: map[string]any{"n": 1,
				"changes": []map[string]any{{"lecture_id": old.ID, "from": "W??-01JD33", "to": "P1"}}},
			Detail: "录制时间早于开课 30 天以上的讲座改按往期讲座编号后，1 节讲座的编号发生变化（W??-01JD33 → P1），之前导出的引用将与新编号不一致"},
		At: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}, {
		ErrorInfo: model.ErrorInfo{Code: "calendar.renumbered_auto",
			Params: map[string]any{"lecture": "01JD3311000000000000000002", "n": 1,
				"changes": []map[string]any{{"lecture_id": thu.ID, "from": "W02L2", "to": "W02L1"}}},
			Detail: "自动识别讲座类型后，另有 1 节讲座的编号发生变化（W02L2 → W02L1），之前导出的引用将与新编号不一致"},
		At: now.Add(-20 * time.Minute).UTC().Format(time.RFC3339),
	}}
}

func (s *store) kindList() []api.LectureKind {
	out := append([]api.LectureKind{}, s.kctx().catalog...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func (m *mock) listKinds(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.kindList())
}

var kindCodeRE = regexp.MustCompile(`^[A-Z]{1,3}$`)

// checkKind validates a custom kind's fields against the catalog (self:
// the kind being edited, "" when creating).
func (s *store) checkKind(self, name, code, group string) error {
	if strings.TrimSpace(name) == "" {
		return codedErr(http.StatusBadRequest, "kind.name_empty", nil, "")
	}
	if len([]rune(name)) > 40 {
		return codedErr(http.StatusBadRequest, "kind.name_too_long", map[string]any{"max": 40}, "")
	}
	if !kindCodeRE.MatchString(code) {
		return codedErr(http.StatusBadRequest, "kind.code_invalid", map[string]any{"code": code}, "")
	}
	if group != api.KindGroupSession && group != api.KindGroupMaterial {
		return codedErr(http.StatusBadRequest, "kind.group_invalid", map[string]any{"group": group}, "")
	}
	for _, k := range s.kctx().catalog {
		if k.ID == self {
			continue
		}
		if k.Code == code {
			return codedErr(http.StatusConflict, "kind.code_taken", map[string]any{"code": code}, "")
		}
		if k.Name != nil && strings.EqualFold(strings.TrimSpace(*k.Name), strings.TrimSpace(name)) {
			return codedErr(http.StatusConflict, "kind.name_taken", map[string]any{"name": name}, "")
		}
	}
	return nil
}

// kindUse counts the lectures whose kind is id (CountLecturesByKind; the
// mock keeps the stored kind in the view, see numberCourse).
func (s *store) kindUse(id string) int {
	n := 0
	for _, l := range s.lectures {
		if l.Kind == id && l.KindSource != api.KindSourceDate {
			n++
		}
	}
	return n
}

// kindsChanged renumbers every course and tells the pages.
func (s *store) kindsChanged() {
	for i := range s.courses {
		finalizeCourse(&s.courses[i], s.lectures, s.kctx())
		s.notify(api.ChangedEvent{Kind: "course", ID: s.courses[i].ID})
	}
	s.notify(api.ChangedEvent{Kind: "kinds"})
}

func (m *mock) createKind(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.LectureKindInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	name, code, group := deref(in.Name), strings.ToUpper(strings.TrimSpace(deref(in.Code))), deref(in.Group)
	if group == "" {
		group = api.KindGroupSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkKind("", name, code, group); err != nil {
		writeErr(w, err)
		return
	}
	kc := s.kctx()
	order := 0
	for _, k := range kc.catalog {
		if k.Order > order {
			order = k.Order
		}
	}
	n := strings.TrimSpace(name)
	k := api.LectureKind{ID: s.newID("custom-"), Code: code, Name: &n, Group: group, Order: order + 10}
	s.kinds = append(s.kinds, k)
	s.kindsChanged()
	writeJSON(w, http.StatusCreated, k)
}

func (m *mock) patchKind(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.LectureKindInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.kctx().kind(r.PathValue("id"))
	switch {
	case k == nil:
		writeErr(w, codedErr(http.StatusNotFound, "kind.not_found", nil, ""))
		return
	case k.Builtin:
		writeErr(w, codedErr(http.StatusBadRequest, "kind.builtin", nil, ""))
		return
	case in.Name == nil && in.Code == nil && in.Group == nil:
		writeErr(w, codedErr(http.StatusBadRequest, "kind.nothing_to_change", nil, ""))
		return
	}
	name, code, group := deref(k.Name), k.Code, k.Group
	if in.Name != nil {
		name = *in.Name
	}
	if in.Code != nil {
		code = strings.ToUpper(strings.TrimSpace(*in.Code))
	}
	if in.Group != nil {
		group = *in.Group
	}
	if err := s.checkKind(k.ID, name, code, group); err != nil {
		writeErr(w, err)
		return
	}
	// the code and the group are part of the citation numbers: only while unused
	if n := s.kindUse(k.ID); n > 0 && (code != k.Code || group != k.Group) {
		writeErr(w, codedErr(http.StatusConflict, "kind.in_use", map[string]any{"count": n}, ""))
		return
	}
	n := strings.TrimSpace(name)
	k.Name, k.Code, k.Group = &n, code, group
	out := *k
	s.kindsChanged()
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) deleteKind(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	k := s.kctx().kind(id)
	switch {
	case k == nil:
		writeErr(w, codedErr(http.StatusNotFound, "kind.not_found", nil, ""))
		return
	case k.Builtin:
		writeErr(w, codedErr(http.StatusBadRequest, "kind.builtin", nil, ""))
		return
	}
	if n := s.kindUse(id); n > 0 {
		writeErr(w, codedErr(http.StatusConflict, "kind.in_use", map[string]any{"count": n}, ""))
		return
	}
	for i := range s.kinds {
		if s.kinds[i].ID == id {
			s.kinds = append(s.kinds[:i], s.kinds[i+1:]...)
			break
		}
	}
	for c, km := range s.kindMaps { // a rule may not point at a kind that is gone
		for code, kid := range km {
			if kid == id {
				delete(s.kindMaps[c], code)
			}
		}
	}
	s.kindsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// setLectureKind is PUT /api/lectures/{id}/kind {kind, confirm}: the user's
// kind ("" = automatic again). Citation numbers may change (409
// calendar.renumber when the course's citations were exported).
func (m *mock) setLectureKind(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.LectureKindSet
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		writeErr(w, codedErr(http.StatusNotFound, "lecture.not_found", nil, ""))
		return
	}
	kind := strings.TrimSpace(in.Kind)
	if kind != "" && s.kctx().kind(kind) == nil {
		writeErr(w, codedErr(http.StatusBadRequest, "kind.unknown", map[string]any{"kind": kind}, ""))
		return
	}
	c, err := s.findCourse(l.CourseID)
	if err != nil {
		writeErr(w, err)
		return
	}
	src := api.KindSourceUser
	if kind == "" {
		src = api.KindSourceNone // automatic: the rule decides again (the example's model judgement is not kept)
	}
	id := l.ID
	if err := s.renumberCheck(c, in.Confirm, func(_ *api.Course, lecs []api.Lecture) {
		for i := range lecs {
			if lecs[i].ID == id {
				lecs[i].Kind, lecs[i].KindSource = kind, src
			}
		}
	}); err != nil {
		writeErr(w, err)
		return
	}
	l.Kind, l.KindSource = kind, src
	s.finalize(l)
	s.fnChanged(c) // the other lectures of the week are renumbered too
	writeJSON(w, http.StatusOK, *l)
}

// patchNumbering is the numbering field of PATCH /api/courses/{id}.
func patchNumbering(c *api.Course, v *string) error {
	if v == nil {
		return nil
	}
	switch *v {
	case api.NumberingAuto, api.NumberingAlways, api.NumberingNever:
		c.Numbering = *v
		return nil
	}
	return codedErr(http.StatusBadRequest, "course.numbering_invalid", map[string]any{"value": *v}, "")
}

// parseBind validates the bind of PATCH /api/materials/{id} for course c.
func (s *store) parseBind(c *api.Course, b api.MaterialBind) (*api.MaterialBind, error) {
	switch b.Scope {
	case api.BindCourse:
		return &api.MaterialBind{Scope: api.BindCourse}, nil
	case api.BindWeek:
		if b.Week == nil || *b.Week < 1 || *b.Week > 60 {
			return nil, codedErr(http.StatusBadRequest, "material.bind_week_invalid", nil, "")
		}
		w := *b.Week
		return &api.MaterialBind{Scope: api.BindWeek, Week: &w}, nil
	case api.BindLecture:
		if b.LectureID == nil {
			return nil, codedErr(http.StatusBadRequest, "material.bind_invalid", nil, "")
		}
		if l, err := s.findLecture(*b.LectureID); err != nil || l.CourseID != c.ID {
			return nil, codedErr(http.StatusBadRequest, "material.bind_lecture_other_course", nil, "")
		}
		id := *b.LectureID
		return &api.MaterialBind{Scope: api.BindLecture, LectureID: &id}, nil
	}
	return nil, codedErr(http.StatusBadRequest, "material.bind_invalid", nil, "")
}
