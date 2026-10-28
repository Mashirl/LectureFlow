// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"

	"lectureflow/internal/model"
)

// Week overrides (migrations/011_week_override.sql; course-ux). Reads go
// with the lecture (lectureSelect in store_sql.go).

// ErrWeekOverrideInvalid: a week outside 1–model.MaxBindWeek or a kind
// other than supplement / makeup.
var ErrWeekOverrideInvalid = errors.New("invalid week override")

func (s *sqlStore) SetWeekOverride(ctx context.Context, lectureID string, o model.WeekOverride) error {
	if !model.ValidOverrideWeek(o.Week) || !model.ValidWeekOverrideKind(o.Kind) {
		return ErrWeekOverrideInvalid
	}
	if o.CreatedAt == "" {
		o.CreatedAt = Now()
	}
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		var one int
		if err := t.q.QueryRowContext(ctx, `SELECT 1 FROM lecture WHERE id = ?`, lectureID).Scan(&one); err != nil {
			return notFound(err)
		}
		_, err := t.q.ExecContext(ctx, `INSERT INTO lecture_week_override (lecture_id, week, kind, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (lecture_id) DO UPDATE SET week = excluded.week, kind = excluded.kind, created_at = excluded.created_at`,
			lectureID, o.Week, o.Kind, o.CreatedAt)
		return err
	})
}

func (s *sqlStore) DeleteWeekOverride(ctx context.Context, lectureID string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `DELETE FROM lecture_week_override WHERE lecture_id = ?`, lectureID))
}
