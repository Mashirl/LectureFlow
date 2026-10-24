// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/resources"
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	srv := httptest.NewServer(newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond))
	t.Cleanup(srv.Close)
	return srv
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestRootRedirectsToMockMode(t *testing.T) {
	srv := testServer(t)
	res, err := noRedirect().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/?mock=1" {
		t.Fatalf("got %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestServesFrontEndAndExamples(t *testing.T) {
	srv := testServer(t)
	cases := []struct{ path, ctype, contains string }{
		{"/?mock=1", "text/html", "app.js"},
		{"/app.js", "text/javascript", "import"},
		{"/style.css", "text/css", "prefers-color-scheme"},
		{"/testdata/api/lecture.json", "application/json", "allowed_actions"},
	}
	for _, c := range cases {
		res, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", c.path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.ctype) {
			t.Errorf("%s: content-type %q, want %s", c.path, ct, c.ctype)
		}
		if !strings.Contains(string(body), c.contains) {
			t.Errorf("%s: body does not contain %q", c.path, c.contains)
		}
	}
}

func multipartBody(t *testing.T, fields map[string]string, filename string, size int) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte{0}, size)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestUploadLecture(t *testing.T) {
	srv := testServer(t)
	body, ctype := multipartBody(t, map[string]string{"title": "Week 10 - Review"}, "week10.mp4", 100000)
	res, err := http.Post(srv.URL+"/api/courses/C1/lectures", ctype, body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", res.StatusCode)
	}
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	var lec api.Lecture
	if err := dec.Decode(&lec); err != nil {
		t.Fatal(err)
	}
	if lec.Title != "Week 10 - Review" || lec.CourseID != "C1" || len(lec.ID) != 26 {
		t.Errorf("unexpected lecture %+v", lec)
	}
	// v1.1 A3: an upload creates a draft; nothing is queued until it is launched.
	// v2 P13.4: without a term start (and a recording time) the number is W??.
	if !lec.Draft || lec.LaunchedAt != nil || lec.Seq != 1 || lec.Label != "W??" {
		t.Errorf("not a draft: draft=%v launched_at=%v seq=%d label=%q", lec.Draft, lec.LaunchedAt, lec.Seq, lec.Label)
	}
	if len(lec.Stages) != 6 {
		t.Fatalf("unexpected stages %+v", lec.Stages)
	}
	for _, st := range lec.Stages {
		if st.Status != "pending" || st.WaitingFor == nil || *st.WaitingFor != "not_started" || len(st.AllowedActions) != 0 {
			t.Errorf("draft stage %s: status %s waiting_for %v actions %v", st.Stage, st.Status, st.WaitingFor, st.AllowedActions)
		}
	}
	if strings.Join(lec.LectureActions, ",") != "launch,delete" {
		t.Errorf("lecture_actions %v, want [launch delete]", lec.LectureActions)
	}
}

func TestUploadSimulatedFailure(t *testing.T) {
	srv := testServer(t)
	body, ctype := multipartBody(t, map[string]string{"title": "please fail"}, "x.mp4", 10)
	res, err := http.Post(srv.URL+"/api/courses/C1/lectures", ctype, body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var e api.ErrorBody
	if res.StatusCode != http.StatusInternalServerError || json.NewDecoder(res.Body).Decode(&e) != nil || e.Error.Detail == "" {
		t.Fatalf("status %d, error %q", res.StatusCode, e.Error.Detail)
	}
}

func TestUploadCourseFiles(t *testing.T) {
	srv := testServer(t)
	for _, p := range []string{"/api/courses/C1/syllabus", "/api/courses/C1/materials"} {
		body, ctype := multipartBody(t, nil, "syllabus.pdf", 1000)
		res, err := http.Post(srv.URL+p, ctype, body)
		if err != nil {
			t.Fatal(err)
		}
		var c api.Course
		err = json.NewDecoder(res.Body).Decode(&c)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || err != nil || c.ID == "" {
			t.Errorf("%s: status %d, err %v", p, res.StatusCode, err)
		}
	}
}

func TestAudioIsWAV(t *testing.T) {
	srv := testServer(t)
	res, err := http.Get(srv.URL + "/api/lectures/L1/audio?start_ms=1000&end_ms=2000")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	// 1 s clip + 1 s padding on each side = 3 s of 16 kHz 16-bit mono.
	if want := 44 + 3*sampleRate*2; len(b) != want {
		t.Fatalf("got %d bytes, want %d", len(b), want)
	}
	if string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" || res.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("not a WAV response")
	}
	res, err = http.Get(srv.URL + "/api/lectures/L1/audio?start_ms=x")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad params: status %d", res.StatusCode)
	}
}

func TestEventsStream(t *testing.T) {
	srv := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	seen := map[string]bool{}
	var event string
	for sc.Scan() && !(seen["job"] && seen["changed"] && seen["ping"] && seen["resources"]) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": ping"):
			seen["ping"] = true
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := []byte(strings.TrimPrefix(line, "data: "))
			var v any = &api.JobEvent{}
			switch event {
			case "changed":
				v = &api.ChangedEvent{}
			case "resources": // v1.1 A10
				v = &resources.Snapshot{}
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("%s: %v", event, err)
			}
			seen[event] = true
		}
	}
	if !(seen["job"] && seen["changed"] && seen["ping"] && seen["resources"]) {
		t.Fatalf("missing events: %v (scan err %v)", seen, sc.Err())
	}
}

func TestUnknownAPIIsJSON404(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/terms/T1", strings.NewReader(`{}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var e api.ErrorBody
	if res.StatusCode != http.StatusNotFound || json.NewDecoder(res.Body).Decode(&e) != nil || e.Error.Detail == "" {
		t.Fatalf("status %d, error %q", res.StatusCode, e.Error.Detail)
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:8421": true, "localhost:1": true, "[::1]:8421": true, "0.0.0.0:8421": false, ":8421": false} {
		if err := checkLoopback(addr); (err == nil) != ok {
			t.Errorf("%s: err %v", addr, err)
		}
	}
}
