// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// Tests of migration 007 and the lecture-kind store methods (lead freeze
// .claude/kinds-brief.md section 2, K1).

func ip(n int) *int { return &n }

// dumpTables renders every row of the given tables (columns as of
// migration 006, in a fixed order) for before/after comparisons.
func dumpTables(t *testing.T, d *sql.DB, queries map[string]string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"course", "lecture", "material", "job", "sentence", "term", "kv"} {
		q, ok := queries[name]
		if !ok {
			continue
		}
		rows, err := d.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s:", name)
			for i, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, " %s=%v", cols[i], v)
			}
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

// v6Queries select the columns that exist before migration 007.
var v6Queries = map[string]string{
	"course":   `SELECT id, name, glossary_version, created_at, code, term_start, skip_weeks, filename_rule FROM course ORDER BY id`,
	"lecture":  `SELECT id, course_id, title, media_path, created_at, launched_at, seq, recorded_at, recorded_at_source, title_zh, title_source, blurb_en, blurb_zh, original_filename FROM lecture ORDER BY id`,
	"material": `SELECT id, course_id, lecture_id, filename, path, pages_md, created_at FROM material ORDER BY id`,
	"job":      `SELECT id, scope, scope_id, stage, status, input_hash, attempt, error, stats, is_current, config_snapshot, stale_reason FROM job ORDER BY id`,
	"sentence": `SELECT lecture_id, idx, asr_text, text, zh, zh_status, words FROM sentence ORDER BY lecture_id, idx`,
	"term":     `SELECT id, en, zh, tier FROM term ORDER BY id`,
	"kv":       `SELECT key, value FROM kv ORDER BY key`,
}

// An existing database (migration 006) upgrades to 007 without any change
// to its data; the new columns get their defaults and the five built-in
// kinds exist.
func TestMigration007Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, d, 6); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 6 {
		t.Fatalf("user_version = %d, want 6", v)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Rows written the way the v2 binary wrote them.
	exec(`INSERT INTO course (id, name, glossary_version, created_at, code, term_start, skip_weeks, filename_rule)
		VALUES ('C1','Crypto',3,'2026-09-20T10:00:00Z','CSC1003','2026-09-28','["2026-10-19"]','{"regex":"x"}')`)
	exec(`INSERT INTO course (id, name, glossary_version, created_at) VALUES ('C2','Empty',0,'2026-09-21T10:00:00Z')`)
	exec(`INSERT INTO lecture (id, course_id, title, media_path, created_at, launched_at, seq, recorded_at, recorded_at_source,
		title_zh, title_source, blurb_en, blurb_zh, original_filename)
		VALUES ('L1','C1','Stream ciphers','m.mp4','2026-10-01T10:00:00Z','2026-10-01T10:00:00Z',1,'2026-09-30T09:35','filename',
		'流密码','llm','b','简介','CSC1003-L01-01 Wed Sep 30 09-35-00 BST 2026.mp4')`)
	exec(`INSERT INTO lecture (id, course_id, title, media_path, created_at, seq) VALUES ('L2','C1','Draft','d.mp4','2026-10-02T10:00:00Z',2)`)
	exec(`INSERT INTO material (id, course_id, lecture_id, filename, path, pages_md, created_at)
		VALUES ('M1','C1','L1','w1.pdf','p1','["a","b"]','2026-10-01T11:00:00Z')`)
	exec(`INSERT INTO material (id, course_id, lecture_id, filename, path, pages_md, created_at)
		VALUES ('M2','C1',NULL,'all.pdf','p2',NULL,'2026-10-01T12:00:00Z')`)
	exec(`INSERT INTO job (id, scope, scope_id, stage, status, input_hash, attempt, error, progress, is_current, stats, created_at,
		config_snapshot, stale_reason) VALUES ('J1','lecture','L1','summary','stale','h',1,NULL,1,1,'{"requests":2}','2026-10-01T10:00:00Z',
		'{"model":"m"}','{"code":"material_changed","params":{"filename":"w1.pdf"}}')`)
	exec(`INSERT INTO sentence (lecture_id, idx, start_ms, end_ms, asr_text, low_conf_words, repaired_text, text, zh_status, words)
		VALUES ('L1',0,0,1000,'hello','[]','hello','hello','none','[{"w":"hello","s":0,"e":900}]')`)
	exec(`INSERT INTO term (id, course_id, en, en_key, zh, source, enabled, freq, tier) VALUES ('T1','C1','LFSR','lfsr','线性反馈移位寄存器','manual',1,3,'reference')`)
	exec(`INSERT INTO kv (key, value) VALUES ('cite_exported:C1','2026-10-10T10:00:00Z')`)
	before := dumpTables(t, d, v6Queries)

	// Up to 007 exactly (008 is tested by TestMigration008Upgrade).
	if err := migrateUpTo(ctx, d, 7); err != nil {
		t.Fatal(err)
	}
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 7 {
		t.Fatalf("user_version = %d, want 7", v)
	}
	if after := dumpTables(t, d, v6Queries); after != before {
		t.Fatalf("data changed by migration 007.\n--- before ---\n%s--- after ---\n%s", before, after)
	}

	st := New(d)
	defer st.Close()
	c, err := st.GetCourse(ctx, "C1")
	if err != nil || c.Numbering != model.NumberingAuto {
		t.Fatalf("course numbering = %q, %v", c.Numbering, err)
	}
	for _, id := range []string{"L1", "L2"} {
		l, err := st.GetLecture(ctx, id)
		if err != nil || l.Kind != "" || l.KindSource != model.KindSourceNone {
			t.Fatalf("lecture %s kind = %q/%q, %v", id, l.Kind, l.KindSource, err)
		}
	}
	var nullKinds int
	d.QueryRow(`SELECT count(*) FROM lecture WHERE kind IS NULL`).Scan(&nullKinds)
	if nullKinds != 2 {
		t.Fatalf("%d lectures with kind NULL, want 2", nullKinds)
	}
	ms, err := st.ListMaterials(ctx, "C1")
	if err != nil || len(ms) != 2 || ms[0].BindWeek != nil || ms[1].BindWeek != nil || *ms[0].LectureID != "L1" ||
		!reflect.DeepEqual(ms[0].PagesMD, []string{"a", "b"}) {
		t.Fatalf("materials = %+v, %v", ms, err)
	}
	if model.BindScope(ms[0]) != model.BindLecture || model.BindScope(ms[1]) != model.BindCourse {
		t.Fatalf("scopes %s %s", model.BindScope(ms[0]), model.BindScope(ms[1]))
	}

	kinds, err := st.ListLectureKinds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.LectureKind{
		{ID: model.KindLecture, Builtin: true, Code: "L", Group: model.KindGroupSession, Order: 10},
		{ID: model.KindTutorial, Builtin: true, Code: "T", Group: model.KindGroupSession, Order: 20},
		{ID: model.KindSeminar, Builtin: true, Code: "S", Group: model.KindGroupSession, Order: 30},
		{ID: model.KindPastLecture, Builtin: true, Code: "P", Group: model.KindGroupMaterial, Order: 40},
		{ID: model.KindMaterial, Builtin: true, Code: "M", Group: model.KindGroupMaterial, Order: 50},
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %+v", kinds)
	}
	for i, id := range model.BuiltinKindIDs {
		if kinds[i].ID != id {
			t.Errorf("BuiltinKindIDs[%d] = %s, catalog has %s", i, id, kinds[i].ID)
		}
	}
	// The CHECK constraints of the DDL hold.
	for _, q := range []string{
		`UPDATE lecture SET kind_source = 'guess' WHERE id = 'L1'`,
		`UPDATE course SET numbering = 'sometimes' WHERE id = 'C1'`,
		`INSERT INTO lecture_kind (id, code, grp) VALUES ('x', 'X', 'other')`,
		`INSERT INTO lecture_kind (id, code, grp) VALUES ('y', 'L', 'session')`,
	} {
		if _, err := d.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}

func TestLectureKindsCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	lab := "实验课"
	k, err := st.InsertLectureKind(ctx, model.LectureKind{Code: "LAB", Name: &lab, Group: model.KindGroupSession, Builtin: true})
	if err != nil {
		t.Fatal(err)
	}
	if k.ID == "" || k.Builtin || k.Order != 60 {
		t.Fatalf("inserted %+v: want an id, not built-in, order 60", k)
	}
	got, err := st.GetLectureKind(ctx, k.ID)
	if err != nil || !reflect.DeepEqual(got, k) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, k)
	}
	all, _ := st.ListLectureKinds(ctx)
	if len(all) != 6 || all[5].ID != k.ID {
		t.Fatalf("catalog = %+v", all)
	}

	// Code and name are unique (the name trimmed, case-insensitively).
	other := " LAB "
	for _, c := range []struct {
		k    model.LectureKind
		want error
	}{
		{model.LectureKind{Code: "LAB", Group: model.KindGroupSession, Name: sp("Lab 2")}, ErrKindCodeTaken},
		{model.LectureKind{Code: "T", Group: model.KindGroupSession, Name: sp("Tut")}, ErrKindCodeTaken},
		{model.LectureKind{Code: "LB", Group: model.KindGroupSession, Name: sp(" 实验课")}, ErrKindNameTaken},
		{model.LectureKind{Code: "LX", Group: model.KindGroupSession, Name: &other}, nil}, // "LAB" is a code, not a name
	} {
		_, err := st.InsertLectureKind(ctx, c.k)
		if !errors.Is(err, c.want) || (c.want != nil && !errors.Is(err, ErrDuplicate)) {
			t.Errorf("insert %+v: %v, want %v", c.k, err, c.want)
		}
	}
	// Explicit order and id are kept.
	wk, err := st.InsertLectureKind(ctx, model.LectureKind{ID: "custom-wk", Code: "WS", Name: sp("Workshop"), Group: model.KindGroupMaterial, Order: 5})
	if err != nil || wk.ID != "custom-wk" || wk.Order != 5 {
		t.Fatalf("insert with id/order: %+v %v", wk, err)
	}
	if all, _ := st.ListLectureKinds(ctx); all[0].ID != "custom-wk" {
		t.Fatalf("ordered by ord: %+v", all)
	}

	// Update a custom kind; conflicts with others, not with itself.
	k.Code, k.Name, k.Group, k.Order = "LB", sp("Lab session"), model.KindGroupMaterial, 0
	if err := st.UpdateLectureKind(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetLectureKind(ctx, k.ID)
	if got.Code != "LB" || *got.Name != "Lab session" || got.Group != model.KindGroupMaterial || got.Order != 60 {
		t.Fatalf("updated %+v (order 0 keeps 60)", got)
	}
	k.Code = "S"
	if err := st.UpdateLectureKind(ctx, k); !errors.Is(err, ErrKindCodeTaken) {
		t.Errorf("update to a taken code: %v", err)
	}
	k.Code, k.Name = "LB", sp("WORKSHOP")
	if err := st.UpdateLectureKind(ctx, k); !errors.Is(err, ErrKindNameTaken) {
		t.Errorf("update to a taken name: %v", err)
	}
	if err := st.UpdateLectureKind(ctx, model.LectureKind{ID: "nope", Code: "Q", Group: model.KindGroupSession}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}

	// Built-in kinds are never edited or deleted.
	if err := st.UpdateLectureKind(ctx, model.LectureKind{ID: model.KindTutorial, Code: "TU", Group: model.KindGroupSession}); !errors.Is(err, ErrBuiltinKind) {
		t.Errorf("update built-in: %v", err)
	}
	if err := st.DeleteLectureKind(ctx, model.KindPastLecture); !errors.Is(err, ErrBuiltinKind) {
		t.Errorf("delete built-in: %v", err)
	}
	if b, _ := st.GetLectureKind(ctx, model.KindTutorial); b.Code != "T" {
		t.Errorf("built-in changed: %+v", b)
	}

	// A kind in use cannot be deleted; counts are by stored kind.
	c1, c2 := mkCourse(t, st), mkCourse(t, st)
	l1, l2, l3 := mkLecture(t, st, c1.ID), mkLecture(t, st, c1.ID), mkLecture(t, st, c2.ID)
	mkLecture(t, st, c2.ID) // no stored kind
	st.SetLectureKind(ctx, l1.ID, k.ID, model.KindSourceUser)
	st.SetLectureKind(ctx, l2.ID, model.KindTutorial, model.KindSourceFilename)
	st.SetLectureKind(ctx, l3.ID, k.ID, model.KindSourceLLM)
	if err := st.DeleteLectureKind(ctx, k.ID); !errors.Is(err, ErrKindInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if n, err := st.CountLecturesByKind(ctx, ""); err != nil || !reflect.DeepEqual(n, map[string]int{k.ID: 2, model.KindTutorial: 1}) {
		t.Fatalf("count all = %v, %v", n, err)
	}
	if n, _ := st.CountLecturesByKind(ctx, c2.ID); !reflect.DeepEqual(n, map[string]int{k.ID: 1}) {
		t.Fatalf("count c2 = %v", n)
	}
	st.SetLectureKind(ctx, l1.ID, "", "")
	st.SetLectureKind(ctx, l3.ID, "", "")
	if err := st.DeleteLectureKind(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetLectureKind(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted kind: %v", err)
	}
	if err := st.DeleteLectureKind(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// A deleted code is free again.
	if _, err := st.InsertLectureKind(ctx, model.LectureKind{Code: "LB", Name: sp("Lab session"), Group: model.KindGroupSession}); err != nil {
		t.Fatalf("reuse code: %v", err)
	}
}

func TestLectureKindFields(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	if c.Numbering != model.NumberingAuto {
		t.Fatalf("new course numbering = %q", c.Numbering)
	}
	l := model.Lecture{ID: NewID(), CourseID: c.ID, Title: "T01", MediaPath: "m", Kind: model.KindTutorial, KindSource: model.KindSourceFilename}
	if err := st.CreateLecture(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetLecture(ctx, l.ID)
	if got.Kind != model.KindTutorial || got.KindSource != model.KindSourceFilename {
		t.Fatalf("created %q/%q", got.Kind, got.KindSource)
	}
	plain := mkLecture(t, st, c.ID)
	if plain.Kind != "" || plain.KindSource != model.KindSourceNone {
		t.Fatalf("default %q/%q", plain.Kind, plain.KindSource)
	}
	if err := st.SetLectureKind(ctx, plain.ID, model.KindSeminar, model.KindSourceLLM); err != nil {
		t.Fatal(err)
	}
	ls, _ := st.ListLectures(ctx, c.ID)
	if ls[1].Kind != model.KindSeminar || ls[1].KindSource != model.KindSourceLLM {
		t.Fatalf("listed %+v", ls[1])
	}
	if err := st.SetLectureKind(ctx, plain.ID, model.KindSeminar, "guess"); err == nil {
		t.Error("an unknown kind_source was accepted")
	}
	if err := st.SetLectureKind(ctx, "nope", model.KindSeminar, model.KindSourceUser); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown lecture: %v", err)
	}

	if err := st.SetCourseNumbering(ctx, c.ID, model.NumberingNever); err != nil {
		t.Fatal(err)
	}
	if c2, _ := st.GetCourse(ctx, c.ID); c2.Numbering != model.NumberingNever {
		t.Fatalf("numbering = %q", c2.Numbering)
	}
	st.SetCourseNumbering(ctx, c.ID, "")
	if c2, _ := st.GetCourse(ctx, c.ID); c2.Numbering != model.NumberingAuto {
		t.Fatalf("numbering \"\" = %q", c2.Numbering)
	}
	if err := st.SetCourseNumbering(ctx, c.ID, "sometimes"); err == nil {
		t.Error("an unknown numbering was accepted")
	}
	cc := model.Course{ID: NewID(), Name: "X", Numbering: model.NumberingAlways}
	st.CreateCourse(ctx, cc)
	if got, _ := st.GetCourse(ctx, cc.ID); got.Numbering != model.NumberingAlways {
		t.Fatalf("created numbering = %q", got.Numbering)
	}
}

func TestMaterialBinding(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	week := model.Material{ID: NewID(), CourseID: c.ID, Filename: "Week3.pdf", Path: "p", PagesMD: []string{"x"}, BindWeek: ip(3), CreatedAt: "2026-10-01T00:00:00Z"}
	lec := model.Material{ID: NewID(), CourseID: c.ID, LectureID: &l.ID, Filename: "l.pdf", Path: "p", CreatedAt: "2026-10-02T00:00:00Z"}
	all := model.Material{ID: NewID(), CourseID: c.ID, Filename: "all.pdf", Path: "p", CreatedAt: "2026-10-03T00:00:00Z"}
	for _, m := range []model.Material{week, lec, all} {
		if err := st.CreateMaterial(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	both := model.Material{ID: NewID(), CourseID: c.ID, LectureID: &l.ID, BindWeek: ip(2), Filename: "b.pdf", Path: "p"}
	if err := st.CreateMaterial(ctx, both); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("both bindings: %v", err)
	}
	if _, err := st.GetMaterial(ctx, both.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused material was stored")
	}
	if err := st.CreateMaterial(ctx, model.Material{ID: NewID(), CourseID: c.ID, BindWeek: ip(61), Filename: "x", Path: "p"}); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("week 61: %v", err)
	}
	got, _ := st.GetMaterial(ctx, week.ID)
	if got.BindWeek == nil || *got.BindWeek != 3 || got.LectureID != nil || model.BindScope(got) != model.BindWeek {
		t.Fatalf("week material = %+v", got)
	}

	// ListMaterialRefs: same rows, no page text.
	refs, err := st.ListMaterialRefs(ctx, c.ID)
	if err != nil || len(refs) != 3 {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
	full, _ := st.ListMaterials(ctx, c.ID)
	for i := range refs {
		if refs[i].PagesMD != nil {
			t.Errorf("ref %s has pages", refs[i].ID)
		}
		f := full[i]
		f.PagesMD = nil
		if !reflect.DeepEqual(refs[i], f) {
			t.Errorf("ref %+v, material %+v", refs[i], f)
		}
	}
	// v1 bindings list only lecture-bound materials.
	if b, _ := st.ListMaterialBindings(ctx, c.ID); !reflect.DeepEqual(b, map[string]string{lec.ID: l.ID}) {
		t.Fatalf("bindings = %v", b)
	}

	// Rebinding: week → lecture → week → course; the two never coexist.
	if err := st.SetMaterialBinding(ctx, week.ID, &l.ID, ip(4)); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("both: %v", err)
	}
	for _, w := range []int{0, -1, 61} {
		if err := st.SetMaterialBinding(ctx, week.ID, nil, ip(w)); !errors.Is(err, ErrBindingInvalid) {
			t.Errorf("week %d: %v", w, err)
		}
	}
	if err := st.SetMaterialLecture(ctx, week.ID, &l.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetMaterial(ctx, week.ID); got.BindWeek != nil || got.LectureID == nil {
		t.Fatalf("v1 binding to a lecture kept the week: %+v", got)
	}
	if err := st.SetMaterialBinding(ctx, week.ID, nil, ip(60)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetMaterial(ctx, week.ID); got.BindWeek == nil || *got.BindWeek != 60 || got.LectureID != nil {
		t.Fatalf("week binding: %+v", got)
	}
	if err := st.SetMaterialLecture(ctx, week.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetMaterial(ctx, week.ID); got.BindWeek != nil || got.LectureID != nil || model.BindScope(got) != model.BindCourse {
		t.Fatalf("v1 course-wide binding kept the week: %+v", got)
	}
	if err := st.SetMaterialBinding(ctx, "nope", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown material: %v", err)
	}

	// Deleting the lecture still deletes the materials bound to it, and
	// leaves week-bound and course-wide ones.
	st.SetMaterialBinding(ctx, week.ID, nil, ip(3))
	if err := st.DeleteLecture(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	refs, _ = st.ListMaterialRefs(ctx, c.ID)
	if len(refs) != 2 || refs[0].ID != week.ID || refs[1].ID != all.ID {
		t.Fatalf("after deleting the lecture: %+v", refs)
	}
}

func TestValidKindHelpers(t *testing.T) {
	for code, ok := range map[string]bool{"L": true, "LAB": true, "": false, "ABCD": false, "l": false, "L1": false, "É": false} {
		if model.ValidKindCode(code) != ok {
			t.Errorf("ValidKindCode(%q) = %v", code, !ok)
		}
	}
	if !model.ValidNumbering(model.NumberingAlways) || model.ValidNumbering("") || !model.ValidKindGroup(model.KindGroupMaterial) ||
		model.ValidKindGroup("x") || !model.ValidKindSource(model.KindSourceLLM) || model.ValidKindSource("") {
		t.Error("validators")
	}
}
