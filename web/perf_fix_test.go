// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// perf-fix (web-builder, 2026-10-27): api.js reports whether the page is
// shown (internal/api/visibility.go) without closing its event stream, and
// shares only simultaneous reads of the three load-time endpoints.
func TestAPIReportsVisibility(t *testing.T) {
	src := readWeb(t, "api.js")
	for _, want := range []string{
		// the stream says it when connecting, a change is posted once open
		"/api/events?client=${encodeURIComponent(clientId)}&visible=${reported ? 1 : 0}&seq=${++visSeq}",
		"fetch(withMock('/api/events/visibility')",
		"body: JSON.stringify({ client: clientId, visible, seq })",
		"document.addEventListener('visibilitychange', report);",
		"window.addEventListener('lectureflow:window-visibility'",
		"return document.visibilityState !== 'hidden' && shellVisible;",
		"report(); // shown or hidden while connecting",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("api.js lacks %q", want)
		}
	}
	// A hidden page keeps its stream: nothing closes it on visibility.
	conn := src[strings.Index(src, "export function connectEvents("):]
	conn = conn[:strings.Index(conn, "\n}\n")]
	report := conn[strings.Index(conn, "const report = () => {"):]
	report = report[:strings.Index(report, "};")]
	if strings.Contains(report, "close()") || strings.Contains(report, "open()") {
		t.Errorf("the visibility report reconnects the stream:\n%s", report)
	}
}

func TestAPISharesOnlyLoadReads(t *testing.T) {
	src := readWeb(t, "api.js")
	m := regexp.MustCompile(`const SHARED_GETS = new Set\(\[([^\]]*)\]\);`).FindStringSubmatch(src)
	if m == nil || strings.TrimSpace(m[1]) != `'/api/settings', '/api/system', '/api/app'` {
		t.Fatalf("SHARED_GETS %q", m)
	}
	for _, want := range []string{
		"if (write) forgetShared();",                      // before and after every write
		"forgetShared(); // something changed",            // every server event
		"!write && !opts.signal && SHARED_GETS.has(path)", // never with a signal
		"performance.now() - e.at < SHARE_MS",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("api.js lacks %q", want)
		}
	}
}
