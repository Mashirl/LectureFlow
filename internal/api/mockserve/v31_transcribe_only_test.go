// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/api"
)

func TestMockTranscribeOnly(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	var l api.Lecture
	doJSON(t, "GET", u+"/api/lectures/"+toLectureID+"?mock=transcribe_only", nil, 200, &l)
	if !l.Playable || len(l.Exports) != 0 || l.Counts.ZhOK != 0 || l.Counts.PatchesPending != 0 {
		t.Fatalf("lecture %+v", l)
	}
	status := map[string]string{}
	for _, st := range l.Stages {
		status[st.Stage] = st.Status
		if st.Stage == "summary" && (st.WaitingFor == nil || *st.WaitingFor != waitNoProvider) {
			t.Fatalf("summary waiting_for %v", st.WaitingFor)
		}
	}
	if status["asr"] != "done" || status["summary"] != "pending" || status["repair"] != "pending" || status["translate"] != "pending" || status["export"] != "pending" {
		t.Fatalf("stages %v", status)
	}
	// the default scenario keeps the lecture as it was
	var other api.Lecture
	doJSON(t, "GET", u+"/api/lectures/"+toLectureID+"?mock=1", nil, 200, &other)
	for _, st := range other.Stages {
		if st.Stage == "summary" && st.Status != "done" {
			t.Fatalf("default scenario changed: %+v", st)
		}
	}
	// still in progress on the workbench (its API stages wait)
	var d api.Dashboard
	doJSON(t, "GET", u+"/api/dashboard?mock=transcribe_only", nil, 200, &d)
	found := false
	for _, ref := range d.InProgress {
		found = found || ref.ID == toLectureID
	}
	if !found {
		t.Fatal("the transcribed-only lecture is not in progress on the workbench")
	}

	get := func(path string) (int, string, http.Header) {
		res, err := http.Get(u + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	code, body, hdr := get("/api/lectures/" + toLectureID + "/english/vtt?mock=transcribe_only")
	if code != 200 || !strings.HasPrefix(body, "WEBVTT\n\n1\n") || !strings.Contains(hdr.Get("Content-Disposition"), ".en.vtt") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/vtt") {
		t.Fatalf("vtt: %d %q %v", code, body[:min(len(body), 80)], hdr)
	}
	code, body, hdr = get("/api/lectures/" + toLectureID + "/english/txt?mock=transcribe_only")
	if code != 200 || strings.Contains(body, "-->") || !strings.Contains(hdr.Get("Content-Disposition"), ".en.txt") {
		t.Fatalf("txt: %d %q", code, body[:min(len(body), 80)])
	}
	if code, body, _ = get("/api/lectures/" + toLectureID + "/english/pdf?mock=transcribe_only"); code != 400 || !strings.Contains(body, "export.bad_name") {
		t.Fatalf("pdf: %d %s", code, body)
	}
	// not transcribed yet: CSC1003 L07 (asr running)
	if code, body, _ = get("/api/lectures/01JD3631000000000000000007/english/vtt?mock=transcribe_only"); code != 404 || !strings.Contains(body, "export.not_found") {
		t.Fatalf("before asr: %d %s", code, body)
	}
	if code, body, _ = get("/api/lectures/nope/english/vtt"); code != 404 || !strings.Contains(body, "lecture.not_found") {
		t.Fatalf("unknown lecture: %d %s", code, body)
	}
}
