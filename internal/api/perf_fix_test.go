// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

// perf-fix: visible front ends (visibility.go), static ETags
// (static_etag.go). The batch counts are in lecture_counts_test.go.

func TestVisibilityOfQuery(t *testing.T) {
	for _, c := range []struct {
		query string
		want  clientVisibility
	}{
		{"", clientVisibility{visible: true}},
		{"?client=abc-1_2&visible=1&seq=3", clientVisibility{id: "abc-1_2", visible: true, seq: 3}},
		{"?client=abc&visible=0&seq=7", clientVisibility{id: "abc", visible: false, seq: 7}},
		{"?client=abc&visible=false", clientVisibility{id: "abc", visible: false}},
		{"?visible=yes&seq=-4", clientVisibility{visible: true}},
		{"?client=a%20b&visible=0", clientVisibility{visible: false}},           // invalid id dropped
		{"?client=" + strings.Repeat("x", 65), clientVisibility{visible: true}}, // too long
		{"?client=" + strings.Repeat("x", 64), clientVisibility{id: strings.Repeat("x", 64), visible: true}},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/events"+c.query, nil)
		if got := visibilityOf(r); got != c.want {
			t.Errorf("%q: %+v, want %+v", c.query, got, c.want)
		}
	}
}

// countingSMI is the fake nvidia-smi of the resource watcher, counting runs.
type countingSMI struct{ n atomic.Int32 }

func (c *countingSMI) run(ctx context.Context, name string, args ...string) (string, error) {
	c.n.Add(1)
	return fakeSMI(ctx, name, args...)
}

func waitVisible(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.VisibleClients() != n {
		if time.Now().After(deadline) {
			t.Fatalf("hub has %d visible clients, want %d", h.VisibleClients(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// quiet asserts that the fake nvidia-smi is not run for a while (one tick
// that was already due may still finish first).
func quiet(t *testing.T, smi *countingSMI, what string) {
	t.Helper()
	time.Sleep(120 * time.Millisecond) // > 2 intervals: a running tick ends
	before := smi.n.Load()
	time.Sleep(300 * time.Millisecond) // 6 intervals of 50 ms
	if after := smi.n.Load(); after != before {
		t.Fatalf("%s: nvidia-smi ran %d times", what, after-before)
	}
}

func TestHiddenFrontEndsDoNotSample(t *testing.T) {
	smi := &countingSMI{}
	e := newEnvWith(t, envConfig{resources: true, resDeps: func(d *resources.Deps) { d.RunCmd = smi.run }})

	// A hidden page: connected, no sampling, but its stream works.
	hidden := openSSE(t, e.ts.URL+"/api/events?client=page1&visible=0&seq=1")
	waitClients(t, e.hub, 1)
	if v := e.hub.VisibleClients(); v != 0 {
		t.Fatalf("visible clients %d", v)
	}
	quiet(t, smi, "hidden page only")
	if n := smi.n.Load(); n != 0 {
		t.Fatalf("nvidia-smi ran %d times for a hidden page", n)
	}
	e.hub.Job(model.Job{ID: "J1", Scope: model.ScopeLecture, ScopeID: "L1", Stage: model.StageExport, Status: model.StatusDone})
	hidden.next(3*time.Second, func(f sseFrame) bool { return f.event == "job" && strings.Contains(f.data, `"done"`) })

	// Shown: a sample at once, broadcast to the stream.
	e.call("POST", "/api/events/visibility", map[string]any{"client": "page1", "visible": true, "seq": 2}, http.StatusNoContent, nil)
	waitVisible(t, e.hub, 1)
	hidden.next(3*time.Second, func(f sseFrame) bool { return f.event == "resources" })
	if smi.n.Load() == 0 {
		t.Fatal("no sample after the page was shown")
	}

	// A late report (lower seq) does not undo the newer state.
	e.call("POST", "/api/events/visibility", map[string]any{"client": "page1", "visible": false, "seq": 1}, http.StatusNoContent, nil)
	if v := e.hub.VisibleClients(); v != 1 {
		t.Fatalf("a stale report hid the page: visible %d", v)
	}

	// Hidden again: sampling stops.
	e.call("POST", "/api/events/visibility", map[string]any{"client": "page1", "visible": false, "seq": 3}, http.StatusNoContent, nil)
	waitVisible(t, e.hub, 0)
	quiet(t, smi, "page hidden again")

	// An unknown client is no error (its next connect reports again).
	e.call("POST", "/api/events/visibility", map[string]any{"client": "gone", "visible": true, "seq": 9}, http.StatusNoContent, nil)
	if v := e.hub.VisibleClients(); v != 0 {
		t.Fatalf("visible %d after a report for an unknown client", v)
	}

	// A stream without the query counts as looking (other clients, old pages).
	plain := openSSE(t, e.ts.URL+"/api/events")
	waitClients(t, e.hub, 2)
	waitVisible(t, e.hub, 1)
	plain.next(3*time.Second, func(f sseFrame) bool { return f.event == "resources" })
}

func TestStreamQueryOrdersAgainstPosts(t *testing.T) {
	e := newEnv(t)
	// The page connected hidden with seq 5 (a reconnect): a post sent
	// before it (seq 4, "shown") arrives late and is ignored.
	openSSE(t, e.ts.URL+"/api/events?client=page2&visible=0&seq=5")
	waitClients(t, e.hub, 1)
	e.call("POST", "/api/events/visibility", map[string]any{"client": "page2", "visible": true, "seq": 4}, http.StatusNoContent, nil)
	if v := e.hub.VisibleClients(); v != 0 {
		t.Fatalf("visible %d, want 0", v)
	}
	e.call("POST", "/api/events/visibility", map[string]any{"client": "page2", "visible": true, "seq": 6}, http.StatusNoContent, nil)
	waitVisible(t, e.hub, 1)
}

func TestEventsVisibilityBadRequests(t *testing.T) {
	e := newEnv(t)
	for _, body := range []map[string]any{
		{"visible": true, "seq": 1},                   // no client
		{"client": "a b", "visible": true, "seq": 1},  // bad id
		{"client": "abc", "seq": 1},                   // no visible
		{"client": "abc", "visible": true},            // no seq
		{"client": "abc", "visible": true, "seq": -1}, // bad seq
	} {
		res, b := e.do("POST", "/api/events/visibility", body)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "request.invalid") {
			t.Errorf("%v: %d %s", body, res.StatusCode, b)
		}
	}
	res, b := e.do("POST", "/api/events/visibility", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("no body: %d %s", res.StatusCode, b)
	}
}

func TestHubVisibleCountReachesHook(t *testing.T) {
	h := NewHub()
	var last atomic.Int32
	last.Store(-1)
	h.setClientsHook(func(n int) { last.Store(int32(n)) })
	if last.Load() != 0 {
		t.Fatalf("initial count %d", last.Load())
	}
	a := h.add(clientVisibility{id: "a", visible: true, seq: 1})
	b := h.add(clientVisibility{id: "b", visible: false, seq: 1})
	if last.Load() != 1 || h.Clients() != 2 {
		t.Fatalf("count %d, clients %d", last.Load(), h.Clients())
	}
	if !h.setVisibility(clientVisibility{id: "b", visible: true, seq: 2}) || last.Load() != 2 {
		t.Fatalf("count %d after b was shown", last.Load())
	}
	if h.setVisibility(clientVisibility{id: "b", visible: false, seq: 2}) {
		t.Fatal("a report with the same seq was applied")
	}
	if h.setVisibility(clientVisibility{visible: false, seq: 9}) {
		t.Fatal("a report without an id was applied")
	}
	h.remove(a)
	if last.Load() != 1 {
		t.Fatalf("count %d after a left", last.Load())
	}
	h.remove(b)
	if last.Load() != 0 {
		t.Fatalf("count %d after b left", last.Load())
	}
}

func TestStaticETagRevalidates(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/app.js", "/api.js", "/style.css", "/i18n/zh-CN.json", "/i18n/en.json"} {
		res, err := http.Get(e.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		tag := res.Header.Get("ETag")
		if res.StatusCode != http.StatusOK || len(body) == 0 || !strings.HasPrefix(tag, `"`) || len(tag) != 34 {
			t.Fatalf("%s: %d, %d bytes, ETag %q", p, res.StatusCode, len(body), tag)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control %q, want no-cache (revalidate every time)", p, cc)
		}
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+p, nil)
		req.Header.Set("If-None-Match", tag)
		res2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := io.ReadAll(res2.Body)
		res2.Body.Close()
		if res2.StatusCode != http.StatusNotModified || len(b2) != 0 || res2.Header.Get("ETag") != tag {
			t.Fatalf("%s revalidation: %d, %d bytes, ETag %q", p, res2.StatusCode, len(b2), res2.Header.Get("ETag"))
		}
		// Another tag (an older build's) gets the content again.
		req.Header.Set("If-None-Match", `"0123456789abcdef0123456789abcdef"`)
		res3, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b3, _ := io.ReadAll(res3.Body)
		res3.Body.Close()
		if res3.StatusCode != http.StatusOK || string(b3) != string(body) {
			t.Fatalf("%s with another tag: %d, %d bytes", p, res3.StatusCode, len(b3))
		}
	}
	// Different content, different tags; the root and index.html share one.
	tags := staticETags(e.srv.staticFS)()
	if tags["app.js"] == tags["api.js"] || tags["index.html"] == "" {
		t.Fatalf("tags %q %q %q", tags["app.js"], tags["api.js"], tags["index.html"])
	}
}

func TestStaticName(t *testing.T) {
	for in, want := range map[string]string{
		"/":                 "index.html",
		"/app.js":           "app.js",
		"/i18n/zh-CN.json":  "i18n/zh-CN.json",
		"/./i18n/../app.js": "app.js",
		"/index.html":       "", // redirected to "/"
		"/vendor/":          "vendor/index.html",
		"/vendor":           "vendor",
	} {
		if got := staticName(in); got != want {
			t.Errorf("staticName(%q) = %q, want %q", in, got, want)
		}
	}
}
