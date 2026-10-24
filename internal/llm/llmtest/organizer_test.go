// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// organizerClient is a legacy-mode client whose chapters and organizer roles
// use the summary role's provider and model.
func organizerClient(t *testing.T, s *llmtest.Server) *llm.HTTPClient {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	s.Configure(cfg)
	for _, r := range []string{model.RoleChapters, model.RoleOrganizer} {
		cfg.LLM.Roles[r] = cfg.LLM.Roles[config.RoleSummary]
	}
	c := llm.New(cfg, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	return c
}

// TestOrganizerRoles: the P7 prompt is the chapters role; the two organizer
// prompts (P13.5, P13.3.2) are told apart by their first lines and answered
// from title.json and filename_rule.json, counted separately.
func TestOrganizerRoles(t *testing.T) {
	s := llmtest.New(t)
	c := organizerClient(t, s)
	ctx := context.Background()
	reqs := []struct {
		llmRole, fakeRole string
		req               ctxbuild.Request
		fixture           string
	}{
		{model.RoleChapters, llmtest.RoleChapters, ctxbuild.ChaptersRequest("s", []ctxbuild.Sentence{{Idx: 0, Text: "a"}}), "chapters.json"},
		{model.RoleOrganizer, llmtest.RoleTitle, ctxbuild.TitleRequest(ctxbuild.TitleInput{Summary: "s"}), "title.json"},
		{model.RoleOrganizer, llmtest.RoleFilenameRule, ctxbuild.FilenameRuleRequest(ctxbuild.FilenameRuleInput{Samples: []ctxbuild.FilenameSample{{Filename: "a.mp4", UploadedAt: "2026-10-20 09:00"}}}), "filename_rule.json"},
	}
	for _, r := range reqs {
		if got := llmtest.RoleOf(r.req.System); got != r.fakeRole {
			t.Fatalf("RoleOf = %q, want %q", got, r.fakeRole)
		}
		res, err := c.Complete(ctx, r.llmRole, r.req.System, r.req.User, true)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(llmtest.FixtureDir(), r.fixture))
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, res.Text, string(want)) {
			t.Errorf("%s answer %q, want %s", r.fakeRole, res.Text, r.fixture)
		}
		if n := s.Count(r.fakeRole); n != 1 {
			t.Errorf("Count(%s) = %d", r.fakeRole, n)
		}
	}
}

// TestOrganizerDefaults: without fixture files the built-in answers are
// used, and they are valid JSON.
func TestOrganizerDefaults(t *testing.T) {
	dir := t.TempDir()
	for _, role := range llmtest.Roles {
		b, err := os.ReadFile(filepath.Join(llmtest.FixtureDir(), role+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, role+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := llmtest.NewFromDir(t, dir)
	c := organizerClient(t, s)
	for role, def := range map[string]string{
		llmtest.RoleChapters:     llmtest.DefaultChaptersAnswer,
		llmtest.RoleTitle:        llmtest.DefaultTitleAnswer,
		llmtest.RoleFilenameRule: llmtest.DefaultFilenameRuleAnswer,
	} {
		if !json.Valid([]byte(def)) {
			t.Errorf("default %s answer is not JSON", role)
		}
		var req ctxbuild.Request
		llmRole := model.RoleOrganizer
		switch role {
		case llmtest.RoleChapters:
			req, llmRole = ctxbuild.ChaptersRequest("s", nil), model.RoleChapters
		case llmtest.RoleTitle:
			req = ctxbuild.TitleRequest(ctxbuild.TitleInput{Summary: "s"})
		default:
			req = ctxbuild.FilenameRuleRequest(ctxbuild.FilenameRuleInput{})
		}
		res, err := c.Complete(context.Background(), llmRole, req.System, req.User, true)
		if err != nil || res.Text != def {
			t.Errorf("%s: %q %v", role, res.Text, err)
		}
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
