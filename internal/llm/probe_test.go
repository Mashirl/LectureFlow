// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

func fakeProvider(s *llmtest.Server, typ string) model.Provider {
	return model.Provider{ID: "p1", Name: "Fake", Type: typ, BaseURL: s.URL, APIKey: llmtest.TestKey}
}

func TestTestConnection(t *testing.T) {
	s := llmtest.New(t)
	for _, typ := range []string{model.ProviderOpenAI, model.ProviderAnthropic} {
		s.Reset()
		r := TestConnection(context.Background(), fakeProvider(s, typ), "some-model")
		if !r.OK || r.Error != "" || r.Latency <= 0 {
			t.Fatalf("%s: %+v", typ, r)
		}
		reqs := s.Requests(llmtest.RolePing)
		if len(reqs) != 1 || reqs[0].Model != "some-model" || reqs[0].API != typ || reqs[0].Key != llmtest.TestKey {
			t.Fatalf("%s: ping requests %+v", typ, reqs)
		}
		if typ == model.ProviderAnthropic && (reqs[0].MaxTokens != 1 || reqs[0].Version != AnthropicVersion) {
			t.Fatalf("anthropic ping %+v", reqs[0])
		}
	}

	// empty model name: the first probed model is used
	s.Reset()
	s.SetModels([]string{"zeta", "alpha"})
	if r := TestConnection(context.Background(), fakeProvider(s, model.ProviderOpenAI), ""); !r.OK {
		t.Fatalf("probe fallback: %+v", r)
	}
	if reqs := s.Requests(llmtest.RolePing); len(reqs) != 1 || reqs[0].Model != "alpha" || s.ModelListCalls() != 1 {
		t.Fatalf("probe fallback requests %+v", reqs)
	}
	s.SetModels(nil)
	if r := TestConnection(context.Background(), fakeProvider(s, model.ProviderOpenAI), ""); r.OK || testCode(r) != CodeProbeNoModel {
		t.Fatalf("no model at all: %+v", r)
	}

	// the server's error text, verbatim
	s.SetError(http.StatusNotFound, "Model Not Exist")
	r := TestConnection(context.Background(), fakeProvider(s, model.ProviderOpenAI), "nope")
	if r.OK || r.Error != `HTTP 404：{"error":{"message":"Model Not Exist","type":"llmtest"}}` || testCode(r) != CodeProbeHTTP ||
		r.ErrorInfo.Params["status"] != 404 || r.ErrorInfo.Detail != r.Error {
		t.Fatalf("404: %+v", r)
	}
	s.SetError(0, "")

	// a wrong key echoed by the server never reaches the caller
	s.RequireKey("sk-the-right-key-0000")
	wrong := fakeProvider(s, model.ProviderAnthropic)
	wrong.APIKey = "sk-wrong-SECRET-key-9876"
	r = TestConnection(context.Background(), wrong, "m")
	if r.OK || testCode(r) != CodeProbeHTTP || strings.Contains(r.Error, "SECRET") || !strings.Contains(r.Error, "sk-…9876") {
		t.Fatalf("401: %+v", r)
	}
	if _, err := ProbeModels(context.Background(), wrong); err == nil || strings.Contains(err.Error(), "SECRET") || !isCode(err, CodeProbeHTTP, "status", 401) ||
		strings.Contains(infoOf(err).Detail, "SECRET") {
		t.Fatalf("probe 401: %v", err)
	}
	s.RequireKey("")

	// no key (not a local server): not sent at all
	nokey := model.Provider{Name: "NoKey", Type: model.ProviderOpenAI, BaseURL: "https://api.example.invalid"}
	if r := TestConnection(context.Background(), nokey, "m"); r.OK || testCode(r) != CodeProbeNoAPIKey || r.ErrorInfo.Params["provider"] != "NoKey" {
		t.Fatalf("no key: %+v", r)
	}
	// a local server without key is fine
	local := fakeProvider(s, model.ProviderOpenAI)
	local.APIKey = ""
	if r := TestConnection(context.Background(), local, "qwen"); !r.OK {
		t.Fatalf("local: %+v", r)
	}

	// timeout
	old := ProbeTimeout
	ProbeTimeout = 50 * time.Millisecond
	s.SetDelay(2 * time.Second)
	start := time.Now()
	r = TestConnection(context.Background(), fakeProvider(s, model.ProviderOpenAI), "m")
	ProbeTimeout = old
	s.SetDelay(0)
	if r.OK || r.Error == "" || testCode(r) != CodeProbeTimeout || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout: %+v after %v", r, time.Since(start))
	}

	// nothing listening
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if r := TestConnection(context.Background(), model.Provider{Name: "Dead", Type: model.ProviderOpenAI, BaseURL: dead.URL, APIKey: "k"}, "m"); r.OK || r.Error == "" || testCode(r) != CodeProbeNetwork {
		t.Fatalf("dead: %+v", r)
	}
	// 200 but not an API (e.g. a web page)
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>hi</html>") }))
	defer html.Close()
	if r := TestConnection(context.Background(), model.Provider{Name: "Web", Type: model.ProviderOpenAI, BaseURL: html.URL, APIKey: "k"}, "m"); r.OK || testCode(r) != CodeProbeNotJSON {
		t.Fatalf("html: %+v", r)
	}
	if s.Count(llmtest.RolePing) == 0 {
		t.Fatal("no ping recorded")
	}
}

func TestProbeModels(t *testing.T) {
	s := llmtest.New(t)
	s.SetModels([]string{"deepseek-reasoner", "deepseek-chat", "deepseek-chat", " "})
	got, err := ProbeModels(context.Background(), fakeProvider(s, model.ProviderOpenAI))
	if err != nil || !slices.Equal(got, []string{"deepseek-chat", "deepseek-reasoner"}) {
		t.Fatalf("openai: %v %v", got, err)
	}

	// Anthropic: GET /v1/models, paginated with limit/after_id
	var many []string
	for i := 0; i < 2345; i++ {
		many = append(many, fmt.Sprintf("claude-%04d", i))
	}
	s.SetModels(many)
	s.Reset()
	got, err = ProbeModels(context.Background(), fakeProvider(s, model.ProviderAnthropic))
	if err != nil || len(got) != 2345 || got[0] != "claude-0000" || got[2344] != "claude-2344" {
		t.Fatalf("anthropic: %d models, err %v", len(got), err)
	}
	if s.ModelListCalls() != 3 {
		t.Fatalf("pages = %d, want 3 (limit 1000)", s.ModelListCalls())
	}

	// headers and paths as the real APIs expect
	var paths, keys, versions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"))
		versions = append(versions, r.Header.Get("anthropic-version"))
		fmt.Fprint(w, `{"data":[{"id":"m1"}],"has_more":false}`)
	}))
	defer srv.Close()
	ProbeModels(context.Background(), model.Provider{Type: model.ProviderOpenAI, BaseURL: srv.URL + "/v1/", APIKey: "k1"})
	ProbeModels(context.Background(), model.Provider{Type: model.ProviderAnthropic, BaseURL: srv.URL, APIKey: "k2"})
	if !slices.Equal(paths, []string{"/v1/models", "/v1/models"}) || keys[0] != "|Bearer k1" || keys[1] != "k2|" ||
		versions[0] != "" || versions[1] != AnthropicVersion {
		t.Fatalf("paths %v keys %v versions %v", paths, keys, versions)
	}

	// errors: the server's text
	s.SetError(http.StatusForbidden, "no access")
	if _, err := ProbeModels(context.Background(), fakeProvider(s, model.ProviderAnthropic)); err == nil ||
		err.Error() != `HTTP 403：{"error":{"message":"no access","type":"permission_error"},"type":"error"}` || !isCode(err, CodeProbeHTTP, "status", 403) {
		t.Fatalf("403: %v", err)
	}
	s.SetError(0, "")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "nope") }))
	defer bad.Close()
	if _, err := ProbeModels(context.Background(), model.Provider{Type: model.ProviderOpenAI, BaseURL: bad.URL}); err == nil || !isCode(err, CodeProbeBadModelList) {
		t.Fatalf("unparsable: %v", err)
	}
	if _, err := ProbeModels(context.Background(), model.Provider{Type: model.ProviderOpenAI, BaseURL: "::bad"}); err == nil {
		t.Fatal("bad base_url")
	}
}
