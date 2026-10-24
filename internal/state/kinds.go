// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Lecture kinds and slides bound to a teaching week (lead freeze
// .claude/kinds-brief.md 1.5; K1): the database side of the pure
// calendar functions — numbering a course, the material set of a lecture,
// and the invalidation when material sets change.
//
// Times: like stages and api, recorded_at, term_start and skip_weeks are
// read as wall-clock times in time.Local.

// KindCatalog converts the lecture_kind rows for calendar.LabelKinds.
func KindCatalog(ks []model.LectureKind) []calendar.Kind {
	out := make([]calendar.Kind, len(ks))
	for i, k := range ks {
		out[i] = calendar.Kind{ID: k.ID, Code: k.Code, Group: k.Group}
	}
	return out
}

// LectureTimes converts lectures (any order; results stay parallel) for
// calendar.LabelKinds / LabelAll / Order, with their stored kinds.
func LectureTimes(ls []model.Lecture) []calendar.LectureTime {
	out := make([]calendar.LectureTime, len(ls))
	for i, l := range ls {
		out[i] = calendar.LectureTime{ID: l.ID, RecordedAt: calendar.RecordedTime(l.RecordedAt, time.Local),
			OriginalFilename: l.OriginalFilename, Kind: l.Kind, KindSource: l.KindSource}
	}
	return out
}

// NumberCourse numbers the lectures of course c (calendar.LabelKinds with
// the course's calendar, the kind catalog and c.Numbering). The result is
// parallel to ls; pass every lecture of the course (drafts included) in
// ListLectures order, as P13.4 numbers them.
func NumberCourse(c model.Course, ls []model.Lecture, kinds []model.LectureKind) []calendar.Numbered {
	ts, skips := calendar.CourseTerm(c.TermStart, c.SkipWeeks, time.Local)
	return calendar.LabelKinds(LectureTimes(ls), ts, skips, calendar.NumberOptions{Kinds: KindCatalog(kinds), Numbering: c.Numbering})
}

// LectureWeek is the teaching week of lecture l in course c (0 = cannot be
// computed): what decides which week-bound materials it uses.
func LectureWeek(c model.Course, l model.Lecture) int {
	ts, skips := calendar.CourseTerm(c.TermStart, c.SkipWeeks, time.Local)
	w, ok := calendar.TeachingWeek(calendar.RecordedTime(l.RecordedAt, time.Local), ts, skips)
	if !ok {
		return 0
	}
	return w
}

// MaterialBindings converts materials for calendar.MaterialSet.
func MaterialBindings(ms []model.Material) []calendar.MaterialBinding {
	out := make([]calendar.MaterialBinding, len(ms))
	for i, m := range ms {
		b := calendar.MaterialBinding{ID: m.ID}
		if m.LectureID != nil {
			b.LectureID = *m.LectureID
		}
		if m.BindWeek != nil {
			b.Week = *m.BindWeek
		}
		out[i] = b
	}
	return out
}

// MaterialUse is one material of a lecture's material set and why.
type MaterialUse struct {
	Material model.Material // as listed by the caller (ListMaterialRefs: no PagesMD)
	Via      string         // model.BindLecture | BindWeek | BindCourse
}

// MaterialSetOf is the material set of the lecture with teaching week week
// (0 = none) among ms (calendar.MaterialSet: bound to it, then to its week,
// then course-wide).
func MaterialSetOf(ms []model.Material, lectureID string, week int) []MaterialUse {
	byID := make(map[string]model.Material, len(ms))
	for _, m := range ms {
		byID[m.ID] = m
	}
	uses := calendar.MaterialSet(MaterialBindings(ms), lectureID, week)
	out := make([]MaterialUse, len(uses))
	for i, u := range uses {
		out[i] = MaterialUse{Material: byID[u.ID], Via: u.Via}
	}
	return out
}

// LectureMaterialSet reads the material set of one lecture (materials
// without their pages). Errors: db.ErrNotFound.
func LectureMaterialSet(ctx context.Context, st db.Store, lectureID string) ([]MaterialUse, error) {
	var out []MaterialUse
	err := st.Tx(ctx, func(tx db.Store) error {
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return err
		}
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		ms, err := tx.ListMaterialRefs(ctx, l.CourseID)
		if err != nil {
			return err
		}
		out = MaterialSetOf(ms, l.ID, LectureWeek(c, l))
		return nil
	})
	return out, err
}

// CourseMaterialSets reads the material set of every lecture of a course
// (drafts included; materials without their pages), keyed by lecture id.
func CourseMaterialSets(ctx context.Context, st db.Store, courseID string) (map[string][]MaterialUse, error) {
	var out map[string][]MaterialUse
	err := st.Tx(ctx, func(tx db.Store) error {
		var err error
		out, _, err = courseSets(ctx, tx, courseID)
		return err
	})
	return out, err
}

// courseSets is CourseMaterialSets inside a transaction; it also returns the
// lectures (ListLectures order).
func courseSets(ctx context.Context, tx db.Store, courseID string) (map[string][]MaterialUse, []model.Lecture, error) {
	c, err := tx.GetCourse(ctx, courseID)
	if err != nil {
		return nil, nil, err
	}
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return nil, nil, err
	}
	ms, err := tx.ListMaterialRefs(ctx, courseID)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string][]MaterialUse, len(ls))
	for _, l := range ls {
		out[l.ID] = MaterialSetOf(ms, l.ID, LectureWeek(c, l))
	}
	return out, ls, nil
}

// lectureMaterials returns the full materials (with pages) of the lecture's
// material set, in upload order (created_at, id), for LoadSnapshot.
func lectureMaterials(ctx context.Context, st db.Store, lec model.Lecture) ([]model.Material, error) {
	c, err := st.GetCourse(ctx, lec.CourseID)
	if err != nil {
		return nil, err
	}
	all, err := st.ListMaterials(ctx, lec.CourseID) // ordered by created_at, id
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for _, u := range calendar.MaterialSet(MaterialBindings(all), lec.ID, LectureWeek(c, lec)) {
		in[u.ID] = true
	}
	out := []model.Material{}
	for _, m := range all {
		if in[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

// ---------- invalidation when material sets change ----------

// StaleMaterials is the cause of InvalidateCourseWith after anything that
// may change which materials a lecture uses: the course's term_start or
// skip_weeks, a lecture's recorded_at, a material's binding, a material
// added or deleted (lead freeze 1.5). Callers pass StaleCause{Kind:
// StaleMaterials} and nothing else; the run compares each lecture's
// material set with the one last seen (kv material_sets:{course}) and
// checks only the lectures whose set changed, with stale_reason
// materials_changed {added, removed} (file names). It affects the stages
// the slides feed, like a material change (summary, translate); a stage
// whose input hash did not change stays done.
const StaleMaterials = "materials"

// materialSeen is one material of a lecture's set in the kv baseline.
type materialSeen struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

// materialBaseline is kv material_sets:{course_id}: lecture id → its
// material set as last seen by the materials invalidation.
type materialBaseline struct {
	Lectures map[string][]materialSeen `json:"lectures"`
}

// courseLocks serialises the materials invalidation per course (its
// read-compare-write of the kv baseline).
var courseLocks sync.Map // course id → *sync.Mutex

func courseLock(courseID string) *sync.Mutex {
	m, _ := courseLocks.LoadOrStore(courseID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func loadBaseline(ctx context.Context, st db.Store, courseID string) (materialBaseline, bool, error) {
	raw, ok, err := st.GetKV(ctx, model.KVMaterialSetsPrefix+courseID)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return materialBaseline{}, false, err
	}
	var b materialBaseline
	if err := json.Unmarshal([]byte(raw), &b); err != nil || b.Lectures == nil {
		slog.Warn("material set baseline unreadable; checking every lecture", "course", courseID, "err", err)
		return materialBaseline{}, false, nil
	}
	return b, true, nil
}

func seenOf(uses []MaterialUse) []materialSeen {
	out := make([]materialSeen, len(uses))
	for i, u := range uses {
		out[i] = materialSeen{ID: u.Material.ID, Filename: u.Material.Filename}
	}
	return out
}

// seenDiff compares two sets by material id: the file names added and
// removed (nil when none), in set order.
func seenDiff(before, after []materialSeen) (added, removed []string) {
	ids := func(s []materialSeen) map[string]bool {
		m := make(map[string]bool, len(s))
		for _, x := range s {
			m[x.ID] = true
		}
		return m
	}
	b, a := ids(before), ids(after)
	for _, x := range after {
		if !b[x.ID] {
			added = append(added, x.Filename)
		}
	}
	for _, x := range before {
		if !a[x.ID] {
			removed = append(removed, x.Filename)
		}
	}
	return added, removed
}

// invalidateMaterialSets is InvalidateCourseWith for StaleMaterials. For
// every lecture of the course: unchanged set → nothing; changed set → the
// invalidation check of the lecture with materials_changed {added,
// removed}; no baseline for it (a course or lecture never seen, or an
// unreadable baseline) → the check with materials_changed without params
// (only stages whose input hash changed become stale, so an unknown
// previous set never marks a stage needlessly). The baseline is then
// updated, except for lectures whose check could not hash a stage (they
// are compared again next time). Lectures deleted since are dropped.
func invalidateMaterialSets(ctx context.Context, st db.Store, hashers map[string]Hasher, courseID string) ([]model.Job, error) {
	mu := courseLock(courseID)
	mu.Lock()
	defer mu.Unlock()

	var (
		sets map[string][]MaterialUse
		ls   []model.Lecture
	)
	err := st.Tx(ctx, func(tx db.Store) error {
		var err error
		sets, ls, err = courseSets(ctx, tx, courseID)
		return err
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil // the course was deleted meanwhile
		}
		return nil, err
	}
	base, haveBase, err := loadBaseline(ctx, st, courseID)
	if err != nil {
		return nil, err
	}
	next := materialBaseline{Lectures: map[string][]materialSeen{}}
	var changed []model.Job
	for _, l := range ls {
		now := seenOf(sets[l.ID])
		before, known := base.Lectures[l.ID]
		known = known && haveBase
		var added, removed []string
		if known {
			added, removed = seenDiff(before, now)
			if added == nil && removed == nil {
				next.Lectures[l.ID] = now
				continue
			}
		}
		reason := reasonFunc(func(j model.Job) string { return materialsReason(j, added, removed) })
		c, hashFailed, err := checkScope(ctx, st, hashers, model.ScopeLecture, l.ID, reason, nil)
		changed = append(changed, c...)
		if err != nil {
			return changed, err
		}
		switch {
		case !hashFailed:
			next.Lectures[l.ID] = now
		case known:
			next.Lectures[l.ID] = before
		}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return changed, err
	}
	return changed, st.SetKV(ctx, model.KVMaterialSetsPrefix+courseID, string(b))
}

// materialsReason is the stale_reason a job j gets when its lecture's
// material set changed: materials_changed {added, removed} when the cause
// feeds j's stage, input_changed otherwise (like StaleReason).
func materialsReason(j model.Job, added, removed []string) string {
	if !causeCovers(StaleMaterials, j.Stage) {
		return reasonInput()
	}
	return model.StaleReasonJSON(model.StaleCodeMaterials, materialsParams(added, removed))
}

// materialsParams are the params of materials_changed: added and removed
// file names, each absent when empty.
func materialsParams(added, removed []string) map[string]any {
	p := map[string]any{}
	if len(added) > 0 {
		p["added"] = append([]string(nil), added...)
	}
	if len(removed) > 0 {
		p["removed"] = append([]string(nil), removed...)
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

// stringsParam reads a []string param ([]string as written, []any from JSON).
func stringsParam(p map[string]any, key string) []string {
	switch v := p[key].(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// materialsReasonText is the transitional Chinese text of materials_changed
// (StaleReasonText): "适用的课件已变化（新增：a.pdf、b.pdf；移除：c.pdf）".
func materialsReasonText(p map[string]any) string {
	var parts []string
	if a := stringsParam(p, "added"); len(a) > 0 {
		parts = append(parts, "新增："+strings.Join(a, "、"))
	}
	if r := stringsParam(p, "removed"); len(r) > 0 {
		parts = append(parts, "移除："+strings.Join(r, "、"))
	}
	if len(parts) == 0 {
		return materialsReasonPrefix
	}
	return materialsReasonPrefix + "（" + strings.Join(parts, "；") + "）"
}

const materialsReasonPrefix = "适用的课件已变化"
