// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Download tasks of the browser extension (migrations/010_downloads.sql,
// .claude/ext-brief.md §6.2). These methods are not part of Store: the
// download queue (internal/fetch) declares the narrow interface it needs
// and cmd hands it the store, which implements it.

// Download task states (the CHECK of download_task.state; the same values
// as the bridge protocol's task states).
var DownloadStates = []string{"queued", "downloading", "merging", "importing", "done", "failed", "needs_refresh", "cancelled"}

// unfinishedDownloadStates are the states a task cannot keep across a
// restart: its credentials lived in the old process only.
var unfinishedDownloadStates = []string{"queued", "downloading", "merging", "importing"}

// DownloadTask is one row of download_task. It never holds a credential:
// no cookie, no referer, no query string, no track path (tracks_json keeps
// each track's host and a hash of its path; internal/fetch privacy.go), no
// page path (page_path stays empty: migrations/013).
type DownloadTask struct {
	ID               int64
	Platform         string
	SessionID        string
	ProfileID        string
	ProfileVersion   string
	OverrideDisabled bool
	CourseID         string
	Title            string
	RecordedAt       *string // local "YYYY-MM-DDTHH:MM"; nil = unknown
	DurationSec      int
	PageHost         string
	TracksJSON       string
	State            string
	ErrorCode        string // "" = NULL
	ErrorParams      string // JSON object; "" = NULL
	BytesDone        int64
	BytesTotal       int64 // -1 = unknown
	LectureID        *string
	CreatedAt        string
	UpdatedAt        string
	// MaxConnections is the profile's maxConnectionsPerFile sent with the
	// enqueue (migrations/012); 0 = no limit.
	MaxConnections int
}

const downloadCols = `id, platform, session_id, profile_id, profile_version, override_disabled, course_id, title,
	recorded_at, duration_sec, page_host, tracks_json, state, error_code, error_params,
	bytes_done, bytes_total, lecture_id, created_at, updated_at, max_connections`

func scanDownload(sc interface{ Scan(...any) error }) (DownloadTask, error) {
	var t DownloadTask
	var override int
	var recorded, code, params, lecture sql.NullString
	err := sc.Scan(&t.ID, &t.Platform, &t.SessionID, &t.ProfileID, &t.ProfileVersion, &override, &t.CourseID, &t.Title,
		&recorded, &t.DurationSec, &t.PageHost, &t.TracksJSON, &t.State, &code, &params,
		&t.BytesDone, &t.BytesTotal, &lecture, &t.CreatedAt, &t.UpdatedAt, &t.MaxConnections)
	t.OverrideDisabled = override != 0
	t.RecordedAt = strPtr(recorded)
	t.ErrorCode, t.ErrorParams = code.String, params.String
	t.LectureID = strPtr(lecture)
	return t, err
}

func duplicateErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %v", ErrDuplicate, err)
	}
	return err
}

// InsertDownloadTask adds a task and returns its id; ErrDuplicate when a
// task of the same platform and session exists. CreatedAt and UpdatedAt
// default to now.
func (s *sqlStore) InsertDownloadTask(ctx context.Context, t DownloadTask) (int64, error) {
	if t.CreatedAt == "" {
		t.CreatedAt = Now()
	}
	if t.UpdatedAt == "" {
		t.UpdatedAt = t.CreatedAt
	}
	res, err := s.q.ExecContext(ctx, `INSERT INTO download_task (platform, session_id, profile_id, profile_version,
		override_disabled, course_id, title, recorded_at, duration_sec, page_host, page_path, tracks_json, state,
		error_code, error_params, bytes_done, bytes_total, lecture_id, created_at, updated_at, max_connections)
		VALUES (?,?,?,?,?,?,?,?,?,?,'',?,?,?,?,?,?,?,?,?,?)`,
		t.Platform, t.SessionID, t.ProfileID, t.ProfileVersion, boolInt(t.OverrideDisabled), t.CourseID, t.Title,
		nullStr(t.RecordedAt), t.DurationSec, t.PageHost, t.TracksJSON, t.State,
		nullIfEmpty(t.ErrorCode), nullIfEmpty(t.ErrorParams), t.BytesDone, t.BytesTotal, nullStr(t.LectureID),
		t.CreatedAt, t.UpdatedAt, t.MaxConnections)
	if err != nil {
		return 0, duplicateErr(err)
	}
	return res.LastInsertId()
}

// UpdateDownloadTask writes every column of the task but id, platform,
// session_id and created_at; ErrNotFound when the row is gone (its course
// was deleted). UpdatedAt defaults to now.
func (s *sqlStore) UpdateDownloadTask(ctx context.Context, t DownloadTask) error {
	if t.UpdatedAt == "" {
		t.UpdatedAt = Now()
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE download_task SET profile_id = ?, profile_version = ?,
		override_disabled = ?, course_id = ?, title = ?, recorded_at = ?, duration_sec = ?, page_host = ?, page_path = '',
		tracks_json = ?, state = ?, error_code = ?, error_params = ?, bytes_done = ?, bytes_total = ?, lecture_id = ?,
		updated_at = ?, max_connections = ? WHERE id = ?`,
		t.ProfileID, t.ProfileVersion, boolInt(t.OverrideDisabled), t.CourseID, t.Title, nullStr(t.RecordedAt),
		t.DurationSec, t.PageHost, t.TracksJSON, t.State, nullIfEmpty(t.ErrorCode), nullIfEmpty(t.ErrorParams),
		t.BytesDone, t.BytesTotal, nullStr(t.LectureID), t.UpdatedAt, t.MaxConnections, t.ID))
}

// SetDownloadProgress records the bytes of a running task; ErrNotFound when
// the row is gone.
func (s *sqlStore) SetDownloadProgress(ctx context.Context, id, done, total int64, updatedAt string) error {
	if updatedAt == "" {
		updatedAt = Now()
	}
	return affectedOrNotFound(s.q.ExecContext(ctx,
		`UPDATE download_task SET bytes_done = ?, bytes_total = ?, updated_at = ? WHERE id = ?`, done, total, updatedAt, id))
}

// GetDownloadTask returns a task; ErrNotFound when there is none.
func (s *sqlStore) GetDownloadTask(ctx context.Context, id int64) (DownloadTask, error) {
	t, err := scanDownload(s.q.QueryRowContext(ctx, `SELECT `+downloadCols+` FROM download_task WHERE id = ?`, id))
	return t, notFound(err)
}

// FindDownloadTask returns the task of a platform's session; ErrNotFound
// when there is none.
func (s *sqlStore) FindDownloadTask(ctx context.Context, platform, sessionID string) (DownloadTask, error) {
	t, err := scanDownload(s.q.QueryRowContext(ctx,
		`SELECT `+downloadCols+` FROM download_task WHERE platform = ? AND session_id = ?`, platform, sessionID))
	return t, notFound(err)
}

// ListDownloadTasks returns every task, oldest first.
func (s *sqlStore) ListDownloadTasks(ctx context.Context) ([]DownloadTask, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+downloadCols+` FROM download_task ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DownloadTask{}
	for rows.Next() {
		t, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteDownloadTask removes a task; ErrNotFound when there is none.
func (s *sqlStore) DeleteDownloadTask(ctx context.Context, id int64) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `DELETE FROM download_task WHERE id = ?`, id))
}

// ResetUnfinishedDownloads is the start-up step of the queue: every task
// that was queued, downloading, merging or importing when the last process
// ended becomes needs_refresh with the error code given (its credentials
// were only in that process). It returns the number of tasks changed.
func (s *sqlStore) ResetUnfinishedDownloads(ctx context.Context, code, updatedAt string) (int64, error) {
	if updatedAt == "" {
		updatedAt = Now()
	}
	args := []any{nullIfEmpty(code), updatedAt}
	for _, st := range unfinishedDownloadStates {
		args = append(args, st)
	}
	res, err := s.q.ExecContext(ctx, `UPDATE download_task SET state = 'needs_refresh', error_code = ?, error_params = NULL,
		updated_at = ? WHERE state IN (`+placeholders(len(unfinishedDownloadStates))+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LectureName is a lecture's title and original file name: what the
// download queue's check compares a platform's session title with (dedup
// by title, bridge.CheckRequest.Titles).
type LectureName struct {
	ID               string
	CourseID         string
	Title            string
	OriginalFilename string // "" when unknown
}

// ListLectureNames returns the title and original file name of every
// lecture of courseID ("" = of every course), oldest first, in one query.
func (s *sqlStore) ListLectureNames(ctx context.Context, courseID string) ([]LectureName, error) {
	q := `SELECT id, course_id, title, original_filename FROM lecture`
	var args []any
	if courseID != "" {
		q += ` WHERE course_id = ?`
		args = append(args, courseID)
	}
	rows, err := s.q.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LectureName{}
	for rows.Next() {
		var n LectureName
		if err := rows.Scan(&n.ID, &n.CourseID, &n.Title, &n.OriginalFilename); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
