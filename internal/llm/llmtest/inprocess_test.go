// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
)

// TestTransportInProcess (perf-llm): Transport answers like the HTTP server
// without any connection (the listener is closed first), injected failures
// included; in a synctest bubble the answer delay runs on the fake clock,
// and a request cancelled while it waits gets an error at once.
func TestTransportInProcess(t *testing.T) {
	s := llmtest.New(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	s.Configure(cfg)
	s.Close() // nothing may go over the network
	client := func() *llm.HTTPClient {
		c := llm.New(cfg, nil, "", &http.Client{Transport: s.Transport()})
		c.Backoff = []time.Duration{time.Millisecond}
		return c
	}
	c := client()

	s.SetTranslation(5, "覆盖的译文。")
	res, err := translateOnce(t, c, 5)
	if err != nil || !strings.Contains(res.Text, "覆盖的译文") || !strings.Contains(res.Text, `"idx":5`) || res.PromptTokens == 0 {
		t.Fatalf("in-process answer: %+v %v", res, err)
	}
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailHTTP500)
	if _, err := translateOnce(t, c, 6); err != nil {
		t.Fatalf("the retry after an injected HTTP 500 should succeed: %v", err)
	}

	synctest.Test(t, func(t *testing.T) {
		c := client()
		s.SetDelay(1200 * time.Millisecond)
		start := time.Now()
		if _, err := translateOnce(t, c, 7); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 1200*time.Millisecond {
			t.Errorf("answer after %v on the fake clock, want the 1.2 s delay", d)
		}

		s.SetDelay(time.Hour)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		req := ctxbuild.BuildTranslateRequest(ctxbuild.LectureSnapshot{Sentences: sample(t)}, 8, 3)
		start = time.Now()
		if _, err := c.Complete(ctx, config.RoleTranslate, req.System, req.User, true); err == nil {
			t.Error("a request cancelled during the delay succeeded")
		}
		if d := time.Since(start); d >= time.Hour {
			t.Errorf("the cancelled request waited %v", d)
		}
	})
}
