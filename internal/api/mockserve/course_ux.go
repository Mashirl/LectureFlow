// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"slices"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// Course page requests of 2026-10-27 (course-ux; internal/api/course_ux.go):
// POST /api/courses/{id}/rerun (batch rerun of one stage, with dry_run),
// PUT / DELETE /api/lectures/{id}/week (week overrides). Scenario
// ?mock=courseux: CSC1003's "One AES Round by Hand" hangs under week 3,
// "Block Ciphers and DES" is week 1's make-up ("补"), COMP2120's "Memory
// Management" (recorded in the reading week) is week 3's make-up; the past
// lecture "Hash Functions and SHA-2" is not hung anywhere yet (drag it).
//
// Mock limitation: numberCourse writes the effective kind back as the
// stored one, so a make-up of a teaching material (shown as a lecture)
// stays a lecture after the make-up is undone; the real server keeps the
// stored kind.

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/courses/{id}/rerun", m.api(m.courseRerun))
		mux.HandleFunc("PUT /api/lectures/{id}/week", m.api(m.setLectureWeek))
		mux.HandleFunc("DELETE /api/lectures/{id}/week", m.api(m.clearLectureWeek))
	})
}

func setupCourseUX(s *store) {
	at := time.Now().UTC().Format(time.RFC3339)
	set := func(id, kind string, week int) {
		if l, err := s.findLecture(id); err == nil {
			l.WeekOverride = &api.WeekOverride{Week: week, Kind: kind, CreatedAt: at}
		}
	}
	set(lecID3631(10), api.WeekOverrideSupplement, 3) // One AES Round by Hand (material)
	set(lecID3631(4), api.WeekOverrideMakeup, 1)      // Block Ciphers and DES
	set(lecID2120(6), api.WeekOverrideMakeup, 3)      // Memory Management (reading week)
	for i := range s.courses {
		s.fnChanged(&s.courses[i])
	}
}

// rerunBlock is sched.rerunBlock on a mock lecture (stage index i).
func rerunBlock(l *api.Lecture, i int) *model.ErrorInfo {
	if l.Draft {
		return &model.ErrorInfo{Code: "rerun.skipped_draft"}
	}
	st := l.Stages[i]
	if st.Status != model.StatusDone && st.Status != model.StatusStale {
		return &model.ErrorInfo{Code: "rerun.skipped_status", Params: map[string]any{"stage": st.Stage, "status": st.Status}}
	}
	for _, d := range l.Stages[i:] {
		if d.Status == model.StatusRunning {
			return &model.ErrorInfo{Code: "rerun.skipped_busy", Params: map[string]any{"stage": d.Stage}}
		}
	}
	return nil
}

func (m *mock) courseRerun(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.CourseRerunRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, codedErr(http.StatusNotFound, "course.not_found", nil, ""))
		return
	}
	if !slices.Contains(model.LectureStages, in.Stage) {
		writeErr(w, codedErr(http.StatusBadRequest, "stage.invalid", map[string]any{"stage": in.Stage, "action": model.ActionRerun}, ""))
		return
	}
	switch in.ModelChoice {
	case "", model.ModelChoiceSnapshot, model.ModelChoiceCurrent:
	default:
		writeErr(w, codedErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": "model_choice"}, ""))
		return
	}
	var ids []string
	for _, id := range in.LectureIDs {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeErr(w, codedErr(http.StatusBadRequest, "lecture.batch_empty", nil, ""))
		return
	}
	out := api.CourseRerunResult{Stage: in.Stage, DryRun: in.DryRun, Diff: []api.ModelDiffView{}, Results: []api.CourseRerunItem{}}
	items := make([]api.CourseRerunItem, len(ids))
	var run []int // indices of lectures to rerun
	for k, id := range ids {
		it := api.CourseRerunItem{LectureID: id, Result: api.RerunQueued}
		l, err := s.findLecture(id)
		switch {
		case err != nil:
			it.Result, it.Reason = api.RerunFailed, &model.ErrorInfo{Code: "lecture.not_found"}
		case l.CourseID != c.ID:
			it.Result, it.Reason = api.RerunSkipped, &model.ErrorInfo{Code: "rerun.other_course"}
		default:
			if b := rerunBlock(l, stageIndex(l, in.Stage)); b != nil {
				it.Result, it.Reason = api.RerunSkipped, b
			} else {
				run = append(run, k)
				if in.ModelChoice == "" {
					out.Diff = append(out.Diff, s.diffs(l, stageIndex(l, in.Stage), false)...)
				}
			}
		}
		items[k] = it
	}
	if !in.DryRun {
		if len(out.Diff) > 0 {
			writeErr(w, needsChoice(out.Diff))
			return
		}
		now := time.Now()
		for _, k := range run {
			l, err := s.stageAction(ids[k], in.Stage, model.ActionRerun, in.ModelChoice, now)
			if err != nil {
				items[k].Result, items[k].Reason = api.RerunFailed, &model.ErrorInfo{Code: "state.conflict", Detail: err.Error()}
				continue
			}
			v := *l
			items[k].Lecture = &v
		}
	}
	for _, it := range items {
		switch it.Result {
		case api.RerunQueued:
			out.Queued++
		case api.RerunSkipped:
			out.Skipped++
		default:
			out.Failed++
		}
		out.Results = append(out.Results, it)
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) setLectureWeek(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.WeekOverrideSet
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if !model.ValidWeekOverrideKind(in.Kind) {
		writeErr(w, codedErr(http.StatusBadRequest, "week_override.kind_invalid", map[string]any{"kind": in.Kind}, ""))
		return
	}
	if !model.ValidOverrideWeek(in.Week) {
		writeErr(w, codedErr(http.StatusBadRequest, "week_override.week_invalid", map[string]any{"week": in.Week, "max": model.MaxBindWeek}, ""))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, c, err := s.lectureAndCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if ts, _ := termOf(c); ts == nil {
		writeErr(w, codedErr(http.StatusConflict, "week_override.no_term_start", nil, ""))
		return
	}
	if in.Kind == api.WeekOverrideSupplement {
		k := s.kctx().kind(l.Kind)
		if l.WeekOverride != nil && l.WeekOverride.Kind == api.WeekOverrideMakeup || k == nil || k.Group != model.KindGroupMaterial {
			writeErr(w, codedErr(http.StatusConflict, "week_override.not_material", map[string]any{"kind": l.Kind}, ""))
			return
		}
	}
	o := &api.WeekOverride{Week: in.Week, Kind: in.Kind, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.renumberCheck(c, in.Confirm, func(_ *api.Course, lecs []api.Lecture) {
		for i := range lecs {
			if lecs[i].ID == l.ID {
				lecs[i].WeekOverride = o
			}
		}
	}); err != nil {
		writeErr(w, err)
		return
	}
	l.WeekOverride = o
	s.fnChanged(c)
	writeJSON(w, http.StatusOK, *l)
}

func (m *mock) clearLectureWeek(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, c, err := s.lectureAndCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if l.WeekOverride != nil {
		q := r.URL.Query().Get("confirm")
		if err := s.renumberCheck(c, q == "1" || q == "true", func(_ *api.Course, lecs []api.Lecture) {
			for i := range lecs {
				if lecs[i].ID == l.ID {
					lecs[i].WeekOverride = nil
				}
			}
		}); err != nil {
			writeErr(w, err)
			return
		}
		l.WeekOverride = nil
		s.fnChanged(c)
	}
	writeJSON(w, http.StatusOK, *l)
}

func (s *store) lectureAndCourse(id string) (*api.Lecture, *api.Course, error) {
	l, err := s.findLecture(id)
	if err != nil {
		return nil, nil, codedErr(http.StatusNotFound, "lecture.not_found", nil, "")
	}
	c, err := s.findCourse(l.CourseID)
	if err != nil {
		return nil, nil, err
	}
	return l, c, nil
}
