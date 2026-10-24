// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"

	"lectureflow/internal/model"
)

// asr_device store methods (SPEC-v2 I6, migrations/008_asr_devices.sql).
// FROZEN by the lead for phase 3b (.claude/v3b-brief.md).

const asrDeviceCols = `id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark`

func scanASRDevice(sc interface{ Scan(...any) error }) (model.ASRDevice, error) {
	var d model.ASRDevice
	var enabled int
	var detected, bench sql.NullString
	err := sc.Scan(&d.ID, &d.Backend, &d.Label, &enabled, &d.Model, &d.Precision, &d.MaxParallel, &d.Priority, &detected, &bench)
	d.Enabled = enabled != 0
	d.DetectedAt = strPtr(detected)
	d.LastBenchmark = strPtr(bench)
	return d, err
}

func (s *sqlStore) ListASRDevices(ctx context.Context) ([]model.ASRDevice, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+asrDeviceCols+` FROM asr_device ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ASRDevice
	for rows.Next() {
		d, err := scanASRDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetASRDevice(ctx context.Context, id string) (model.ASRDevice, error) {
	d, err := scanASRDevice(s.q.QueryRowContext(ctx, `SELECT `+asrDeviceCols+` FROM asr_device WHERE id = ?`, id))
	return d, notFound(err)
}

func (s *sqlStore) InsertASRDevice(ctx context.Context, d model.ASRDevice) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO asr_device (`+asrDeviceCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Backend, d.Label, boolInt(d.Enabled), d.Model, d.Precision, d.MaxParallel, d.Priority,
		nullStr(d.DetectedAt), nullStr(d.LastBenchmark))
	return duplicate(err)
}

func (s *sqlStore) UpdateASRDevice(ctx context.Context, d model.ASRDevice) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE asr_device SET backend = ?, label = ?, enabled = ?, model = ?,
		precision = ?, max_parallel = ?, priority = ?, detected_at = ? WHERE id = ?`,
		d.Backend, d.Label, boolInt(d.Enabled), d.Model, d.Precision, d.MaxParallel, d.Priority, nullStr(d.DetectedAt), d.ID))
}

func (s *sqlStore) SetASRDeviceBenchmark(ctx context.Context, id string, raw *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE asr_device SET last_benchmark = ? WHERE id = ?`, nullStr(raw), id))
}
