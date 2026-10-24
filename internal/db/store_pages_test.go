// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// MaterialPageCounts (lecture-kinds review item 9): the page counts of a
// course's extracted materials, equal to len(PagesMD) of ListMaterials,
// without the page texts; not extracted → missing; other courses apart.
func TestMaterialPageCounts(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c, other := mkCourse(t, st), mkCourse(t, st)
	three := model.Material{ID: NewID(), CourseID: c.ID, Filename: "a.pdf", Path: "p", PagesMD: []string{"# 1", "# 2", "# 3"}}
	empty := model.Material{ID: NewID(), CourseID: c.ID, Filename: "b.pdf", Path: "p", PagesMD: []string{}}
	pending := model.Material{ID: NewID(), CourseID: c.ID, Filename: "c.pdf", Path: "p"}
	elsewhere := model.Material{ID: NewID(), CourseID: other.ID, Filename: "d.pdf", Path: "p", PagesMD: []string{"x"}}
	for _, m := range []model.Material{three, empty, pending, elsewhere} {
		if err := st.CreateMaterial(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.MaterialPageCounts(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{three.ID: 3, empty.ID: 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	// the same numbers as ListMaterials
	full, _ := st.ListMaterials(ctx, c.ID)
	for _, m := range full {
		if got[m.ID] != len(m.PagesMD) {
			t.Errorf("%s: %d pages counted, %d listed", m.Filename, got[m.ID], len(m.PagesMD))
		}
	}
	// extraction finishing is seen at once
	if err := st.SetMaterialPages(ctx, pending.ID, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MaterialPageCounts(ctx, c.ID); got[pending.ID] != 2 || len(got) != 3 {
		t.Fatalf("after extraction: %v", got)
	}
	if got, _ := st.MaterialPageCounts(ctx, other.ID); !reflect.DeepEqual(got, map[string]int{elsewhere.ID: 1}) {
		t.Fatalf("other course: %v", got)
	}
	if got, err := st.MaterialPageCounts(ctx, "nope"); err != nil || len(got) != 0 {
		t.Fatalf("unknown course: %v, %v", got, err)
	}
	// a damaged pages_md is 0 pages, not an error
	raw := st.(*sqlStore)
	for _, bad := range []string{`not json`, `"one page"`, `{"a":1}`} {
		if _, err := raw.q.ExecContext(ctx, `UPDATE material SET pages_md = ? WHERE id = ?`, bad, three.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := st.MaterialPageCounts(ctx, c.ID); err != nil || got[three.ID] != 0 {
			t.Errorf("pages_md %s: %v, %v", bad, got, err)
		}
	}
}
