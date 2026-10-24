// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// kvUsageMigrated marks the one-time import of v1 token usage into usage_daily.
const kvUsageMigrated = "v1_1.usage_migrated"

// migrateV1Usage fills usage_daily (SPEC-v1.1 A5.1, A10.2 "累计") from the
// token usage v1 recorded in job.stats, once, so the resource watcher's
// totals include the lectures processed before the upgrade (lead decision,
// NOTES.md). It only runs while usage_daily is still empty: once v1.1 has
// recorded a request, the per-request ledger is the only source. A job's
// provider is its snapshot's provider; v1 jobs (no snapshot) belong to the
// single provider imported from config.toml, and are skipped when there is
// more than one. Days are local dates of finished_at.
func migrateV1Usage(ctx context.Context, st db.Store, set *settings.Service) (int, error) {
	if _, done, err := st.GetKV(ctx, kvUsageMigrated); err != nil || done {
		return 0, err
	}
	rows, err := st.ListUsageDaily(ctx, "")
	if err != nil {
		return 0, err
	}
	if len(rows) > 0 {
		return 0, st.SetKV(ctx, kvUsageMigrated, "1")
	}
	jobs, err := st.ListJobsFinishedSince(ctx, "")
	if err != nil {
		return 0, err
	}
	fallback := ""
	if ps := set.Providers(); len(ps) == 1 {
		fallback = ps[0].ID
	}
	sums := map[[2]string]*model.UsageDaily{}
	for _, j := range jobs {
		s := j.Stats
		if s.PromptTokens == 0 && s.CompletionTokens == 0 || j.FinishedAt == nil {
			continue
		}
		pid := fallback
		if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok {
			pid = snap.ProviderID
		}
		t, err := time.Parse(time.RFC3339, *j.FinishedAt)
		if pid == "" || err != nil {
			continue
		}
		key := [2]string{pid, t.In(time.Local).Format("2006-01-02")}
		u := sums[key]
		if u == nil {
			u = &model.UsageDaily{ProviderID: key[0], Day: key[1]}
			sums[key] = u
		}
		u.Requests += s.Requests
		u.PromptTokens += s.PromptTokens
		u.CacheHitTokens += s.CacheHitTokens
		u.CompletionTokens += s.CompletionTokens
	}
	err = st.Tx(ctx, func(tx db.Store) error {
		for _, u := range sums {
			if err := tx.AddUsageDaily(ctx, *u); err != nil {
				return err
			}
		}
		return tx.SetKV(ctx, kvUsageMigrated, "1")
	})
	if err != nil {
		return 0, err
	}
	return len(sums), nil
}
