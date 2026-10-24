// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"strings"

	"lectureflow/internal/model"
)

// v1.1 queries (migrations/003_v1_1.sql).

func snapshotOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return model.EmptySnapshot
	}
	return s
}

// duplicate maps a UNIQUE constraint failure to ErrDuplicate.
func duplicate(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrDuplicate
	}
	return err
}

func (s *sqlStore) SetCourseCode(ctx context.Context, id string, code *string) error {
	var v any
	if code != nil && strings.TrimSpace(*code) != "" {
		v = strings.TrimSpace(*code)
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET code = ? WHERE id = ?`, v, id))
}

func (s *sqlStore) SetCourseName(ctx context.Context, id, name string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET name = ? WHERE id = ?`, name, id))
}

func (s *sqlStore) SetLectureLaunched(ctx context.Context, id, at string) (bool, error) {
	launched := false
	err := s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		res, err := t.q.ExecContext(ctx, `UPDATE lecture SET launched_at = ? WHERE id = ? AND launched_at IS NULL`, at, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			launched = true
			return nil
		}
		_, err = t.GetLecture(ctx, id) // ErrNotFound vs already launched
		return err
	})
	return launched && err == nil, err
}

func (s *sqlStore) SetLectureSeq(ctx context.Context, id string, seq int) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET seq = ? WHERE id = ?`, seq, id))
}

func (s *sqlStore) NextLectureSeq(ctx context.Context, courseID string) (int, error) {
	var n sql.NullInt64
	if err := s.q.QueryRowContext(ctx, `SELECT MAX(seq) FROM lecture WHERE course_id = ?`, courseID).Scan(&n); err != nil {
		return 0, err
	}
	return int(n.Int64) + 1, nil
}

func (s *sqlStore) SetJobSnapshot(ctx context.Context, jobID, snapshot string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET config_snapshot = ? WHERE id = ?`, snapshotOrEmpty(snapshot), jobID))
}

func (s *sqlStore) SetStaleReason(ctx context.Context, jobID, reason string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE job SET stale_reason = ?
		WHERE id = ? AND status = 'stale' AND is_current = 1 AND stale_reason IS NOT ?`, reason, jobID, reason)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---------- provider ----------

const providerCols = `id, name, type, base_url, api_key, max_concurrency, rpm, balance_api, price_input, price_cache_hit, price_output, currency, created_at, tariff, offpeak_only`

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func floatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func scanProvider(sc interface{ Scan(...any) error }) (model.Provider, error) {
	var p model.Provider
	var in, hit, out sql.NullFloat64
	var cur, tariff sql.NullString
	var offpeak int
	err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKey, &p.MaxConcurrency, &p.RPM, &p.BalanceAPI,
		&in, &hit, &out, &cur, &p.CreatedAt, &tariff, &offpeak)
	p.PriceInput, p.PriceCacheHit, p.PriceOutput, p.Currency = floatPtr(in), floatPtr(hit), floatPtr(out), strPtr(cur)
	p.Tariff, p.OffpeakOnly = strPtr(tariff), offpeak != 0
	return p, err
}

func (s *sqlStore) ListProviders(ctx context.Context) ([]model.Provider, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+providerCols+` FROM provider ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetProvider(ctx context.Context, id string) (model.Provider, error) {
	p, err := scanProvider(s.q.QueryRowContext(ctx, `SELECT `+providerCols+` FROM provider WHERE id = ?`, id))
	return p, notFound(err)
}

func (s *sqlStore) InsertProvider(ctx context.Context, p model.Provider) error {
	if p.CreatedAt == "" {
		p.CreatedAt = Now()
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO provider (`+providerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.BaseURL, p.APIKey, p.MaxConcurrency, p.RPM, p.BalanceAPI,
		nullFloat(p.PriceInput), nullFloat(p.PriceCacheHit), nullFloat(p.PriceOutput), nullStr(p.Currency), p.CreatedAt,
		nullStr(p.Tariff), boolInt(p.OffpeakOnly))
	return duplicate(err)
}

func (s *sqlStore) UpdateProvider(ctx context.Context, p model.Provider) error {
	res, err := s.q.ExecContext(ctx, `UPDATE provider SET name = ?, type = ?, base_url = ?, api_key = ?, max_concurrency = ?,
		rpm = ?, balance_api = ?, price_input = ?, price_cache_hit = ?, price_output = ?, currency = ?,
		tariff = ?, offpeak_only = ? WHERE id = ?`,
		p.Name, p.Type, p.BaseURL, p.APIKey, p.MaxConcurrency, p.RPM, p.BalanceAPI,
		nullFloat(p.PriceInput), nullFloat(p.PriceCacheHit), nullFloat(p.PriceOutput), nullStr(p.Currency),
		nullStr(p.Tariff), boolInt(p.OffpeakOnly), p.ID)
	return affectedOrNotFound(res, duplicate(err))
}

func (s *sqlStore) DeleteProvider(ctx context.Context, id string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `DELETE FROM provider WHERE id = ?`, id))
}

// ---------- role_setting ----------

func (s *sqlStore) ListRoleSettings(ctx context.Context) ([]model.RoleSetting, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT role, provider_id, model, params FROM role_setting ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RoleSetting
	for rows.Next() {
		var r model.RoleSetting
		var pid, m sql.NullString
		if err := rows.Scan(&r.Role, &pid, &m, &r.Params); err != nil {
			return nil, err
		}
		r.ProviderID, r.Model = strPtr(pid), strPtr(m)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqlStore) UpsertRoleSetting(ctx context.Context, r model.RoleSetting) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO role_setting (role, provider_id, model, params) VALUES (?,?,?,?)
		ON CONFLICT(role) DO UPDATE SET provider_id = excluded.provider_id, model = excluded.model, params = excluded.params`,
		r.Role, nullStr(r.ProviderID), nullStr(r.Model), snapshotOrEmpty(r.Params))
	return err
}

// ---------- usage_daily ----------

func (s *sqlStore) AddUsageDaily(ctx context.Context, u model.UsageDaily) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO usage_daily (provider_id, day, requests, prompt_tokens, cache_hit_tokens, completion_tokens, estimated_cost)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(provider_id, day) DO UPDATE SET
			requests = requests + excluded.requests,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			cache_hit_tokens = cache_hit_tokens + excluded.cache_hit_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens,
			estimated_cost = estimated_cost + excluded.estimated_cost`,
		u.ProviderID, u.Day, u.Requests, u.PromptTokens, u.CacheHitTokens, u.CompletionTokens, u.EstimatedCost)
	return err
}

func (s *sqlStore) ListUsageDaily(ctx context.Context, sinceDay string) ([]model.UsageDaily, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT provider_id, day, requests, prompt_tokens, cache_hit_tokens, completion_tokens, estimated_cost
		FROM usage_daily WHERE day >= ? ORDER BY day, provider_id`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.UsageDaily
	for rows.Next() {
		var u model.UsageDaily
		if err := rows.Scan(&u.ProviderID, &u.Day, &u.Requests, &u.PromptTokens, &u.CacheHitTokens, &u.CompletionTokens, &u.EstimatedCost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
