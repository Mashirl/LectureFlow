// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// Lecture kinds (user request 2026-10-23; lead freeze .claude/kinds-brief.md
// section 3): the global kind catalog, a lecture's kind, and how the kinds
// reach the views (numbering itself: v2_calendar.go numberLectures through
// state.NumberCourse).

// kindView is the API form of a catalog entry.
func kindView(k model.LectureKind) LectureKind {
	v := LectureKind{ID: k.ID, Builtin: k.Builtin, Code: k.Code, Group: k.Group, Order: k.Order}
	if !k.Builtin && k.Name != nil {
		n := *k.Name
		v.Name = &n
	}
	return v
}

// GET /api/lecture-kinds → []LectureKind in catalog order (built-in first).
func (s *Server) listKinds(w http.ResponseWriter, r *http.Request) {
	ks, err := s.st.ListLectureKinds(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]LectureKind, 0, len(ks))
	for _, k := range ks {
		out = append(out, kindView(k))
	}
	writeJSON(w, http.StatusOK, out)
}

// kindFields validates the fields of a custom kind: the name (trimmed, 1–40
// characters), the code (trimmed, upper-cased, 1–3 letters) and the group.
func kindFields(name, code, group string) (string, string, string, error) {
	name, code = strings.TrimSpace(name), strings.ToUpper(strings.TrimSpace(code))
	switch {
	case name == "":
		return "", "", "", apiErr(http.StatusBadRequest, "kind.name_empty", nil)
	case utf8.RuneCountInString(name) > model.KindNameMaxLen:
		return "", "", "", apiErr(http.StatusBadRequest, "kind.name_too_long", map[string]any{"max": model.KindNameMaxLen})
	case !model.ValidKindCode(code):
		return "", "", "", apiErr(http.StatusBadRequest, "kind.code_invalid", map[string]any{"code": code})
	case !model.ValidKindGroup(group):
		return "", "", "", apiErr(http.StatusBadRequest, "kind.group_invalid", map[string]any{"group": group})
	}
	return name, code, group, nil
}

// kindWriteErr maps the Store's duplicate errors to kind.code_taken /
// kind.name_taken (409).
func kindWriteErr(err error, name, code string) error {
	switch {
	case errors.Is(err, db.ErrKindCodeTaken):
		return apiErr(http.StatusConflict, "kind.code_taken", map[string]any{"code": code})
	case errors.Is(err, db.ErrKindNameTaken):
		return apiErr(http.StatusConflict, "kind.name_taken", map[string]any{"name": name})
	case errors.Is(err, db.ErrBuiltinKind):
		return apiErr(http.StatusBadRequest, "kind.builtin", nil)
	}
	return notFoundAs(err, "kind.not_found")
}

// POST /api/lecture-kinds {name, code, group} → 201 LectureKind (a custom
// kind; group defaults to session). 400 kind.name_empty, name_too_long,
// code_invalid, group_invalid; 409 kind.code_taken, kind.name_taken.
func (s *Server) createKind(w http.ResponseWriter, r *http.Request) {
	var in LectureKindInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	group := strVal(in.Group)
	if group == "" {
		group = model.KindGroupSession
	}
	name, code, group, err := kindFields(strVal(in.Name), strVal(in.Code), group)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	k, err := s.st.InsertLectureKind(r.Context(), model.LectureKind{Code: code, Name: &name, Group: group})
	if err != nil {
		s.fail(w, r, kindWriteErr(err, name, code))
		return
	}
	s.hub.Changed("kinds", "")
	writeJSON(w, http.StatusCreated, kindView(k))
}

// PATCH /api/lecture-kinds/{id} {name?, code?, group?} → LectureKind. Only
// custom kinds (400 kind.builtin); 400 kind.nothing_to_change without a
// field; the code and the group are part of citation numbers, so they
// change only while no lecture's stored kind is this one (409 kind.in_use
// {count}).
func (s *Server) patchKind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in LectureKindInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	var out model.LectureKind
	err := s.st.Tx(ctx, func(tx db.Store) error {
		k, err := tx.GetLectureKind(ctx, r.PathValue("id"))
		if err != nil {
			return notFoundAs(err, "kind.not_found")
		}
		switch {
		case k.Builtin:
			return apiErr(http.StatusBadRequest, "kind.builtin", nil)
		case in.Name == nil && in.Code == nil && in.Group == nil:
			return apiErr(http.StatusBadRequest, "kind.nothing_to_change", nil)
		}
		name, code, group := strVal(k.Name), k.Code, k.Group
		if in.Name != nil {
			name = *in.Name
		}
		if in.Code != nil {
			code = *in.Code
		}
		if in.Group != nil {
			group = *in.Group
		}
		if name, code, group, err = kindFields(name, code, group); err != nil {
			return err
		}
		if code != k.Code || group != k.Group {
			counts, err := tx.CountLecturesByKind(ctx, "")
			if err != nil {
				return err
			}
			if n := counts[k.ID]; n > 0 {
				return apiErr(http.StatusConflict, "kind.in_use", map[string]any{"count": n})
			}
		}
		k.Name, k.Code, k.Group = &name, code, group
		if err := tx.UpdateLectureKind(ctx, k); err != nil {
			return kindWriteErr(err, name, code)
		}
		out = k
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// lecture views name a kind through the catalog: the front end reloads
	// it on "kinds"; no number changes (code and group only while unused)
	s.hub.Changed("kinds", "")
	writeJSON(w, http.StatusOK, kindView(out))
}

// DELETE /api/lecture-kinds/{id} → 204. Only custom kinds (400
// kind.builtin) that no lecture uses as its stored kind (409 kind.in_use
// {count}); the kind_map entries of course rules (and of waiting rule
// proposals) that point at it are dropped.
func (s *Server) deleteKind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var touched []string
	err := s.st.Tx(ctx, func(tx db.Store) error {
		k, err := tx.GetLectureKind(ctx, id)
		if err != nil {
			return notFoundAs(err, "kind.not_found")
		}
		if k.Builtin {
			return apiErr(http.StatusBadRequest, "kind.builtin", nil)
		}
		counts, err := tx.CountLecturesByKind(ctx, "")
		if err != nil {
			return err
		}
		if n := counts[k.ID]; n > 0 {
			return apiErr(http.StatusConflict, "kind.in_use", map[string]any{"count": n})
		}
		if err := tx.DeleteLectureKind(ctx, k.ID); err != nil {
			if errors.Is(err, db.ErrKindInUse) {
				return apiErr(http.StatusConflict, "kind.in_use", map[string]any{"count": 1})
			}
			return kindWriteErr(err, strVal(k.Name), k.Code)
		}
		touched, err = dropKindFromRules(ctx, tx, k.ID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("kinds", "")
	for _, cid := range touched {
		s.hub.Changed("course", cid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// dropKindFromRules removes the kind_map entries that point at kind id
// from every course's file-name rule and waiting proposal; it returns the
// ids of the courses it changed.
func dropKindFromRules(ctx context.Context, tx db.Store, id string) ([]string, error) {
	cs, err := tx.ListCourses(ctx)
	if err != nil {
		return nil, err
	}
	drop := func(km map[string]string) (map[string]string, bool) {
		changed := false
		for code, k := range km {
			if k == id {
				delete(km, code)
				changed = true
			}
		}
		if len(km) == 0 {
			km = nil
		}
		return km, changed
	}
	var touched []string
	for _, c := range cs {
		changed := false
		if rule := parseRule(c.FilenameRule); rule != nil {
			var ok bool
			if rule.KindMap, ok = drop(rule.KindMap); ok {
				raw, err := json.Marshal(rule)
				if err != nil {
					return nil, err
				}
				v := string(raw)
				if err := tx.SetCourseFilenameRule(ctx, c.ID, &v); err != nil {
					return nil, err
				}
				changed = true
			}
		}
		raw, ok, err := tx.GetKV(ctx, model.KVFilenameProposal+c.ID)
		if err != nil {
			return nil, err
		}
		if ok && strings.TrimSpace(raw) != "" {
			var p model.FilenameProposal
			var rule calendar.FilenameRule
			if json.Unmarshal([]byte(raw), &p) == nil && json.Unmarshal(p.Rule, &rule) == nil {
				var dropped bool
				if rule.KindMap, dropped = drop(rule.KindMap); dropped {
					rb, err := json.Marshal(rule)
					if err != nil {
						return nil, err
					}
					p.Rule = rb
					pb, err := json.Marshal(p)
					if err != nil {
						return nil, err
					}
					if err := tx.SetKV(ctx, model.KVFilenameProposal+c.ID, string(pb)); err != nil {
						return nil, err
					}
					changed = true
				}
			}
		}
		if changed {
			touched = append(touched, c.ID)
		}
	}
	return touched, nil
}

// PUT /api/lectures/{id}/kind {kind, confirm?} → Lecture. kind is a kind
// id of the catalog (the user's: kind_source user; 400 kind.unknown), or ""
// for the automatic kind again (the course's file-name rule through its
// kind_map, else the title task's remembered judgement with source llm —
// state.FallbackKind —, else not decided until the next title task). A
// user kind replacing an llm kind keeps that judgement
// (state.WriteLectureKind). A change of citation numbers in a course whose
// citations were exported answers 409 RenumberConflict unless confirmed
// (P13.6).
func (s *Server) setLectureKind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in LectureKindSet
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.courseOr404(ctx, l.CourseID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kind, src := strings.TrimSpace(in.Kind), model.KindSourceUser
	if kind != "" {
		if !kindKnown(kinds, kind) {
			writeError(w, http.StatusBadRequest, "kind.unknown", map[string]any{"kind": kind})
			return
		}
	} else if kind, src = automaticKind(c, l, kinds); src == model.KindSourceNone {
		// no file-name kind: the model's remembered judgement (source
		// llm; state.FallbackKind), else not decided
		if kind, src, err = state.FallbackKind(ctx, s.st, l, kinds); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// course-ux: a kind that contradicts the week override ends it
	dropOverride := dropOverrideForKind(l, kind, src, kinds)
	after := make([]model.Lecture, len(lecs))
	for i, o := range lecs {
		if o.ID == l.ID {
			o.Kind, o.KindSource = kind, src
			if dropOverride {
				o.WeekOverride = nil
			}
		}
		after[i] = o
	}
	if err := s.checkRenumber(ctx, c, lecs, c, after, in.Confirm); err != nil {
		s.failRenumber(w, r, err)
		return
	}
	// an llm kind the user replaces is remembered (state.WriteLectureKind)
	if err := s.st.Tx(ctx, func(tx db.Store) error {
		cur, err := tx.GetLecture(ctx, l.ID)
		if err != nil {
			return err
		}
		if err := state.WriteLectureKind(ctx, tx, cur, kind, src); err != nil {
			return err
		}
		if dropOverride {
			if err := tx.DeleteWeekOverride(ctx, l.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
				return err
			}
		}
		return nil
	}); err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	// The kind does not move the lecture to another week, but the freeze
	// (1.5) lists it with the changes that may alter material sets; an
	// unchanged set costs nothing.
	s.materialSetsChanged(c.ID)
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", c.ID) // the other lectures of its week are renumbered too
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// DELETE /api/courses/{id}/renumber-notices: the user read the notices of
// automatic renumbering (Course.renumber_notices; stages records them when
// the title task's kind renumbers other lectures after a citation export).
// → the course.
func (s *Server) dismissRenumberNotices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := stages.DismissRenumberNotices(ctx, s.st, c.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}

// kindKnown reports whether id is in the catalog.
func kindKnown(kinds []model.LectureKind, id string) bool {
	for _, k := range kinds {
		if k.ID == id {
			return true
		}
	}
	return false
}

// fileNameKind is the kind the course's file-name rule gives lecture l
// through its kind_map ("" = none, or a kind not in the catalog).
func fileNameKind(c model.Course, l model.Lecture, kinds []model.LectureKind) string {
	rule := parseRule(c.FilenameRule)
	if rule == nil || len(rule.KindMap) == 0 || strings.TrimSpace(l.OriginalFilename) == "" {
		return ""
	}
	compiled, err := calendar.Compile(*rule)
	if err != nil {
		return ""
	}
	k := compiled.Parse(calendar.Sample{Filename: l.OriginalFilename, UploadedAt: uploadedAt(l)}).Kind
	if !kindKnown(kinds, k) {
		return ""
	}
	return k
}

// automaticKind is the kind of lecture l when the user's kind is cleared:
// the file-name rule's kind (source filename), else not decided (the
// caller then tries the remembered llm kind, state.FallbackKind).
func automaticKind(c model.Course, l model.Lecture, kinds []model.LectureKind) (string, string) {
	if k := fileNameKind(c, l, kinds); k != "" {
		return k, model.KindSourceFilename
	}
	return "", model.KindSourceNone
}

// predictKind is the stored kind and source of lecture l after course c's
// file-name rule is applied, for the renumbering check made before
// anything is written (as stages.ApplyFilenameRule writes it): the user's
// kind stays; a kind the rule gives replaces a weaker or file-name one
// (calendar.KindOverrides); a file-name kind the rule no longer gives falls
// back to the model's judgement llm (state.RememberedLLMKind of l; source
// llm when it is in the catalog), else is not decided any more.
func predictKind(c model.Course, l model.Lecture, kinds []model.LectureKind, llm string) (string, string) {
	src := l.KindSource
	if !model.ValidKindSource(src) {
		src = model.KindSourceNone
	}
	if src == model.KindSourceUser {
		return l.Kind, src
	}
	if k := fileNameKind(c, l, kinds); k != "" && calendar.KindOverrides(model.KindSourceFilename, src) {
		return k, model.KindSourceFilename
	}
	if src == model.KindSourceFilename {
		return state.LLMFallback(llm, kinds)
	}
	return l.Kind, src
}

// kindTag is the dashboard's short form of a lecture's kind and number.
func kindTag(n lectureNumber) KindTag {
	t := KindTag{Kind: n.Kind, KindN: n.KindN, ShowN: n.ShowN}
	if t.Kind == "" {
		t.Kind = model.KindLecture
	}
	if n.OK {
		t.Week = n.Week
	}
	return t
}

// lectureMaterials lists, for every lecture of a course, the slides it uses
// and why (Lecture.materials): state.MaterialSetOf over the course's
// materials with the teaching week of its number (the same week
// state.LectureWeek computes). ms: the course's materials (ListMaterialRefs,
// upload order; no page texts); pages: their page counts
// (MaterialPageCounts; missing = 0).
func lectureMaterials(ms []model.Material, pages map[string]int, nums numbering) map[string][]MaterialUse {
	out := make(map[string][]MaterialUse, len(nums))
	for id, n := range nums {
		uses := []MaterialUse{}
		for _, u := range state.MaterialSetOf(ms, id, n.Week) {
			mu := MaterialUse{ID: u.Material.ID, Filename: u.Material.Filename, Pages: pages[u.Material.ID], Via: u.Via}
			if u.Via == model.BindWeek && u.Material.BindWeek != nil {
				mu.Week = *u.Material.BindWeek
			}
			uses = append(uses, mu)
		}
		out[id] = uses
	}
	return out
}

// materialBind is Material.bind of m.
func materialBind(m model.Material) *MaterialBind {
	b := &MaterialBind{Scope: model.BindScope(m)}
	switch b.Scope {
	case BindLecture:
		id := *m.LectureID
		b.LectureID = &id
	case BindWeek:
		w := *m.BindWeek
		b.Week = &w
	}
	return b
}

// weekSuggested is Material.week_suggested: the teaching week in the file
// name (calendar.WeekFromFilename), nil when none.
func weekSuggested(filename string) *int {
	if w, ok := calendar.WeekFromFilename(filename); ok {
		return &w
	}
	return nil
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
