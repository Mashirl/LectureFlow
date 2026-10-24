// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

func TestMigrateV1Usage(t *testing.T) {
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	llmtest.New(t).Configure(cfg) // never the real API
	set, err := settings.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	day := func(ts string) string {
		tm, _ := time.Parse(time.RFC3339, ts)
		return tm.In(time.Local).Format("2006-01-02")
	}
	t1, t2 := "2026-10-22T10:00:00Z", "2026-10-23T09:00:00Z"
	for _, j := range []model.Job{
		{Stage: model.StageSummary, FinishedAt: &t1, Stats: model.JobStats{Requests: 1, PromptTokens: 100, CacheHitTokens: 40, CompletionTokens: 10}},
		{Stage: model.StageRepair, FinishedAt: &t1, Stats: model.JobStats{Requests: 2, PromptTokens: 200, CacheHitTokens: 100, CompletionTokens: 20}},
		{Stage: model.StageTranslate, FinishedAt: &t2, Stats: model.JobStats{Requests: 5, PromptTokens: 500, CacheHitTokens: 400, CompletionTokens: 50}},
		{Stage: model.StageASR, FinishedAt: &t2},                                             // no tokens
		{Stage: model.StageTranslate, Stats: model.JobStats{Requests: 9, PromptTokens: 900}}, // not finished
		{Stage: model.StageTranslate, FinishedAt: &t2, ConfigSnapshot: `{"provider_id":"other","model":"m"}`, Stats: model.JobStats{Requests: 1, PromptTokens: 7, CompletionTokens: 1}},
	} {
		j.ID, j.Scope, j.ScopeID, j.Status, j.IsCurrent = db.NewID(), model.ScopeLecture, db.NewID(), model.StatusDone, true
		if err := st.InsertJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	n, err := migrateV1Usage(ctx, st, set)
	if err != nil || n != 3 {
		t.Fatalf("migrate = %d, %v; want 3 rows", n, err)
	}
	rows, _ := st.ListUsageDaily(ctx, "")
	got := map[[2]string]model.UsageDaily{}
	for _, r := range rows {
		got[[2]string{r.ProviderID, r.Day}] = r
	}
	if r := got[[2]string{"deepseek", day(t1)}]; r.Requests != 3 || r.PromptTokens != 300 || r.CacheHitTokens != 140 || r.CompletionTokens != 30 {
		t.Fatalf("day 1 = %+v", r)
	}
	if r := got[[2]string{"deepseek", day(t2)}]; r.Requests != 5 || r.PromptTokens != 500 {
		t.Fatalf("day 2 = %+v", r)
	}
	if r := got[[2]string{"other", day(t2)}]; r.PromptTokens != 7 {
		t.Fatalf("snapshot provider = %+v", r)
	}
	// Once only.
	if n, err := migrateV1Usage(ctx, st, set); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v", n, err)
	}
	if rows2, _ := st.ListUsageDaily(ctx, ""); len(rows2) != len(rows) || rows2[0] != rows[0] {
		t.Fatal("second run changed usage_daily")
	}
}

func TestMigrateV1UsageSkipsWhenLedgerHasData(t *testing.T) {
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	llmtest.New(t).Configure(cfg)
	set, err := settings.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddUsageDaily(ctx, model.UsageDaily{ProviderID: "deepseek", Day: "2026-10-23", Requests: 1, PromptTokens: 1}); err != nil {
		t.Fatal(err)
	}
	ts := "2026-10-22T10:00:00Z"
	if err := st.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: "x", Stage: model.StageSummary,
		Status: model.StatusDone, IsCurrent: true, FinishedAt: &ts, Stats: model.JobStats{PromptTokens: 100}}); err != nil {
		t.Fatal(err)
	}
	if n, err := migrateV1Usage(ctx, st, set); err != nil || n != 0 {
		t.Fatalf("migrate = %d, %v; want 0 (usage_daily already used by v1.1)", n, err)
	}
	if rows, _ := st.ListUsageDaily(ctx, ""); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}
