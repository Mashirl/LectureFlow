// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// satisfied: an upstream job in done or stale has its outputs in place.
// stale only means "inputs changed since" (SPEC §4.4) and is never rerun
// automatically, so it does not block downstream (see NOTES.md).
func satisfied(status string) bool {
	return status == model.StatusDone || status == model.StatusStale
}

// Deps reports whether job's dependencies are met (SPEC §4.1, §4.3) and, if
// not, the waiting_for value (SPEC §9.5):
//   - every lecture stage of a draft (launched_at IS NULL, v1.1 A3.1) →
//     "not_started", checked before anything else, so the scheduler never
//     moves a draft out of pending;
//   - lecture stages: the immediately preceding stage → "upstream:{stage}";
//   - asr additionally: glossary_seed when the course has a syllabus →
//     "glossary_seed"; every material bound to this lecture or to its
//     teaching week (lecture kinds, lead freeze 1.5: the lecture's material
//     set without the course-wide part) must have material_terms done →
//     "material_terms:{filename}" (course-wide materials do not block; a
//     lecture without a teaching week waits for none bound to a week);
//   - asr, last (v2 G3.1, lead ruling 3): the course's glossary_review
//     while it is pending, queued or running → "glossary_review";
//   - material_terms: material_extract → "upstream:material_extract";
//   - glossary_review (not in the course chain, lead ruling 2): see
//     reviewDeps ("upstream:glossary_seed", "material_terms:{filename}");
//   - auxiliary tasks (v2 phase 2 ruling 1): see auxDeps (title, chapters
//     → "upstream:summary", stream → "upstream:prep"; filename_rule none);
//   - prep, material_extract, glossary_seed: no dependencies.
func Deps(ctx context.Context, st db.Store, job model.Job) (bool, string, error) {
	if job.Stage == model.StageGlossaryReview {
		return reviewDeps(ctx, st, job.ScopeID)
	}
	if model.IsAuxStage(job.Stage) {
		return auxDeps(ctx, st, job)
	}
	var lec model.Lecture
	if job.Scope == model.ScopeLecture {
		var err error
		if lec, err = st.GetLecture(ctx, job.ScopeID); err != nil {
			return false, "", err
		}
		if IsDraft(lec) {
			return false, model.WaitNotStarted, nil
		}
	}
	if up := Upstream(job.Stage); up != "" {
		u, err := st.GetCurrentJob(ctx, job.Scope, job.ScopeID, up)
		if errors.Is(err, db.ErrNotFound) {
			return false, model.WaitUpstream(up), nil
		}
		if err != nil {
			return false, "", err
		}
		if !satisfied(u.Status) {
			return false, model.WaitUpstream(up), nil
		}
	}
	if job.Stage != model.StageASR {
		return true, "", nil
	}
	_, err := st.GetSyllabus(ctx, lec.CourseID)
	switch {
	case err == nil:
		g, err := st.GetCurrentJob(ctx, model.ScopeCourse, lec.CourseID, model.StageGlossarySeed)
		if errors.Is(err, db.ErrNotFound) {
			return false, model.WaitGlossarySeed, nil
		}
		if err != nil {
			return false, "", err
		}
		if !satisfied(g.Status) {
			return false, model.WaitGlossarySeed, nil
		}
	case !errors.Is(err, db.ErrNotFound):
		return false, "", err
	}
	c, err := st.GetCourse(ctx, lec.CourseID)
	if err != nil {
		return false, "", err
	}
	mats, err := st.ListMaterialRefs(ctx, lec.CourseID)
	if err != nil {
		return false, "", err
	}
	for _, u := range MaterialSetOf(mats, lec.ID, LectureWeek(c, lec)) {
		if u.Via == model.BindCourse {
			continue // course-wide materials do not block
		}
		m := u.Material
		mt, err := st.GetCurrentJob(ctx, model.ScopeMaterial, m.ID, model.StageMaterialTerms)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return false, "", err
		}
		if err != nil || !satisfied(mt.Status) {
			return false, model.WaitMaterialTerms(m.Filename), nil
		}
	}
	held, err := reviewHoldsASR(ctx, st, lec.CourseID)
	if err != nil {
		return false, "", err
	}
	if held {
		return false, model.WaitGlossaryReview, nil
	}
	return true, "", nil
}
