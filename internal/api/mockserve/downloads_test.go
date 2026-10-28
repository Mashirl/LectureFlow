// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
)

// fastDownloads shortens the mock's timing for one test.
func fastDownloads(t *testing.T) {
	t.Helper()
	tick, merge, imp, start, rate := dlTickEvery, dlMergeDur, dlImportDur, dlStartDelay, dlRetryRate
	dlTickEvery, dlMergeDur, dlImportDur, dlStartDelay, dlRetryRate = 10*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond, 1e12
	t.Cleanup(func() {
		dlTickEvery, dlMergeDur, dlImportDur, dlStartDelay, dlRetryRate = tick, merge, imp, start, rate
	})
}

// dlGet decodes GET /api/downloads strictly (the frozen Download fields only).
func dlGet(t *testing.T, base, query string) []mockDownload {
	t.Helper()
	code, b, _ := mockDo(t, "GET", base+"/api/downloads"+query, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/downloads%s: %d %s", query, code, b)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var out api.DownloadList
	if err := dec.Decode(&out); err != nil || out.Items == nil {
		t.Fatalf("GET /api/downloads%s: %v %s", query, err, b)
	}
	return out.Items
}

func dlByID(items []mockDownload) map[int64]mockDownload {
	m := map[int64]mockDownload{}
	for _, d := range items {
		m[d.ID] = d
	}
	return m
}

func dlErrorCode(t *testing.T, b []byte) string {
	t.Helper()
	var e api.ErrorBody
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("error body %s: %v", b, err)
	}
	return e.Error.Code
}

func TestMockDownloadsList(t *testing.T) {
	srv := testServer(t)
	u := srv.URL

	// other scenarios: no downloads, the pages look as before
	for _, sc := range []string{"1", "empty"} {
		if items := dlGet(t, u, "?mock="+sc); len(items) != 0 {
			t.Errorf("scenario %s: %d downloads", sc, len(items))
		}
	}

	items := dlGet(t, u, "?mock=downloads")
	if len(items) != 10 {
		t.Fatalf("%d sample downloads, want 10", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].CreatedAt < items[i].CreatedAt {
			t.Errorf("not newest first: %d (%s) before %d (%s)", items[i-1].ID, items[i-1].CreatedAt, items[i].ID, items[i].CreatedAt)
		}
	}
	states := map[string]int{}
	for _, d := range items {
		states[d.State]++
		if d.Tracks == nil || d.AllowedActions == nil {
			t.Errorf("%d: tracks / allowed_actions must be lists: %+v", d.ID, d)
		}
	}
	for _, s := range []string{"queued", "downloading", "merging", "importing", "done", "failed", "needs_refresh", "cancelled"} {
		if states[s] == 0 {
			t.Errorf("no sample in state %s", s)
		}
	}
	by := dlByID(items)
	if by[42].Position != 1 || by[43].Position != 2 || by[41].Position != 0 {
		t.Errorf("queue positions: 42=%d 43=%d 41=%d", by[42].Position, by[43].Position, by[41].Position)
	}
	if by[46].Error == nil || by[46].Error.Code != "downloads.restarted" || by[47].Error == nil || by[47].Error.Code != "downloads.http_status" {
		t.Errorf("errors: needs_refresh %+v, failed %+v", by[46].Error, by[47].Error)
	}
	if by[49].LectureID == "" {
		t.Error("the done sample has no lecture")
	}
	want := map[string]string{"queued": "cancel", "downloading": "cancel", "merging": "cancel", "needs_refresh": "cancel", "failed": "retry,remove", "done": "remove", "cancelled": "remove", "importing": ""}
	for _, d := range items {
		if got := strings.Join(d.AllowedActions, ","); got != want[d.State] {
			t.Errorf("%d (%s): allowed %q, want %q", d.ID, d.State, got, want[d.State])
		}
	}

	osItems := dlGet(t, u, "?mock=downloads&course_id="+dlCourseOS)
	if len(osItems) != 1 || osItems[0].ID != 51 {
		t.Errorf("course filter: %+v", osItems)
	}
	if none := dlGet(t, u, "?mock=downloads&course_id=nope"); len(none) != 0 {
		t.Errorf("unknown course: %d downloads", len(none))
	}
}

func TestMockDownloadsActions(t *testing.T) {
	fastDownloads(t)
	srv := testServer(t)
	u := srv.URL + "/api/downloads/"
	q := "?mock=downloads"

	expect := func(method, path string, status int, code string) []byte {
		t.Helper()
		got, b, _ := mockDo(t, method, u+path+q, "")
		if got != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, got, b, status)
		}
		if code != "" {
			if c := dlErrorCode(t, b); c != code {
				t.Fatalf("%s %s: code %q, want %q", method, path, c, code)
			}
		}
		return b
	}

	expect("POST", "999/cancel", 404, "downloads.not_found")
	expect("DELETE", "abc", 404, "downloads.not_found")
	if got, b, _ := mockDo(t, "POST", u+"41/cancel?mock=1", ""); got != 404 || dlErrorCode(t, b) != "downloads.not_found" {
		t.Errorf("another scenario: %d %s", got, b)
	}

	var d mockDownload
	if err := json.Unmarshal(expect("POST", "41/cancel", 200, ""), &d); err != nil || d.State != "cancelled" || strings.Join(d.AllowedActions, ",") != "remove" {
		t.Fatalf("cancel: %v %+v", err, d)
	}
	expect("POST", "41/cancel", 409, "downloads.not_cancellable")
	expect("POST", "41/retry", 409, "downloads.not_retryable")
	expect("POST", "45/cancel", 409, "downloads.not_cancellable") // importing
	expect("POST", "46/retry", 409, "downloads.needs_refresh")
	expect("DELETE", "42", 409, "downloads.active")
	expect("DELETE", "41", 204, "")
	if _, ok := dlByID(dlGet(t, srv.URL, q))[41]; ok {
		t.Error("removed task still listed")
	}
	// 43 moved up in the queue when 42 left it
	expect("POST", "42/cancel", 200, "")
	if p := dlByID(dlGet(t, srv.URL, q))[43].Position; p != 1 {
		t.Errorf("position after cancelling the first: %d", p)
	}

	// a retried task queues, downloads to the end, merges and creates its lecture
	if err := json.Unmarshal(expect("POST", "47/retry", 200, ""), &d); err != nil || d.State != "queued" || d.Error != nil || d.Position == 0 {
		t.Fatalf("retry: %v %+v", err, d)
	}
	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !seen["done"] {
		d = dlByID(dlGet(t, srv.URL, q))[47]
		seen[d.State] = true
		time.Sleep(5 * time.Millisecond)
	}
	if !seen["done"] || d.LectureID == "" {
		t.Fatalf("retried task did not finish: %+v (seen %v)", d, seen)
	}
	code, b, _ := mockDo(t, "GET", srv.URL+"/api/courses/"+dlCourseCrypto+"/lectures?mock=downloads", "")
	if code != 200 || !strings.Contains(string(b), d.LectureID) || !strings.Contains(string(b), "Week 6 · Digital signatures") {
		t.Errorf("the lecture of the download is not in the course: %d", code)
	}
}

func TestMockDownloadsEvents(t *testing.T) {
	fastDownloads(t)
	srv := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events?mock=downloads", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// the progress of the downloading samples once the scenario is read
	dlGet(t, srv.URL, "?mock=downloads")
	sc := bufio.NewScanner(res.Body)
	var event string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if event != "changed" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev api.ChangedEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == "downloads" {
			if ev.ID == "" {
				t.Errorf("changed downloads without the task id: %+v", ev)
			}
			return
		}
	}
	t.Fatalf("no changed downloads event (scan err %v)", sc.Err())
}

func TestMockDownloadSettings(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/app/settings?mock=1"
	read := func(b []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		return m
	}
	code, b, _ := mockDo(t, "GET", u, "")
	s := read(b)
	if code != 200 || s["download_concurrency"] != float64(3) || s["download_connections"] != float64(4) || s["close_behavior"] != "ask" {
		t.Fatalf("defaults: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PATCH", u, `{"download_concurrency":5}`); code != 200 || read(b)["download_concurrency"] != float64(5) || read(b)["download_connections"] != float64(4) {
		t.Errorf("concurrency: %d %s", code, b)
	}
	for _, body := range []string{`{"download_concurrency":0}`, `{"download_concurrency":7}`, `{"download_concurrency":2.5}`, `{"download_concurrency":"3"}`} {
		if code, b, _ = mockDo(t, "PATCH", u, body); code != 400 || dlErrorCode(t, b) != "downloads.concurrency_invalid" {
			t.Errorf("%s: %d %s", body, code, b)
		}
	}
	if code, b, _ = mockDo(t, "PATCH", u, `{"download_connections":17}`); code != 400 || dlErrorCode(t, b) != "downloads.connections_invalid" {
		t.Errorf("connections 17: %d %s", code, b)
	}
	// with another field: both are saved; a bad other field saves nothing
	if code, b, _ = mockDo(t, "PATCH", u, `{"download_connections":8,"close_behavior":"bogus"}`); code != 400 || dlErrorCode(t, b) != "app_settings.close_behavior_invalid" {
		t.Errorf("bad other field: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PATCH", u, `{"download_connections":16,"close_behavior":"quit"}`); code != 200 || read(b)["download_connections"] != float64(16) || read(b)["close_behavior"] != "quit" {
		t.Errorf("mixed patch: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PATCH", u, `{}`); code != 400 || dlErrorCode(t, b) != "request.nothing_to_change" {
		t.Errorf("empty patch: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "GET", u, ""); code != 200 || read(b)["download_concurrency"] != float64(5) || read(b)["download_connections"] != float64(16) {
		t.Errorf("after the patches: %d %s", code, b)
	}
}
