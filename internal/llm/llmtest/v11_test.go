// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// settingsClient opens a settings service (no legacy import), points every
// role at the fake server through ConfigureSettings and returns a v1.1 client.
func settingsClient(t *testing.T, s *llmtest.Server, typ string) (*llm.HTTPClient, *settings.Service, model.Provider) {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(context.Background(), model.KVSettingsImported, "1")
	set, err := settings.Open(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Config().DataDir = t.TempDir()
	p, err := s.ConfigureSettings(context.Background(), set, "Fake", typ, "fake-model")
	if err != nil {
		t.Fatal(err)
	}
	c := llm.NewWithSettings(set, nil, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	return c, set, p
}

func TestAnthropicEndpoint(t *testing.T) {
	s := llmtest.New(t)
	c, set, p := settingsClient(t, s, model.ProviderAnthropic)
	if len(set.Roles()) != len(model.Roles) || *set.Roles()[model.RoleTranslate].ProviderID != p.ID {
		t.Fatalf("ConfigureSettings roles %+v", set.Roles())
	}
	lec := ctxbuild.LectureSnapshot{LectureID: "L", Sentences: []ctxbuild.Sentence{{Idx: 0, Text: "A cache line.", ASRText: "A cache line."}}}
	sum := ctxbuild.SummaryRequest(lec)
	r1, err := c.Complete(context.Background(), model.RoleSummary, sum.System, sum.User, false)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Complete(context.Background(), model.RoleSummary, sum.System, sum.User, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(r1.Text)) < 50 || r1.Text != r2.Text {
		t.Fatalf("summary text %q", r1.Text)
	}
	// first request writes the cache (prompt includes the creation), the second reads it
	if r1.CacheHitTokens != 0 || r2.CacheHitTokens == 0 || r1.PromptTokens != r2.PromptTokens || r2.CacheHitTokens >= r2.PromptTokens {
		t.Fatalf("usage r1 %+v r2 %+v", r1, r2)
	}
	reqs := s.Requests(llmtest.RoleSummary)
	if len(reqs) != 2 || reqs[0].API != model.ProviderAnthropic || !reqs[0].CacheControl || reqs[0].HasTemp ||
		reqs[0].Version != llm.AnthropicVersion || reqs[0].Key != llmtest.TestKey || reqs[0].Auth != "" ||
		reqs[0].MaxTokens != llm.DefaultAnthropicMaxTokens || reqs[0].JSONMode || reqs[0].Model != "fake-model" {
		t.Fatalf("anthropic request %+v", reqs[0])
	}

	// JSON mode: instruction appended, recognised and stripped; translate parses
	tr := ctxbuild.BuildTranslateRequest(lec, 0, 3)
	res, err := c.Complete(context.Background(), model.RoleTranslate, tr.System, tr.User, true)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	}
	if err := llm.ParseJSON(res.Text, &out); err != nil || out.Idx != 0 || out.Zh == "" {
		t.Fatalf("translate %q: %v", res.Text, err)
	}
	treq := s.Requests(llmtest.RoleTranslate)[0]
	if !treq.JSONMode || treq.User != tr.User || treq.RawUser != tr.User+llm.JSONInstruction || treq.TargetIdx != 0 {
		t.Fatalf("translate request json=%v user=%q", treq.JSONMode, treq.RawUser)
	}

	// 529 is retried
	s.FailNext(llmtest.RoleSummary, 2, llmtest.FailHTTP529)
	if _, err := c.Complete(context.Background(), model.RoleSummary, sum.System, sum.User, false); err != nil {
		t.Fatalf("after 529s: %v", err)
	}
	if s.Count(llmtest.RoleSummary) != 5 {
		t.Fatalf("summary count %d", s.Count(llmtest.RoleSummary))
	}
}

func TestSyllabusFormatRole(t *testing.T) {
	s := llmtest.New(t)
	c, _, _ := settingsClient(t, s, model.ProviderOpenAI)
	res, err := c.Complete(context.Background(), model.RoleSyllabusFormat, ctxbuild.SyllabusFormatSystem, "RAW SYLLABUS:\nweek 1 caches", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != llmtest.DefaultSyllabusMarkdown || s.Count(llmtest.RoleSyllabusFormat) != 1 {
		t.Fatalf("syllabus_format: %q", res.Text)
	}
	s.SetResponse(llmtest.RoleSyllabusFormat, "# X\n")
	if res, _ := c.Complete(context.Background(), model.RoleSyllabusFormat, ctxbuild.SyllabusFormatSystem, "RAW SYLLABUS:\n", false); res.Text != "# X\n" {
		t.Fatalf("override: %q", res.Text)
	}
}

func TestProbeAndPingAgainstFakeServer(t *testing.T) {
	s := llmtest.New(t)
	for _, typ := range []string{model.ProviderOpenAI, model.ProviderAnthropic} {
		p := model.Provider{Name: "Fake", Type: typ, BaseURL: s.URL, APIKey: llmtest.TestKey}
		models, err := llm.ProbeModels(context.Background(), p)
		if err != nil || strings.Join(models, ",") != strings.Join(llmtest.DefaultModels, ",") {
			t.Fatalf("%s probe: %v %v", typ, models, err)
		}
		if r := llm.TestConnection(context.Background(), p, ""); !r.OK {
			t.Fatalf("%s test: %+v", typ, r)
		}
	}
	if s.Count(llmtest.RolePing) != 2 || s.ModelListCalls() != 4 {
		t.Fatalf("pings %d model lists %d", s.Count(llmtest.RolePing), s.ModelListCalls())
	}
	// FailNext works for the ping role too
	s.FailNext(llmtest.RolePing, 1, llmtest.FailHTTP401)
	p := model.Provider{Name: "Fake", Type: model.ProviderOpenAI, BaseURL: s.URL, APIKey: llmtest.TestKey}
	if r := llm.TestConnection(context.Background(), p, "m"); r.OK || !strings.Contains(r.Error, "HTTP 401") || !strings.Contains(r.Error, "injected authentication failure") {
		t.Fatalf("injected 401: %+v", r)
	}
	// SetError answers every endpoint in the provider's format
	s.SetError(http.StatusServiceUnavailable, "maintenance")
	pa := model.Provider{Name: "Fake", Type: model.ProviderAnthropic, BaseURL: s.URL, APIKey: llmtest.TestKey}
	if _, err := llm.ProbeModels(context.Background(), pa); err == nil || !strings.Contains(err.Error(), `"type":"api_error"`) || !strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("SetError: %v", err)
	}
}
