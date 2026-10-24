// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// Tests of lecture kinds and week-bound slides in state (lead freeze
// .claude/kinds-brief.md 1.5, K1): numbering a course from the database,
// material sets, the asr wait, the snapshot's materials and the
// invalidation when material sets change.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// setTerm sets the course's term start (and skipped weeks).
func (f *fixture) setTerm(termStart string, skip ...string) {
	f.t.Helper()
	if err := f.st.SetCourseCalendar(f.ctx, f.course.ID, &termStart, skip); err != nil {
		f.t.Fatal(err)
	}
}

// recorded sets a lecture's recorded_at (local wall clock).
func (f *fixture) recorded(lectureID, at string) {
	f.t.Helper()
	if err := f.st.SetLectureRecordedAt(f.ctx, lectureID, &at, model.RecordedAtFilename); err != nil {
		f.t.Fatal(err)
	}
}

// material creates an extracted material (bound to a lecture, a week, or
// neither) with its material_terms job in the given status path.
func (f *fixture) boundMaterial(name string, lectureID *string, week *int, termsPath ...string) model.Material {
	f.t.Helper()
	m := model.Material{ID: db.NewID(), CourseID: f.course.ID, LectureID: lectureID, BindWeek: week, Filename: name, Path: "p",
		PagesMD: []string{"slides of " + name}}
	if err := f.st.CreateMaterial(f.ctx, m); err != nil {
		f.t.Fatal(err)
	}
	if len(termsPath) > 0 {
		f.drive(model.ScopeMaterial, m.ID, model.StageMaterialTerms, termsPath...)
	}
	return m
}

// redo reruns summary (and so everything after it) and finishes it again
// with fresh hashes, as if the stages had run with the current inputs.
func (f *fixture) redo(hashers map[string]Hasher, ls ...model.Lecture) {
	f.t.Helper()
	for _, l := range ls {
		if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, l.ID, model.StageSummary); err != nil {
			f.t.Fatal(err)
		}
		for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
			f.done(model.ScopeLecture, l.ID, s)
		}
		f.recordHashes(hashers, l.ID)
	}
}

func useIDs(us []MaterialUse) string {
	var s []string
	for _, u := range us {
		s = append(s, u.Material.Filename+":"+u.Via)
	}
	return strings.Join(s, " ")
}

func TestNumberCourseFromStore(t *testing.T) {
	f := newFixture(t)
	f.setTerm("2026-09-28")
	lab := "实验课"
	labKind, err := f.st.InsertLectureKind(f.ctx, model.LectureKind{Code: "LAB", Name: &lab, Group: model.KindGroupSession})
	if err != nil {
		t.Fatal(err)
	}
	l1, t1, t2, x, p := f.lecture(), f.lecture(), f.lecture(), f.lecture(), f.draft()
	f.recorded(l1.ID, "2026-10-06T13:35")
	f.recorded(t1.ID, "2026-10-09T12:35")
	f.recorded(t2.ID, "2026-10-09T09:35")
	f.recorded(x.ID, "2026-10-08T10:00")
	f.recorded(p.ID, "2025-10-07T13:35")
	f.st.SetLectureKind(f.ctx, t1.ID, model.KindTutorial, model.KindSourceFilename)
	f.st.SetLectureKind(f.ctx, t2.ID, model.KindTutorial, model.KindSourceFilename)
	f.st.SetLectureKind(f.ctx, x.ID, labKind.ID, model.KindSourceUser)
	f.st.SetCourseNumbering(f.ctx, f.course.ID, model.NumberingAlways)

	c, _ := f.st.GetCourse(f.ctx, f.course.ID)
	ls, _ := f.st.ListLectures(f.ctx, f.course.ID)
	kinds, _ := f.st.ListLectureKinds(f.ctx)
	nums := NumberCourse(c, ls, kinds)
	want := map[string]string{l1.ID: "W02L1", t2.ID: "W02T1", t1.ID: "W02T2", x.ID: "W02LAB1", p.ID: "P1"}
	for i, n := range nums {
		if n.ID != ls[i].ID || n.Label != want[n.ID] {
			t.Errorf("%s: %s, want %s", n.ID, n.Label, want[n.ID])
		}
		if !n.ShowN {
			t.Errorf("%s: numbering always must show n", n.ID)
		}
	}
	if got := LectureWeek(c, l1); got != 0 { // l1 as created: no recorded_at yet
		t.Errorf("LectureWeek of the stale row = %d", got)
	}
	l1, _ = f.st.GetLecture(f.ctx, l1.ID)
	if got := LectureWeek(c, l1); got != 2 {
		t.Errorf("LectureWeek = %d, want 2", got)
	}
	if lt := LectureTimes(ls); lt[1].Kind != model.KindTutorial || lt[1].KindSource != model.KindSourceFilename || lt[1].RecordedAt == nil {
		t.Errorf("LectureTimes %+v", lt[1])
	}
}

func TestMaterialSets(t *testing.T) {
	f := newFixture(t)
	f.setTerm("2026-09-28", "2026-10-19")
	l2, l3, l3b, none, skipped := f.lecture(), f.lecture(), f.lecture(), f.lecture(), f.lecture()
	f.recorded(l2.ID, "2026-10-06T09:35")
	f.recorded(l3.ID, "2026-10-13T09:35")
	f.recorded(l3b.ID, "2026-10-16T09:35")
	f.recorded(skipped.ID, "2026-10-21T09:35")
	all := f.boundMaterial("all.pdf", nil, nil)
	w3 := f.boundMaterial("Week3.pdf", nil, ip(3))
	own := f.boundMaterial("own.pdf", &l3.ID, nil)
	f.boundMaterial("Week2.pdf", nil, ip(2))

	sets, err := CourseMaterialSets(f.ctx, f.st, f.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		l2.ID:      "Week2.pdf:week all.pdf:course",
		l3.ID:      "own.pdf:lecture Week3.pdf:week all.pdf:course",
		l3b.ID:     "Week3.pdf:week all.pdf:course",
		none.ID:    "all.pdf:course", // no recorded_at: no week
		skipped.ID: "all.pdf:course", // in the skipped week
	}
	for id, w := range want {
		if got := useIDs(sets[id]); got != w {
			t.Errorf("set of %s = %s, want %s", id, got, w)
		}
		one, err := LectureMaterialSet(f.ctx, f.st, id)
		if err != nil || useIDs(one) != w {
			t.Errorf("LectureMaterialSet(%s) = %s, %v", id, useIDs(one), err)
		}
	}
	if sets[l3.ID][0].Material.ID != own.ID || sets[l3.ID][0].Material.PagesMD != nil {
		t.Errorf("set entries are the materials without pages: %+v", sets[l3.ID][0])
	}
	if _, err := LectureMaterialSet(f.ctx, f.st, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown lecture: %v", err)
	}

	// The snapshot of the prompts and hashes holds exactly the set, with pages.
	snap, err := LoadSnapshot(f.ctx, f.st, l3.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range snap.Materials {
		names = append(names, m.Filename)
		if m.PagesMD == nil {
			t.Errorf("%s without pages", m.Filename)
		}
	}
	if got := strings.Join(names, " "); got != "all.pdf Week3.pdf own.pdf" { // upload order
		t.Errorf("snapshot materials = %s", got)
	}
	if s := ctxbuild.LectureSlides(snap); !strings.Contains(s, "slides of Week3.pdf") || strings.Contains(s, "Week2.pdf") {
		t.Errorf("slides text = %q", s)
	}
	snap2, _ := LoadSnapshot(f.ctx, f.st, l2.ID)
	if s := ctxbuild.LectureSlides(snap2); strings.Contains(s, "Week3.pdf") || strings.Contains(s, "own.pdf") || !strings.Contains(s, "Week2.pdf") {
		t.Errorf("l2 slides text = %q", s)
	}
	_ = all
	_ = w3
}

func TestASRWaitsForWeekMaterials(t *testing.T) {
	f := newFixture(t)
	f.setTerm("2026-09-28")
	l := f.lecture()
	other := f.lecture()
	undated := f.lecture()
	f.recorded(l.ID, "2026-10-13T09:35")     // week 3
	f.recorded(other.ID, "2026-10-06T09:35") // week 2
	for _, x := range []model.Lecture{l, other, undated} {
		f.done(model.ScopeLecture, x.ID, model.StagePrep)
	}
	asrOf := func(id string) model.Job { return f.lj(id, model.StageASR) }

	f.boundMaterial("all.pdf", nil, nil)                                                    // course-wide: never blocks
	f.boundMaterial("Week2.pdf", nil, ip(2), model.StatusQueued, model.StatusRunning)       // another week
	w3 := f.boundMaterial("Week3.pdf", nil, ip(3), model.StatusQueued, model.StatusRunning) // this lecture's week
	if ok, w := f.deps(asrOf(l.ID)); ok || w != model.WaitMaterialTerms("Week3.pdf") {
		t.Fatalf("week 3 lecture: ok=%v waiting=%q", ok, w)
	}
	if ok, w := f.deps(asrOf(other.ID)); ok || w != model.WaitMaterialTerms("Week2.pdf") {
		t.Fatalf("week 2 lecture: ok=%v waiting=%q", ok, w)
	}
	if ok, w := f.deps(asrOf(undated.ID)); !ok || w != "" {
		t.Fatalf("a lecture without a week waits for no week material: ok=%v waiting=%q", ok, w)
	}
	f.drive(model.ScopeMaterial, w3.ID, model.StageMaterialTerms, model.StatusDone)
	if ok, w := f.deps(asrOf(l.ID)); !ok || w != "" {
		t.Fatalf("after material_terms done: ok=%v waiting=%q", ok, w)
	}
	// Bound to the lecture: still waits (v1 rule), and stale is satisfied.
	own := f.boundMaterial("own.pdf", &l.ID, nil, model.StatusQueued)
	if ok, w := f.deps(asrOf(l.ID)); ok || w != model.WaitMaterialTerms("own.pdf") {
		t.Fatalf("own material: ok=%v waiting=%q", ok, w)
	}
	f.drive(model.ScopeMaterial, own.ID, model.StageMaterialTerms, model.StatusRunning, model.StatusDone, model.StatusStale)
	if ok, _ := f.deps(asrOf(l.ID)); !ok {
		t.Fatal("stale material_terms has its output in place")
	}
	// Moving the lecture into week 2 makes it wait for that week instead.
	f.recorded(l.ID, "2026-10-07T09:35")
	if ok, w := f.deps(asrOf(l.ID)); ok || w != model.WaitMaterialTerms("Week2.pdf") {
		t.Fatalf("moved to week 2: ok=%v waiting=%q", ok, w)
	}
}

// slidesHashers hash what the slides feed: summary and translate hash the
// lecture's slides text (LoadSnapshot, so the material set); repair hashes
// nothing that materials change. calls counts the hasher calls.
func slidesHashers(st db.Store, calls *atomic.Int64, fail *atomic.Bool) map[string]Hasher {
	sum := func(parts ...string) string {
		h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return hex.EncodeToString(h[:])
	}
	slides := func(stage string) Hasher {
		return HasherFunc(func(ctx context.Context, id string) (string, error) {
			calls.Add(1)
			if fail != nil && fail.Load() {
				return "", errors.New("hasher down")
			}
			snap, err := LoadSnapshot(ctx, st, id)
			if err != nil {
				return "", err
			}
			return sum(stage, id, ctxbuild.LectureSlides(snap)), nil
		})
	}
	return map[string]Hasher{
		model.StageSummary:   slides(model.StageSummary),
		model.StageTranslate: slides(model.StageTranslate),
		model.StageRepair: HasherFunc(func(ctx context.Context, id string) (string, error) {
			calls.Add(1)
			return sum("repair", id), nil
		}),
	}
}

func rMaterials(added, removed []string) string {
	return model.StaleReasonJSON(model.StaleCodeMaterials, materialsParams(added, removed))
}

func TestInvalidateMaterialSets(t *testing.T) {
	f := newFixture(t)
	f.setTerm("2026-09-28")
	var calls atomic.Int64
	var fail atomic.Bool
	hashers := slidesHashers(f.st, &calls, &fail)
	l1, l2, l3 := f.lecture(), f.lecture(), f.lecture()
	f.recorded(l1.ID, "2026-10-06T09:35") // week 2
	f.recorded(l2.ID, "2026-10-13T09:35") // week 3
	f.recorded(l3.ID, "2026-10-20T09:35") // week 4
	f.boundMaterial("all.pdf", nil, nil)
	w2 := f.boundMaterial("Week2.pdf", nil, ip(2))
	w3 := f.boundMaterial("Week3.pdf", nil, ip(3))
	for _, l := range []model.Lecture{l1, l2, l3} {
		f.allDone(l.ID)
		f.recordHashes(hashers, l.ID)
	}
	run := func() []model.Job {
		t.Helper()
		changed, err := InvalidateCourseWith(f.ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleMaterials})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range changed {
			if j.Status != model.StatusStale || j.StaleReason == nil {
				t.Fatalf("returned job must carry status and reason: %+v", j)
			}
		}
		return changed
	}
	stateOf := func(l model.Lecture) string {
		var s []string
		for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
			j := f.lj(l.ID, st)
			s = append(s, st+"="+j.Status+" "+reasonOf(j))
		}
		return strings.Join(s, "; ")
	}

	// No baseline yet: every lecture is checked, nothing changed.
	calls.Store(0)
	if c := run(); len(c) != 0 {
		t.Fatalf("first run: %+v", c)
	}
	if calls.Load() == 0 {
		t.Fatal("the first run (no baseline) must check the lectures")
	}
	// Nothing changed since: nothing is checked at all.
	calls.Store(0)
	if c := run(); len(c) != 0 || calls.Load() != 0 {
		t.Fatalf("unchanged sets: %+v, %d hasher calls", c, calls.Load())
	}

	// The term start moves one week later: l1 (week 1) loses Week2.pdf,
	// l2 (week 2) gains Week2.pdf and loses Week3.pdf, l3 (week 3) gains
	// Week3.pdf. Only summary and translate become stale; repair stays.
	f.setTerm("2026-10-05")
	if c := run(); len(c) != 6 {
		t.Fatalf("term start moved: %d changed: %+v", len(c), c)
	}
	for l, r := range map[model.Lecture]string{
		l1: rMaterials(nil, []string{"Week2.pdf"}),
		l2: rMaterials([]string{"Week2.pdf"}, []string{"Week3.pdf"}),
		l3: rMaterials([]string{"Week3.pdf"}, nil),
	} {
		want := "summary=stale " + r + "; repair=done <nil>; translate=stale " + r
		if got := stateOf(l); got != want {
			t.Errorf("%s:\n got %s\nwant %s", l.ID, got, want)
		}
	}
	if got := StaleText(mustJobs(t, f, l2.ID)); got != "适用的课件已变化（新增：Week2.pdf；移除：Week3.pdf），需要更新：摘要、翻译" {
		t.Errorf("StaleText = %q", got)
	}

	// Rerun l1 (fresh hashes), then bind Week3.pdf to l1 itself and delete
	// Week2.pdf: l1 gains Week3.pdf (bound to it), l2 loses Week2.pdf, l3
	// loses Week3.pdf (it was bound to its week).
	f.redo(hashers, l1, l2, l3)
	if err := f.st.SetMaterialLecture(f.ctx, w3.ID, &l1.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteMaterial(f.ctx, w2.ID); err != nil {
		t.Fatal(err)
	}
	run()
	for l, r := range map[model.Lecture]string{
		l1: rMaterials([]string{"Week3.pdf"}, nil),
		l2: rMaterials(nil, []string{"Week2.pdf"}), // the deleted material's name comes from the baseline
		l3: rMaterials(nil, []string{"Week3.pdf"}),
	} {
		if j := f.lj(l.ID, model.StageSummary); j.Status != model.StatusStale || reasonOf(j) != r {
			t.Errorf("%s summary: %s %s, want %s", l.ID, j.Status, reasonOf(j), r)
		}
	}

	// A set change whose material has no pages yet changes no hash: nothing
	// becomes stale, and the next run does not report it again.
	f.redo(hashers, l1, l2, l3)
	raw := model.Material{ID: db.NewID(), CourseID: f.course.ID, Filename: "Week4.pdf", Path: "p", BindWeek: ip(4)}
	f.st.CreateMaterial(f.ctx, raw) // l3 is in week 3 now; week 4 has no lecture
	f.st.SetMaterialBinding(f.ctx, raw.ID, nil, ip(3))
	if c := run(); len(c) != 0 {
		t.Fatalf("an unextracted material: %+v", c)
	}
	calls.Store(0)
	if run(); calls.Load() != 0 {
		t.Fatalf("baseline not updated: %d hasher calls", calls.Load())
	}
	// Once extracted, the material path (material_changed) reports it.
	f.st.SetMaterialPages(f.ctx, raw.ID, []string{"week 4 pages"})
	if c, _ := InvalidateCourseWith(f.ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleMaterial, Material: "Week4.pdf"}); len(c) != 2 {
		t.Fatalf("extraction: %+v", c)
	}

	// A hasher failure keeps the old baseline: the lecture is compared again.
	f.redo(hashers, l1, l2, l3)
	f.st.SetMaterialBinding(f.ctx, raw.ID, nil, ip(2)) // l2 (week 2) gains it, l3 loses it
	fail.Store(true)
	if c := run(); len(c) != 0 {
		t.Fatalf("hashers down: %+v", c)
	}
	fail.Store(false)
	run()
	if j := f.lj(l2.ID, model.StageSummary); j.Status != model.StatusStale || reasonOf(j) != rMaterials([]string{"Week4.pdf"}, nil) {
		t.Errorf("after the hashers recovered, l2 summary: %s %s", j.Status, reasonOf(j))
	}
	if j := f.lj(l3.ID, model.StageTranslate); j.Status != model.StatusStale || reasonOf(j) != rMaterials(nil, []string{"Week4.pdf"}) {
		t.Errorf("after the hashers recovered, l3 translate: %s %s", j.Status, reasonOf(j))
	}

	// The baseline drops deleted lectures and is stored as JSON.
	if err := f.st.DeleteLecture(f.ctx, l3.ID); err != nil {
		t.Fatal(err)
	}
	run()
	base, ok, err := loadBaseline(f.ctx, f.st, f.course.ID)
	if err != nil || !ok || len(base.Lectures) != 2 || base.Lectures[l3.ID] != nil {
		t.Fatalf("baseline %+v ok=%v err=%v", base, ok, err)
	}
}

// A new lecture (no baseline entry) is checked once with materials_changed
// without params; an unreadable baseline is treated as none.
func TestInvalidateMaterialSetsUnknownBaseline(t *testing.T) {
	f := newFixture(t)
	f.setTerm("2026-09-28")
	var calls atomic.Int64
	hashers := slidesHashers(f.st, &calls, nil)
	l := f.lecture()
	f.recorded(l.ID, "2026-10-06T09:35")
	f.allDone(l.ID)
	f.recordHashes(hashers, l.ID)
	if err := f.st.SetKV(f.ctx, model.KVMaterialSetsPrefix+f.course.ID, "not json"); err != nil {
		t.Fatal(err)
	}
	f.boundMaterial("Week2.pdf", nil, ip(2))
	changed, err := InvalidateCourseWith(f.ctx, f.st, hashers, f.course.ID, StaleCause{Kind: StaleMaterials})
	if err != nil || len(changed) != 2 {
		t.Fatalf("changed = %+v, %v", changed, err)
	}
	if j := f.lj(l.ID, model.StageSummary); reasonOf(j) != model.StaleReasonJSON(model.StaleCodeMaterials, nil) {
		t.Errorf("reason without a previous set = %s", reasonOf(j))
	}
	if j := f.lj(l.ID, model.StageRepair); j.Status != model.StatusDone {
		t.Errorf("repair = %s", j.Status)
	}
	// A deleted course is not an error.
	if c, err := InvalidateCourseWith(f.ctx, f.st, hashers, "gone", StaleCause{Kind: StaleMaterials}); err != nil || len(c) != 0 {
		t.Errorf("unknown course: %v %v", c, err)
	}
}

func TestMaterialsReasonFormats(t *testing.T) {
	j := model.Job{Stage: model.StageSummary}
	if got := StaleReason(StaleCause{Kind: StaleMaterials}, j, 0); got != `{"code":"materials_changed"}` {
		t.Errorf("StaleReason = %s", got)
	}
	if got := StaleReason(StaleCause{Kind: StaleMaterials}, model.Job{Stage: model.StageRepair}, 0); got != rInput() {
		t.Errorf("repair = %s (materials do not feed repair)", got)
	}
	if got := materialsReason(j, []string{"a.pdf"}, []string{"b.pdf", "c.pdf"}); got != `{"code":"materials_changed","params":{"added":["a.pdf"],"removed":["b.pdf","c.pdf"]}}` {
		t.Errorf("materialsReason = %s", got)
	}
	cases := map[string]string{
		model.StaleReasonJSON(model.StaleCodeMaterials, nil):                                 "适用的课件已变化",
		rMaterials([]string{"a.pdf", "b.pdf"}, nil):                                          "适用的课件已变化（新增：a.pdf、b.pdf）",
		rMaterials(nil, []string{"c.pdf"}):                                                   "适用的课件已变化（移除：c.pdf）",
		rMaterials([]string{"a.pdf"}, []string{"c.pdf"}):                                     "适用的课件已变化（新增：a.pdf；移除：c.pdf）",
		`{"code":"materials_changed","params":{"added":["x.pdf",3],"removed":"not a list"}}`: "适用的课件已变化（新增：x.pdf）",
	}
	for raw, want := range cases {
		if got := StaleReasonText(raw); got != want {
			t.Errorf("StaleReasonText(%s) = %q, want %q", raw, got, want)
		}
	}
	// The JSON round trip keeps the lists (as []any).
	r := StaleReasonOf(model.Job{StaleReason: sp(rMaterials([]string{"a.pdf"}, nil))})
	if r.Code != model.StaleCodeMaterials || !reflect.DeepEqual(stringsParam(r.Params, "added"), []string{"a.pdf"}) {
		t.Errorf("StaleReasonOf = %+v", r)
	}
	// CheckStaleWith with the materials cause writes the reason without params.
	f := newFixture(t)
	var calls atomic.Int64
	hashers := slidesHashers(f.st, &calls, nil)
	l := f.lecture()
	f.allDone(l.ID)
	f.recordHashes(hashers, l.ID)
	f.boundMaterial("all.pdf", nil, nil)
	if c, err := CheckStaleWith(f.ctx, f.st, hashers, l.ID, StaleCause{Kind: StaleMaterials}); err != nil || len(c) != 2 ||
		reasonOf(c[0]) != model.StaleReasonJSON(model.StaleCodeMaterials, nil) {
		t.Fatalf("CheckStaleWith = %+v, %v", c, err)
	}
}
