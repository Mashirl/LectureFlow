// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// Course page requests of 2026-10-27 (course-ux; NOTES.md "决定（course-ux：
// core-builder，2026-10-27）"):
//
//   - POST /api/courses/{id}/rerun {stage, lecture_ids, model_choice?,
//     dry_run?}: rerun one stage of several lectures of the course. Each
//     lecture goes through sched.Rerun, the single-lecture rerun; the model
//     question (v1.1 A4.3) is asked once for all (409 needs_model_choice
//     with every lecture's differences); lectures that cannot rerun the
//     stage now are skipped with the reason. dry_run answers the same
//     without changing anything, so the page asks one question.
//   - PUT /api/lectures/{id}/week {week, kind, confirm?} and DELETE
//     /api/lectures/{id}/week[?confirm=1]: the lecture's week override
//     (model.WeekOverride): a teaching material hung under a week
//     (supplement), or a lecture made the content of a week (makeup, "补").
//     Numbers may change, so P13.6's confirmation applies (409
//     calendar.renumber). Material sets may change too (the week decides
//     the week-bound slides), so the materials invalidation is requested.

func init() {
	for code, def := range courseUXCodes {
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("POST /api/courses/{id}/rerun", s.courseRerun)
		h("PUT /api/lectures/{id}/week", s.setLectureWeek)
		h("DELETE /api/lectures/{id}/week", s.clearLectureWeek)
	})
}

// courseUXCodes join errorCatalog (testdata/api/error_codes.json).
var courseUXCodes = map[string]errDef{
	"rerun.skipped_draft":         {"这节讲座还没有启动（草稿），不重跑", nil},
	"rerun.skipped_status":        {"{stage}当前为{status}，只有已完成或已过期的阶段可以重跑", []string{"stage", "status"}},
	"rerun.skipped_busy":          {"{stage}正在运行，等它结束后才能重跑", []string{"stage"}},
	"rerun.other_course":          {"这节讲座不属于这门课程", nil},
	"week_override.week_invalid":  {"周次 {week} 无效：应为 1–{max}", []string{"week", "max"}},
	"week_override.kind_invalid":  {"未知的归属方式：{kind}（应为 supplement 或 makeup）", []string{"kind"}},
	"week_override.no_term_start": {"这门课还没有开课日期，算不出教学周；请先在课程设置中填写开课日期", nil},
	"week_override.not_material":  {"只有课程补充内容（补充材料、往期讲座等）可以挂到某一周下", []string{"kind"}},
}

// weekOverrideView is Lecture.week_override of lecture l numbered n.
func weekOverrideView(l model.Lecture, n lectureNumber) *WeekOverride {
	o := l.WeekOverride
	if o == nil {
		return nil
	}
	return &WeekOverride{Week: o.Week, Kind: o.Kind, Applied: n.Override != "", CreatedAt: o.CreatedAt}
}

// ---- batch rerun ----

// rerunBlockInfo is the reason of a skipped lecture.
func rerunBlockInfo(b *sched.RerunBlock) *model.ErrorInfo {
	var info model.ErrorInfo
	switch b.Reason {
	case sched.RerunBlockDraft:
		info = model.ErrorInfo{Code: "rerun.skipped_draft"}
	case sched.RerunBlockBusy:
		info = model.ErrorInfo{Code: "rerun.skipped_busy", Params: map[string]any{"stage": b.Stage}}
	default:
		st := b.Status
		if st == "" {
			st = model.StatusPending // no current job of the stage: it has not run
		}
		info = model.ErrorInfo{Code: "rerun.skipped_status", Params: map[string]any{"stage": b.Stage, "status": st}}
	}
	info.Detail = zhText(info)
	return &info
}

// POST /api/courses/{id}/rerun.
func (s *Server) courseRerun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in CourseRerunRequest
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !slices.Contains(model.LectureStages, in.Stage) {
		writeError(w, http.StatusBadRequest, "stage.invalid", map[string]any{"stage": in.Stage, "action": model.ActionRerun})
		return
	}
	switch in.ModelChoice {
	case "", model.ModelChoiceSnapshot, model.ModelChoiceCurrent:
	default:
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "model_choice"})
		return
	}
	ids := make([]string, 0, len(in.LectureIDs))
	seen := map[string]bool{}
	for _, id := range in.LectureIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "lecture.batch_empty", nil)
		return
	}
	if len(ids) > maxBatch {
		writeError(w, http.StatusBadRequest, "lecture.batch_too_many", map[string]any{"max": maxBatch})
		return
	}

	// lectures of this course go to the scheduler; others are answered here
	items := make(map[string]CourseRerunItem, len(ids))
	var mine []string
	for _, id := range ids {
		l, err := s.st.GetLecture(ctx, id)
		switch {
		case errors.Is(err, db.ErrNotFound):
			_, info := s.batchError(ctx, notFoundAs(err, "lecture.not_found"), "")
			items[id] = CourseRerunItem{LectureID: id, Result: RerunFailed, Reason: info}
		case err != nil:
			s.fail(w, r, err)
			return
		case l.CourseID != c.ID:
			info := model.ErrorInfo{Code: "rerun.other_course"}
			info.Detail = zhText(info)
			items[id] = CourseRerunItem{LectureID: id, Result: RerunSkipped, Reason: &info}
		default:
			mine = append(mine, id)
		}
	}

	out := CourseRerunResult{Stage: in.Stage, DryRun: in.DryRun, Diff: []ModelDiffView{}}
	var res []sched.RerunResult
	if in.DryRun {
		var diffs []state.ModelDiff
		if len(mine) > 0 {
			if res, diffs, err = s.sc.PlanRerun(ctx, mine, in.Stage); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if len(diffs) > 0 {
			out.Diff = s.needsChoiceView(ctx, &state.NeedsModelChoiceError{Diffs: diffs}, true).Diff
		}
	} else if len(mine) > 0 {
		if res, err = s.sc.RerunLectures(ctx, mine, in.Stage, in.ModelChoice); err != nil {
			s.failChoice(w, r, err)
			return
		}
	}
	for _, x := range res {
		it := CourseRerunItem{LectureID: x.LectureID, Result: RerunQueued}
		switch {
		case x.Block != nil:
			it.Result, it.Reason = RerunSkipped, rerunBlockInfo(x.Block)
		case x.Err != nil:
			_, info := s.batchError(ctx, notFoundAs(x.Err, "lecture.not_found"), "")
			it.Result, it.Reason = RerunFailed, info
		case !in.DryRun:
			if l, err := s.st.GetLecture(ctx, x.LectureID); err == nil {
				if v, err := s.lectureView(ctx, l); err == nil {
					it.Lecture = &v
				}
			}
		}
		items[x.LectureID] = it
	}
	out.Results = make([]CourseRerunItem, 0, len(ids))
	for _, id := range ids {
		it := items[id]
		switch it.Result {
		case RerunQueued:
			out.Queued++
		case RerunSkipped:
			out.Skipped++
		default:
			out.Failed++
		}
		out.Results = append(out.Results, it)
	}
	if !in.DryRun && out.Queued > 0 {
		s.hub.Changed("course", c.ID) // counts of stale and running lectures
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- week overrides ----

// withOverride returns a copy of lecs where lecture id has override o.
func withOverride(lecs []model.Lecture, id string, o *model.WeekOverride) []model.Lecture {
	out := make([]model.Lecture, len(lecs))
	for i, l := range lecs {
		if l.ID == id {
			l.WeekOverride = o
		}
		out[i] = l
	}
	return out
}

// overrideContext reads what a week override change needs: the lecture,
// its course and the course's lectures.
func (s *Server) overrideContext(ctx context.Context, id string) (model.Lecture, model.Course, []model.Lecture, error) {
	l, err := s.getLectureRow(ctx, id)
	if err != nil {
		return l, model.Course{}, nil, err
	}
	c, err := s.courseOr404(ctx, l.CourseID)
	if err != nil {
		return l, c, nil, err
	}
	lecs, err := s.st.ListLectures(ctx, c.ID)
	return l, c, lecs, err
}

// overrideChanged tells everyone (SSE) and requests the materials
// invalidation: the week decides the lecture's week-bound slides.
func (s *Server) overrideChanged(c model.Course, lectureID string) {
	s.materialSetsChanged(c.ID)
	s.hub.Changed("lecture", lectureID)
	s.hub.Changed("course", c.ID) // the other lectures of both weeks may be renumbered
}

// PUT /api/lectures/{id}/week {week, kind, confirm?} → the lecture.
func (s *Server) setLectureWeek(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in WeekOverrideSet
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if !model.ValidWeekOverrideKind(in.Kind) {
		writeError(w, http.StatusBadRequest, "week_override.kind_invalid", map[string]any{"kind": in.Kind})
		return
	}
	if !model.ValidOverrideWeek(in.Week) {
		writeError(w, http.StatusBadRequest, "week_override.week_invalid", map[string]any{"week": in.Week, "max": model.MaxBindWeek})
		return
	}
	l, c, lecs, err := s.overrideContext(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ts, _ := courseTerm(c.TermStart, c.SkipWeeks)
	if ts == nil {
		writeError(w, http.StatusConflict, "week_override.no_term_start", nil)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Kind == model.WeekOverrideSupplement {
		// what the lecture is without any override: only teaching
		// materials hang under a week (the user's request)
		k := calendar.EffectiveKind(l.Kind, l.KindSource, recordedTime(l.RecordedAt), ts, state.KindCatalog(kinds))
		if k.Group != model.KindGroupMaterial {
			writeError(w, http.StatusConflict, "week_override.not_material", map[string]any{"kind": k.ID})
			return
		}
	}
	o := &model.WeekOverride{Week: in.Week, Kind: in.Kind}
	if err := s.checkRenumber(ctx, c, lecs, c, withOverride(lecs, l.ID, o), in.Confirm); err != nil {
		s.failRenumber(w, r, err)
		return
	}
	if err := s.st.SetWeekOverride(ctx, l.ID, *o); err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	s.overrideChanged(c, l.ID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// DELETE /api/lectures/{id}/week[?confirm=1] → the lecture (also when it
// had no override).
func (s *Server) clearLectureWeek(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, c, lecs, err := s.overrideContext(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if l.WeekOverride == nil {
		s.respondLecture(w, r, l.ID, http.StatusOK)
		return
	}
	q := r.URL.Query().Get("confirm")
	confirm := q == "1" || q == "true"
	if err := s.checkRenumber(ctx, c, lecs, c, withOverride(lecs, l.ID, nil), confirm); err != nil {
		s.failRenumber(w, r, err)
		return
	}
	if err := s.st.DeleteWeekOverride(ctx, l.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	s.overrideChanged(c, l.ID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// dropOverrideForKind: the user sets lecture l's kind to kind (source src),
// which contradicts its week override (state.OverrideConflicts); the
// override then goes in the same write (PUT /api/lectures/{id}/kind).
func dropOverrideForKind(l model.Lecture, kind, src string, kinds []model.LectureKind) bool {
	return src == model.KindSourceUser && state.OverrideConflicts(l.WeekOverride, state.KindGroupOf(kinds, kind))
}
