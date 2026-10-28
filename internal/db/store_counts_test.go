// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// perf-fix: the batch counts equal the per-lecture methods, also across
// statement chunks (more ids than countChunk).
func TestLectureCountsMatchPerLectureAcrossChunks(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	n := countChunk + 7
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		l := mkLecture(t, st, c.ID)
		ids = append(ids, l.ID)
		k := i % 4 // 0 sentences, 1 without words, 2 with words on the last, 3 on the first only
		var ss []model.Sentence
		for j := 0; j < k; j++ {
			s := model.Sentence{Idx: j, StartMS: int64(j) * 1000, EndMS: int64(j*1000 + 500 + i), ASRText: "x", RepairedText: "x",
				ZhStatus: []string{model.ZhNone, model.ZhOK, model.ZhStale}[j%3]}
			if (k == 2 && j == k-1) || (k == 3 && j == 0) {
				s.Words = []model.Word{{W: "x", S: 0, E: 10}}
			}
			ss = append(ss, s)
		}
		if len(ss) > 0 {
			if err := st.InsertSentences(ctx, l.ID, ss); err != nil {
				t.Fatal(err)
			}
		}
		if i%5 == 0 {
			p := []model.Patch{{ID: NewID(), LectureID: l.ID, SentenceIdx: 0, Before: "x", After: "y", Class: "decide", Reason: "r",
				Review: model.ReviewPending, CreatedAt: Now()}}
			if err := st.InsertPatches(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	cnt := st.(Counter)
	got, err := cnt.LectureCounts(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d entries, want %d", len(got), n)
	}
	end := func(id string, idx int) SentenceEnd {
		x, err := st.GetSentence(ctx, id, idx)
		if err != nil {
			return SentenceEnd{}
		}
		return SentenceEnd{Found: true, EndMS: x.EndMS, HasWords: len(x.Words) > 0}
	}
	for i, id := range ids {
		var want LectureCounts
		if want.Sentences, err = st.CountSentences(ctx, id); err != nil {
			t.Fatal(err)
		}
		if want.Zh, err = st.CountZh(ctx, id); err != nil {
			t.Fatal(err)
		}
		if want.PatchesPending, err = st.CountPatches(ctx, id, model.ReviewPending); err != nil {
			t.Fatal(err)
		}
		if want.Sentences > 0 {
			want.Last, want.First = end(id, want.Sentences-1), end(id, 0)
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Fatalf("lecture %d: %+v, want %+v", i, got[id], want)
		}
	}
	pend, err := cnt.PendingPatchCounts(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != (n+4)/5 {
		t.Fatalf("%d lectures with pending patches, want %d", len(pend), (n+4)/5)
	}
	// Term counts by course; a course without terms is absent (0).
	c2 := mkCourse(t, st)
	for i := 0; i < 3; i++ {
		if err := st.InsertTerm(ctx, model.Term{ID: NewID(), CourseID: c.ID, En: fmt.Sprintf("term %d", i), Zh: "术语", Source: "syllabus", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	tc, err := cnt.TermCounts(ctx, []string{c.ID, c2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if tc[c.ID] != 3 || tc[c2.ID] != 0 {
		t.Fatalf("term counts %v", tc)
	}
	// No ids: empty results, no statement with an empty IN list.
	if m, err := cnt.LectureCounts(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("no ids: %v %v", m, err)
	}
	if m, err := cnt.PendingPatchCounts(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("no ids: %v %v", m, err)
	}
}

// The tx-bound store has the batch counts too (the API may call them in a
// transaction later).
func TestCounterInTransaction(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	if err := st.Tx(ctx, func(tx Store) error {
		m, err := tx.(Counter).LectureCounts(ctx, []string{l.ID})
		if err != nil {
			return err
		}
		if m[l.ID].Sentences != 0 || len(m[l.ID].Zh) != 4 {
			return fmt.Errorf("counts %+v", m[l.ID])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
