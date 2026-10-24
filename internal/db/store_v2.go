// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
)

// v2 phase 2 store methods (migrations/006_v2.sql). FROZEN by the lead.

func (s *sqlStore) SetCourseCalendar(ctx context.Context, id string, termStart *string, skipWeeks []string) error {
	skip, err := skipWeeksJSON(skipWeeks)
	if err != nil {
		return err
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET term_start = ?, skip_weeks = ? WHERE id = ?`,
		nullStr(termStart), skip, id))
}

func (s *sqlStore) SetCourseFilenameRule(ctx context.Context, id string, rule *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET filename_rule = ? WHERE id = ?`, nullStr(rule), id))
}

func (s *sqlStore) SetLectureRecordedAt(ctx context.Context, id string, at *string, source string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET recorded_at = ?, recorded_at_source = ? WHERE id = ?`,
		nullStr(at), source, id))
}

func (s *sqlStore) SetLectureTitles(ctx context.Context, id, title string, titleZh *string, source string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET title = ?, title_zh = ?, title_source = ? WHERE id = ?`,
		title, nullStr(titleZh), source, id))
}

func (s *sqlStore) SetLectureBlurb(ctx context.Context, id string, en, zh *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET blurb_en = ?, blurb_zh = ? WHERE id = ?`,
		nullStr(en), nullStr(zh), id))
}

func (s *sqlStore) SetLectureOriginalFilename(ctx context.Context, id, name string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET original_filename = ? WHERE id = ?`, name, id))
}

func (s *sqlStore) ListStaleReasons(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, stale_reason FROM job WHERE stale_reason IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var r sql.NullString
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r.String
	}
	return out, rows.Err()
}

func (s *sqlStore) ReplaceStaleReason(ctx context.Context, jobID, reason string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET stale_reason = ? WHERE id = ?`, reason, jobID))
}
