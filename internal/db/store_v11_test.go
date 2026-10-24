// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

func fp(v float64) *float64 { return &v }

func TestCourseCodeAndName(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := model.Course{ID: NewID(), Name: "Cryptography", Code: sp("CSC1003")}
	if err := st.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCourse(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code == nil || *got.Code != "CSC1003" {
		t.Fatalf("code = %v", got.Code)
	}
	if err := st.SetCourseCode(ctx, c.ID, sp("")); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCourseName(ctx, c.ID, "Crypto"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetCourse(ctx, c.ID)
	if got.Code != nil || got.Name != "Crypto" {
		t.Fatalf("after update: code=%v name=%q", got.Code, got.Name)
	}
	if err := st.SetCourseCode(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing course: %v", err)
	}
}

func TestLectureDraftSeqAndLaunch(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	if n, err := st.NextLectureSeq(ctx, c.ID); err != nil || n != 1 {
		t.Fatalf("NextLectureSeq empty = %d, %v", n, err)
	}
	l1 := mkLecture(t, st, c.ID)
	l2 := mkLecture(t, st, c.ID)
	if l1.Seq != 1 || l2.Seq != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", l1.Seq, l2.Seq)
	}
	if l1.LaunchedAt != nil {
		t.Fatal("new lecture must be a draft")
	}
	if err := st.SetLectureSeq(ctx, l1.ID, 7); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.NextLectureSeq(ctx, c.ID); n != 8 {
		t.Fatalf("NextLectureSeq = %d, want 8", n)
	}
	explicit := model.Lecture{ID: NewID(), CourseID: c.ID, Title: "x", MediaPath: "m", Seq: 3}
	if err := st.CreateLecture(ctx, explicit); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetLecture(ctx, explicit.ID); got.Seq != 3 {
		t.Fatalf("explicit seq = %d", got.Seq)
	}
	ok, err := st.SetLectureLaunched(ctx, l1.ID, "2026-10-23T10:00:00Z")
	if err != nil || !ok {
		t.Fatalf("launch: %v %v", ok, err)
	}
	ok, err = st.SetLectureLaunched(ctx, l1.ID, "2026-10-23T11:00:00Z")
	if err != nil || ok {
		t.Fatalf("second launch must report false: %v %v", ok, err)
	}
	got, _ := st.GetLecture(ctx, l1.ID)
	if got.LaunchedAt == nil || *got.LaunchedAt != "2026-10-23T10:00:00Z" || got.Seq != 7 {
		t.Fatalf("lecture = %+v", got)
	}
	lst, _ := st.ListLectures(ctx, c.ID)
	if len(lst) != 3 || lst[0].Seq != 7 {
		t.Fatalf("list = %+v", lst)
	}
	if _, err := st.SetLectureLaunched(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lecture: %v", err)
	}
}

func TestJobSnapshotAndStaleReason(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	jobs, _ := st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
	for _, j := range jobs {
		if j.ConfigSnapshot != model.EmptySnapshot || j.StaleReason != nil {
			t.Fatalf("new job snapshot=%q reason=%v", j.ConfigSnapshot, j.StaleReason)
		}
	}
	tr, _ := st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, model.StageTranslate)
	snap := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "p1", ProviderName: "DeepSeek", Model: "deepseek-chat"})
	if err := st.SetJobSnapshot(ctx, tr.ID, snap); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetJob(ctx, tr.ID)
	if got.ConfigSnapshot != snap {
		t.Fatalf("snapshot = %q", got.ConfigSnapshot)
	}
	if err := st.SetJobSnapshot(ctx, tr.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetJob(ctx, tr.ID); got.ConfigSnapshot != model.EmptySnapshot {
		t.Fatalf("empty snapshot stored as %q", got.ConfigSnapshot)
	}

	// InsertJob carries snapshot and reason; CAS writes and clears the reason.
	j := model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: "other", Stage: model.StageRepair,
		Status: model.StatusDone, IsCurrent: true, ConfigSnapshot: snap}
	if err := st.InsertJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	reason := "术语表已更新（v3 → v4）"
	ok, err := st.CASJobStatus(ctx, j.ID, model.StatusDone, model.StatusStale, JobFinish{StaleReason: &reason})
	if err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	got, _ = st.GetCurrentJob(ctx, model.ScopeLecture, "other", model.StageRepair)
	if got.StaleReason == nil || *got.StaleReason != reason || got.ConfigSnapshot != snap {
		t.Fatalf("after cas: %+v", got)
	}
	byStatus, _ := st.ListCurrentJobsByStatus(ctx, model.StatusStale)
	if len(byStatus) != 1 || byStatus[0].StaleReason == nil {
		t.Fatalf("by status = %+v", byStatus)
	}

	// SetStaleReason: only a stale job, only a different reason, status untouched.
	newer := "术语表已更新（v3 → v6）"
	if ok, err := st.SetStaleReason(ctx, j.ID, newer); err != nil || !ok {
		t.Fatalf("set reason: %v %v", ok, err)
	}
	if ok, _ := st.SetStaleReason(ctx, j.ID, newer); ok {
		t.Fatal("same reason must report false")
	}
	if got, _ = st.GetJob(ctx, j.ID); got.Status != model.StatusStale || *got.StaleReason != newer {
		t.Fatalf("after set reason: %+v", got)
	}
	if ok, _ := st.SetStaleReason(ctx, tr.ID, newer); ok { // a pending job
		t.Fatal("non-stale job must not get a reason")
	}
	// a stale job replaced by a rerun (is_current = 0) is left alone
	old := model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: "other", Stage: model.StageTranslate,
		Status: model.StatusStale, IsCurrent: false, StaleReason: &reason}
	if err := st.InsertJob(ctx, old); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.SetStaleReason(ctx, old.ID, newer); ok {
		t.Fatal("a job that is no longer current must not get a reason")
	}
}

func TestProviderCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	p := model.Provider{ID: NewID(), Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
		APIKey: "sk-test-0000", MaxConcurrency: 8, RPM: 300, BalanceAPI: model.BalanceDeepSeek,
		PriceInput: fp(2), PriceCacheHit: fp(0.5), PriceOutput: fp(8), Currency: sp("CNY")}
	if err := st.InsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt == "" {
		t.Fatal("created_at not filled")
	}
	p.CreatedAt = got.CreatedAt
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("got %+v\nwant %+v", got, p)
	}
	dup := p
	dup.ID = NewID()
	if err := st.InsertProvider(ctx, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate name: %v", err)
	}
	a := model.Provider{ID: NewID(), Name: "Anthropic", Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com",
		APIKey: "k", MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone}
	if err := st.InsertProvider(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Name = "DeepSeek"
	if err := st.UpdateProvider(ctx, a); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("rename to existing: %v", err)
	}
	a.Name, a.RPM, a.PriceInput = "Claude", 50, fp(3)
	if err := st.UpdateProvider(ctx, a); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListProviders(ctx)
	if len(list) != 2 || list[1].Name != "Claude" || list[1].RPM != 50 || list[1].PriceInput == nil || list[1].PriceOutput != nil {
		t.Fatalf("list = %+v", list)
	}
	// role_setting references block deletion (FK).
	if err := st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleTranslate, ProviderID: &a.ID, Model: sp("claude-x")}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProvider(ctx, a.ID); err == nil {
		t.Fatal("delete of a provider used by a role must fail")
	}
	if err := st.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetProvider(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted provider: %v", err)
	}
	if err := st.DeleteProvider(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	if err := st.UpdateProvider(ctx, model.Provider{ID: "nope", Name: "n", Type: model.ProviderOpenAI, BalanceAPI: model.BalanceNone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestRoleSettings(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	p := model.Provider{ID: "deepseek", Name: "deepseek", Type: model.ProviderOpenAI, BaseURL: "u", APIKey: "", MaxConcurrency: 1, RPM: 1, BalanceAPI: model.BalanceNone}
	if err := st.InsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleSummary, ProviderID: &p.ID, Model: sp("m1")}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleSummary, ProviderID: &p.ID, Model: sp("m2"), Params: `{"temperature":0.5}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRoleSetting(ctx, model.RoleSetting{Role: model.RoleSyllabusFormat}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.ListRoleSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.RoleSetting{
		{Role: model.RoleSummary, ProviderID: &p.ID, Model: sp("m2"), Params: `{"temperature":0.5}`},
		{Role: model.RoleSyllabusFormat, Params: "{}"},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("roles = %+v", rs)
	}
	if err := st.UpsertRoleSetting(ctx, model.RoleSetting{Role: "bogus"}); err == nil {
		t.Fatal("unknown role must be rejected by the CHECK constraint")
	}
}

func TestUsageDaily(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	add := func(p, day string, r, pt, h, c int64) {
		t.Helper()
		if err := st.AddUsageDaily(ctx, model.UsageDaily{ProviderID: p, Day: day, Requests: r, PromptTokens: pt, CacheHitTokens: h, CompletionTokens: c}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "2026-10-22", 1, 100, 50, 10)
	add("a", "2026-10-23", 1, 100, 80, 10)
	add("a", "2026-10-23", 2, 300, 200, 30)
	add("b", "2026-10-23", 1, 10, 0, 1)
	all, err := st.ListUsageDaily(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	today, _ := st.ListUsageDaily(ctx, "2026-10-23")
	want := []model.UsageDaily{
		{ProviderID: "a", Day: "2026-10-23", Requests: 3, PromptTokens: 400, CacheHitTokens: 280, CompletionTokens: 40},
		{ProviderID: "b", Day: "2026-10-23", Requests: 1, PromptTokens: 10, CacheHitTokens: 0, CompletionTokens: 1},
	}
	if !reflect.DeepEqual(today, want) {
		t.Fatalf("today = %+v", today)
	}
}

func TestMigrationBackfillsV1Lectures(t *testing.T) {
	// 004 backfills launched_at and seq for rows that predate 003; on a
	// fresh database new lectures stay drafts (covered above). Here the
	// backfill is replayed on rows inserted with NULLs.
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	s := st.(*sqlStore)
	for i, id := range []string{"L2", "L1"} {
		ts := []string{"2026-10-02T00:00:00Z", "2026-10-01T00:00:00Z"}[i]
		if _, err := s.q.ExecContext(ctx, `INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES (?,?,?,?,?)`,
			id, c.ID, id, "m", ts); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrationFS.ReadFile("migrations/004_v1_1_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.q.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	l1, _ := st.GetLecture(ctx, "L1")
	l2, _ := st.GetLecture(ctx, "L2")
	if l1.Seq != 1 || l2.Seq != 2 || l1.LaunchedAt == nil || *l1.LaunchedAt != l1.CreatedAt {
		t.Fatalf("l1=%+v l2=%+v", l1, l2)
	}
}
