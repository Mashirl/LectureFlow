// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"strings"

	"lectureflow/internal/model"
)

// Batch counts of the lecture lists (perf-fix): the dashboard and a
// course's lecture list read four numbers per lecture, one query each
// (CountSentences, CountZh, CountPatches, GetSentence); these read them for
// many lectures in a few GROUP BY queries. They are not part of the frozen
// Store interface: the API uses them when the store has them (Counter) and
// falls back to the per-lecture methods otherwise, with the same results.

// LectureCounts are one lecture's numbers, as the per-lecture methods give
// them.
type LectureCounts struct {
	Sentences int            // CountSentences
	Zh        map[string]int // CountZh: all four statuses present
	// PatchesPending is CountPatches(…, model.ReviewPending).
	PatchesPending int
	// Last is GetSentence(…, Sentences-1) and First GetSentence(…, 0)
	// reduced to what the views read; Found false = ErrNotFound (no
	// sentences, or idx not contiguous).
	Last, First SentenceEnd
}

// SentenceEnd is the part of a sentence a lecture view reads.
type SentenceEnd struct {
	Found    bool
	EndMS    int64
	HasWords bool // len(Words) > 0
	// Err is GetSentence's error for a row that does not decode (bad words
	// or low_conf_words JSON); a view fails only if it reads this sentence.
	Err error
}

// Counter is implemented by the SQLite store.
type Counter interface {
	// LectureCounts returns the counts of every lecture in ids (a lecture
	// without rows gets zero counts and its four zh statuses).
	LectureCounts(ctx context.Context, ids []string) (map[string]LectureCounts, error)
	// PendingPatchCounts is CountPatches(…, model.ReviewPending) for every
	// lecture in ids (absent = 0).
	PendingPatchCounts(ctx context.Context, ids []string) (map[string]int, error)
	// TermCounts is len(ListTerms(course)) for every course in ids
	// (absent = 0).
	TermCounts(ctx context.Context, courseIDs []string) (map[string]int, error)
}

var _ Counter = (*sqlStore)(nil)

// countChunk bounds the ids of one statement (two parameters per id in the
// first/last query, far below SQLite's limit).
const countChunk = 400

func idChunks(ids []string) [][]string {
	var out [][]string
	for len(ids) > countChunk {
		out = append(out, ids[:countChunk])
		ids = ids[countChunk:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

func anyArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func (s *sqlStore) LectureCounts(ctx context.Context, ids []string) (map[string]LectureCounts, error) {
	out := make(map[string]LectureCounts, len(ids))
	for _, id := range ids {
		out[id] = LectureCounts{Zh: map[string]int{model.ZhNone: 0, model.ZhOK: 0, model.ZhQAFailed: 0, model.ZhStale: 0}}
	}
	for _, part := range idChunks(ids) {
		if err := s.countSentences(ctx, part, out); err != nil {
			return nil, err
		}
	}
	pend, err := s.PendingPatchCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, n := range pend {
		if c, ok := out[id]; ok {
			c.PatchesPending = n
			out[id] = c
		}
	}
	// The first and the last sentence of the lectures that have any.
	type key struct {
		id  string
		idx int
	}
	var want []key
	for _, id := range ids {
		if n := out[id].Sentences; n > 0 {
			want = append(want, key{id, n - 1})
			if n > 1 {
				want = append(want, key{id, 0})
			}
		}
	}
	for len(want) > 0 {
		part := want
		if len(part) > countChunk {
			part = want[:countChunk]
		}
		want = want[len(part):]
		args := make([]any, 0, 2*len(part))
		vals := make([]string, len(part))
		for i, k := range part {
			vals[i] = "(?,?)"
			args = append(args, k.id, k.idx)
		}
		// The whole rows through scanSentence, so a row fails to decode
		// exactly as GetSentence would.
		rows, err := s.q.QueryContext(ctx, `SELECT `+sentenceCols+` FROM sentence
			WHERE (lecture_id, idx) IN (VALUES `+strings.Join(vals, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				x, err := scanSentence(rows)
				if err != nil && x.LectureID == "" {
					return err // the row itself could not be read
				}
				e := SentenceEnd{Found: true, EndMS: x.EndMS, HasWords: len(x.Words) > 0, Err: err}
				c := out[x.LectureID]
				if x.Idx == c.Sentences-1 {
					c.Last = e
				}
				if x.Idx == 0 {
					c.First = e
				}
				out[x.LectureID] = c
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// countSentences adds the sentence and zh_status counts of ids to out.
func (s *sqlStore) countSentences(ctx context.Context, ids []string, out map[string]LectureCounts) error {
	rows, err := s.q.QueryContext(ctx, `SELECT lecture_id, zh_status, count(*) FROM sentence
		WHERE lecture_id IN (`+placeholders(len(ids))+`) GROUP BY lecture_id, zh_status`, anyArgs(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, st string
		var n int
		if err := rows.Scan(&id, &st, &n); err != nil {
			return err
		}
		c := out[id]
		c.Sentences += n
		c.Zh[st] = n
		out[id] = c
	}
	return rows.Err()
}

func (s *sqlStore) PendingPatchCounts(ctx context.Context, ids []string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range idChunks(ids) {
		args := append([]any{model.ReviewPending}, anyArgs(part)...)
		rows, err := s.q.QueryContext(ctx, `SELECT lecture_id, count(*) FROM patch
			WHERE review = ? AND lecture_id IN (`+placeholders(len(part))+`) GROUP BY lecture_id`, args...)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var id string
				var n int
				if err := rows.Scan(&id, &n); err != nil {
					return err
				}
				out[id] = n
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqlStore) TermCounts(ctx context.Context, courseIDs []string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range idChunks(courseIDs) {
		rows, err := s.q.QueryContext(ctx, `SELECT course_id, count(*) FROM term
			WHERE course_id IN (`+placeholders(len(part))+`) GROUP BY course_id`, anyArgs(part)...)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var id string
				var n int
				if err := rows.Scan(&id, &n); err != nil {
					return err
				}
				out[id] = n
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
