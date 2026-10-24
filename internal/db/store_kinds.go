// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"lectureflow/internal/model"
)

// Lecture kinds, numbering mode and week bindings (migrations/007_lecture_kinds.sql;
// lead freeze .claude/kinds-brief.md section 2). Added by K1 on the lead's order.

var (
	// ErrKindCodeTaken: another kind has that code. errors.Is(err, ErrDuplicate) holds.
	ErrKindCodeTaken = fmt.Errorf("lecture kind code taken: %w", ErrDuplicate)
	// ErrKindNameTaken: another custom kind has that name (compared trimmed,
	// case-insensitively). errors.Is(err, ErrDuplicate) holds.
	ErrKindNameTaken = fmt.Errorf("lecture kind name taken: %w", ErrDuplicate)
	// ErrBuiltinKind: built-in kinds are never edited or deleted.
	ErrBuiltinKind = errors.New("built-in lecture kind")
	// ErrKindInUse: a lecture's stored kind is the kind being deleted.
	ErrKindInUse = errors.New("lecture kind in use")
	// ErrBindingInvalid: a material bound to both a lecture and a week, or
	// to a week outside 1–model.MaxBindWeek.
	ErrBindingInvalid = errors.New("invalid material binding")
)

// kindOrderStep is the gap between the ord of consecutive kinds (the
// built-in kinds are seeded with 10, 20, … 50).
const kindOrderStep = 10

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// numberingOrAuto: course.numbering "" is written as auto.
func numberingOrAuto(n string) string {
	if n == "" {
		return model.NumberingAuto
	}
	return n
}

// checkBinding enforces that a material is bound to at most one of a
// lecture and a week, and that a week is in range.
func checkBinding(lectureID *string, week *int) error {
	if lectureID != nil && week != nil {
		return fmt.Errorf("%w: both a lecture and a week", ErrBindingInvalid)
	}
	if week != nil && (*week < 1 || *week > model.MaxBindWeek) {
		return fmt.Errorf("%w: week %d", ErrBindingInvalid, *week)
	}
	return nil
}

const kindCols = `id, builtin, code, name, grp, ord`

func scanKind(sc interface{ Scan(...any) error }) (model.LectureKind, error) {
	var k model.LectureKind
	var builtin int
	var name sql.NullString
	err := sc.Scan(&k.ID, &builtin, &k.Code, &name, &k.Group, &k.Order)
	k.Builtin = builtin != 0
	k.Name = strPtr(name)
	return k, err
}

func (s *sqlStore) ListLectureKinds(ctx context.Context) ([]model.LectureKind, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+kindCols+` FROM lecture_kind ORDER BY ord, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LectureKind
	for rows.Next() {
		k, err := scanKind(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetLectureKind(ctx context.Context, id string) (model.LectureKind, error) {
	k, err := scanKind(s.q.QueryRowContext(ctx, `SELECT `+kindCols+` FROM lecture_kind WHERE id = ?`, id))
	return k, notFound(err)
}

// kindConflict checks the code and the name of k against every other kind.
func (s *sqlStore) kindConflict(ctx context.Context, k model.LectureKind) error {
	all, err := s.ListLectureKinds(ctx)
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.ID == k.ID {
			continue
		}
		if o.Code == k.Code {
			return ErrKindCodeTaken
		}
		if k.Name != nil && o.Name != nil && strings.EqualFold(strings.TrimSpace(*o.Name), strings.TrimSpace(*k.Name)) {
			return ErrKindNameTaken
		}
	}
	return nil
}

// kindWriteErr maps a UNIQUE failure that slipped past kindConflict.
func kindWriteErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: lecture_kind.code") {
		return ErrKindCodeTaken
	}
	return duplicate(err)
}

func (s *sqlStore) InsertLectureKind(ctx context.Context, k model.LectureKind) (model.LectureKind, error) {
	if k.ID == "" {
		k.ID = NewID()
	}
	k.Builtin = false
	err := s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if err := t.kindConflict(ctx, k); err != nil {
			return err
		}
		if k.Order == 0 {
			var max sql.NullInt64
			if err := t.q.QueryRowContext(ctx, `SELECT max(ord) FROM lecture_kind`).Scan(&max); err != nil {
				return err
			}
			k.Order = int(max.Int64) + kindOrderStep
		}
		_, err := t.q.ExecContext(ctx, `INSERT INTO lecture_kind (`+kindCols+`) VALUES (?,0,?,?,?,?)`,
			k.ID, k.Code, nullStr(k.Name), k.Group, k.Order)
		return kindWriteErr(err)
	})
	if err != nil {
		return model.LectureKind{}, err
	}
	return k, nil
}

func (s *sqlStore) UpdateLectureKind(ctx context.Context, k model.LectureKind) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		old, err := t.GetLectureKind(ctx, k.ID)
		if err != nil {
			return err
		}
		if old.Builtin {
			return ErrBuiltinKind
		}
		if k.Order == 0 {
			k.Order = old.Order
		}
		if err := t.kindConflict(ctx, k); err != nil {
			return err
		}
		return kindWriteErr(affectedOrNotFound(t.q.ExecContext(ctx,
			`UPDATE lecture_kind SET code = ?, name = ?, grp = ?, ord = ? WHERE id = ?`,
			k.Code, nullStr(k.Name), k.Group, k.Order, k.ID)))
	})
}

func (s *sqlStore) DeleteLectureKind(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		k, err := t.GetLectureKind(ctx, id)
		if err != nil {
			return err
		}
		if k.Builtin {
			return ErrBuiltinKind
		}
		var n int
		if err := t.q.QueryRowContext(ctx, `SELECT count(*) FROM lecture WHERE kind = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w (%d lectures)", ErrKindInUse, n)
		}
		return affectedOrNotFound(t.q.ExecContext(ctx, `DELETE FROM lecture_kind WHERE id = ?`, id))
	})
}

func (s *sqlStore) CountLecturesByKind(ctx context.Context, courseID string) (map[string]int, error) {
	q := `SELECT kind, count(*) FROM lecture WHERE kind IS NOT NULL`
	var args []any
	if courseID != "" {
		q += ` AND course_id = ?`
		args = append(args, courseID)
	}
	rows, err := s.q.QueryContext(ctx, q+` GROUP BY kind`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

func (s *sqlStore) SetLectureKind(ctx context.Context, id, kind, source string) error {
	if source == "" {
		source = model.KindSourceNone
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET kind = ?, kind_source = ? WHERE id = ?`,
		nullIfEmpty(kind), source, id))
}

func (s *sqlStore) SetCourseNumbering(ctx context.Context, id, numbering string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET numbering = ? WHERE id = ?`, numberingOrAuto(numbering), id))
}

func (s *sqlStore) SetMaterialBinding(ctx context.Context, id string, lectureID *string, week *int) error {
	if err := checkBinding(lectureID, week); err != nil {
		return err
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE material SET lecture_id = ?, bind_week = ? WHERE id = ?`,
		nullStr(lectureID), nullInt(week), id))
}

// materialRefCols are materialCols with pages_md replaced by NULL.
const materialRefCols = `id, course_id, lecture_id, filename, path, NULL, created_at, bind_week`

func (s *sqlStore) ListMaterialRefs(ctx context.Context, courseID string) ([]model.Material, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+materialRefCols+` FROM material WHERE course_id = ? ORDER BY created_at, id`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Material
	for rows.Next() {
		m, err := scanMaterial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MaterialPageCounts counts the pages in SQL (json_array_length): the view
// of a lecture lists its slides with their page counts without loading
// every page text of the course. A pages_md that is not a JSON array
// counts as 0 pages (ListMaterials would report it as an error).
func (s *sqlStore) MaterialPageCounts(ctx context.Context, courseID string) (map[string]int, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, CASE WHEN json_valid(pages_md) AND json_type(pages_md) = 'array'
		THEN json_array_length(pages_md) ELSE 0 END FROM material WHERE course_id = ? AND pages_md IS NOT NULL`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
