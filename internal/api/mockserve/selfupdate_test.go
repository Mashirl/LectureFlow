// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
)

// fastSelfUpdate shortens the mock's timing for one test.
func fastSelfUpdate(t *testing.T) {
	t.Helper()
	c, d, i, k := suCheckDur, suDownloadDur, suImportDur, suTickEvery
	suCheckDur, suDownloadDur, suImportDur, suTickEvery = 10*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { suCheckDur, suDownloadDur, suImportDur, suTickEvery = c, d, i, k })
}

// suDecode decodes a Status strictly (the fields of selfupdate.Status only).
func suDecode(t *testing.T, b []byte) selfupdate.Status {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var st selfupdate.Status
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("status %s: %v", b, err)
	}
	return st
}

func suStatus(t *testing.T, base, sc string) selfupdate.Status {
	t.Helper()
	code, b, _ := mockDo(t, "GET", base+"/api/selfupdate?mock="+sc, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/selfupdate?mock=%s: %d %s", sc, code, b)
	}
	return suDecode(t, b)
}

// suCall sends a request and checks its status; it returns the body.
func suCall(t *testing.T, method, url, body string, want int) []byte {
	t.Helper()
	code, b, _ := mockDo(t, method, url, body)
	if code != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, url, code, want, b)
	}
	return b
}

func suWait(t *testing.T, base, sc, state string) selfupdate.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := suStatus(t, base, sc)
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: state %q, want %q", sc, st.State, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMockSelfUpdateNotConfigured(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	// web mode: nothing could install an update
	st := suStatus(t, u, "1")
	if st.State != selfupdate.StateNotConfigured || st.Reason != selfupdate.ReasonWebMode || st.CurrentVersion == "" {
		t.Fatalf("web mode: %+v", st)
	}
	for _, c := range []struct{ method, path string }{{"POST", "/api/selfupdate/check"}, {"POST", "/api/selfupdate/download"}, {"DELETE", "/api/selfupdate/pending"}} {
		b := suCall(t, c.method, u+c.path+"?mock=1", "", http.StatusConflict)
		if code := dlErrorCode(t, b); code != selfupdate.CodeNotConfigured {
			t.Errorf("%s %s: %s", c.method, c.path, code)
		}
	}
	for sc, reason := range map[string]string{"su_nokey": selfupdate.ReasonNoPublicKey, "su_dev": selfupdate.ReasonDevBuild} {
		if st := suStatus(t, u, sc); st.State != selfupdate.StateNotConfigured || st.Reason != reason || st.Available != nil {
			t.Errorf("%s: %+v", sc, st)
		}
	}
	// desktop mode outside the su_* scenarios: a build without the update
	// key, like every build so far (the workbench keeps the electron-updater
	// hint of the update_* scenarios)
	for _, sc := range []string{"ob_desktop", "update_available"} {
		if st := suStatus(t, u, sc); st.State != selfupdate.StateNotConfigured || (sc == "ob_desktop" && st.Reason != selfupdate.ReasonNoPublicKey) {
			t.Errorf("%s: %+v", sc, st)
		}
	}
	// the su_* scenarios play a configured desktop app
	if st := suStatus(t, u, "su_idle"); st.State != selfupdate.StateIdle || !st.AutoCheck || st.Reason != "" {
		t.Errorf("su_idle: %+v", st)
	}
}

func TestMockSelfUpdateStates(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	want := map[string]string{
		"su_idle": "idle", "su_uptodate": "up_to_date", "su_available": "available", "su_downloading": "downloading",
		"su_pending": "pending", "su_both": "available", "su_error": "error", "su_dlerror": "error", "su_dlfail": "available",
		"su_tooold": "available", "su_nopkg": "available", "su_rollback": "idle", "su_failed": "idle", "su_installed": "up_to_date",
		"su_nomanifest": "error", "su_checkfail": "available",
	}
	for sc, state := range want {
		st := suStatus(t, u, sc)
		if st.State != state {
			t.Errorf("%s: state %q, want %q", sc, st.State, state)
		}
		if st.Platform != selfupdate.PlatformWindowsX64 || st.Kind != selfupdate.KindInstaller || st.Channel != selfupdate.ChannelStable {
			t.Errorf("%s: platform %q kind %q channel %q", sc, st.Platform, st.Kind, st.Channel)
		}
	}
	if st := suStatus(t, u, "su_both"); st.Available == nil || st.Available.Version != "3.2.1" || st.Pending == nil || st.Pending.Version != "3.2.0" {
		t.Errorf("su_both: %+v", st)
	}
	if st := suStatus(t, u, "su_tooold"); st.Available == nil || st.Available.Direct || st.Available.MinVersion != "3.5.0" {
		t.Errorf("su_tooold: %+v", st.Available)
	}
	if st := suStatus(t, u, "su_dlerror"); st.Error == nil || st.Error.Code != selfupdate.CodeDownloadFailed || st.Error.Detail == "" || st.Available == nil {
		t.Errorf("su_dlerror: %+v", st)
	}
	// a failed check: its own code for a release without update.json, the
	// next automatic check sooner (selfupdate.CheckInterval), and an update
	// found before stays available with the failure as a note
	if st := suStatus(t, u, "su_nomanifest"); st.Error == nil || st.Error.Code != selfupdate.CodeNoManifest || st.Error.Detail == "" || st.NextCheck == "" {
		t.Errorf("su_nomanifest: %+v", st)
	}
	if st := suStatus(t, u, "su_checkfail"); st.Available == nil || st.Error == nil || st.Error.Code != selfupdate.CodeUnreachable ||
		strings.Count(st.Error.Detail, "\n") != 1 {
		t.Errorf("su_checkfail: %+v", st)
	}
	if st := suStatus(t, u, "su_error"); st.NextCheck == "" {
		t.Errorf("su_error: no next check %+v", st)
	} else {
		last, _ := time.Parse(time.RFC3339, st.LastCheck)
		next, _ := time.Parse(time.RFC3339, st.NextCheck)
		if next.Sub(last) != time.Hour {
			t.Errorf("su_error: next check %s after %s", st.NextCheck, st.LastCheck)
		}
	}
	if st := suStatus(t, u, "su_rollback"); st.LastResult == nil || st.LastResult.Outcome != "rolled_back" || st.LastResult.Error == "" || st.LastResult.From != st.CurrentVersion {
		t.Errorf("su_rollback: %+v", st.LastResult)
	}
	if st := suStatus(t, u, "su_pending"); st.Rollback == nil || st.Rollback.Version != st.CurrentVersion || st.Pending.Origin != "download" {
		t.Errorf("su_pending: %+v", st)
	}
	for _, sc := range []string{"su_available", "su_dlfail"} {
		if a := suStatus(t, u, sc).Available; a == nil || !strings.HasPrefix(a.NotesURL, "https://") || a.File == "" || a.Size <= 0 || !a.Direct {
			t.Errorf("%s: %+v", sc, a)
		}
	}
}

func TestMockSelfUpdateCheck(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	check := func(sc string) selfupdate.Status {
		t.Helper()
		return suDecode(t, suCall(t, "POST", u+"/api/selfupdate/check?mock="+sc, "", http.StatusOK))
	}
	if st := check("su_idle"); st.State != "available" || st.Available.Version != "3.2.0" || st.LastCheck == "" {
		t.Errorf("su_idle check: %+v", st)
	}
	if st := check("su_uptodate"); st.State != "up_to_date" || st.Available != nil {
		t.Errorf("su_uptodate check: %+v", st)
	}
	if st := check("su_pending"); st.State != "pending" || st.Available != nil {
		t.Errorf("su_pending check: nothing newer than the pending version: %+v", st)
	}
	// a check that reaches no source is Status.error, until a mirror helps
	if st := check("su_error"); st.State != "error" || st.Error.Code != selfupdate.CodeUnreachable {
		t.Errorf("su_error check: %+v", st)
	}
	if st := check("su_nosource"); st.State != "error" || st.Error.Code != selfupdate.CodeNotConfigured || st.Error.Params["reason"] != selfupdate.ReasonNoSource {
		t.Errorf("su_nosource check: %+v", st)
	}
	if st := check("su_nomanifest"); st.State != "error" || st.Error.Code != selfupdate.CodeNoManifest {
		t.Errorf("su_nomanifest check: %+v", st)
	}
	if st := check("su_checkfail"); st.State != "available" || st.Available == nil || st.Error == nil || st.Error.Code != selfupdate.CodeUnreachable {
		t.Errorf("su_checkfail check: %+v", st)
	}
	suCall(t, "PATCH", u+"/api/selfupdate/settings?mock=su_checkfail", `{"sources":["https://mirror.example.org/lectureflow/","https://mirror2.example.org/lf/"]}`, http.StatusOK)
	if st := check("su_checkfail"); st.State != "available" || st.Error != nil {
		t.Errorf("su_checkfail check with another mirror: %+v", st)
	}
	for _, sc := range []string{"su_error", "su_nosource", "su_nomanifest"} {
		suCall(t, "PATCH", u+"/api/selfupdate/settings?mock="+sc, `{"sources":["https://mirror.example.org/lectureflow/"]}`, http.StatusOK)
		if st := check(sc); st.State != "available" || st.Error != nil {
			t.Errorf("%s check with a mirror: %+v", sc, st)
		}
	}
}

func TestMockSelfUpdateDownload(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	b := suCall(t, "POST", u+"/api/selfupdate/download?mock=su_available", "", http.StatusAccepted)
	if st := suDecode(t, b); st.State != "downloading" || st.Progress == nil || st.Progress.BytesTotal != st.Available.Size {
		t.Fatalf("download: %+v", st)
	}
	// busy while it runs
	if code := dlErrorCode(t, suCall(t, "POST", u+"/api/selfupdate/check?mock=su_available", "", http.StatusConflict)); code != selfupdate.CodeBusy {
		t.Errorf("check while downloading: %s", code)
	}
	st := suWait(t, u, "su_available", "pending")
	if st.Pending == nil || st.Pending.Version != "3.2.0" || st.Pending.Origin != "download" || st.Rollback == nil || st.Available != nil || st.Progress != nil {
		t.Errorf("after the download: %+v", st)
	}
	// discard the pending update: the version is offered again
	st = suDecode(t, suCall(t, "DELETE", u+"/api/selfupdate/pending?mock=su_available", "", http.StatusOK))
	if st.Pending != nil || st.State != "available" {
		t.Errorf("after discarding: %+v", st)
	}

	// cancel keeps the downloaded part; the next download continues
	suCall(t, "POST", u+"/api/selfupdate/download?mock=su_idle", "", http.StatusConflict) // nothing available yet
	suCall(t, "POST", u+"/api/selfupdate/check?mock=su_idle", "", http.StatusOK)
	suCall(t, "POST", u+"/api/selfupdate/download?mock=su_idle", "", http.StatusAccepted)
	time.Sleep(100 * time.Millisecond)
	st = suDecode(t, suCall(t, "DELETE", u+"/api/selfupdate/download?mock=su_idle", "", http.StatusOK))
	if st.State != "available" || st.Progress != nil {
		t.Fatalf("after cancelling: %+v", st)
	}
	st = suDecode(t, suCall(t, "POST", u+"/api/selfupdate/download?mock=su_idle", "", http.StatusAccepted))
	if st.Progress == nil || st.Progress.BytesDone == 0 {
		t.Errorf("the next download does not continue: %+v", st.Progress)
	}
	suWait(t, u, "su_idle", "pending")

	// what cannot be downloaded here
	for sc, code := range map[string]string{"su_tooold": selfupdate.CodeTooOld, "su_nopkg": selfupdate.CodeNoPackage, "su_uptodate": selfupdate.CodeNotAvailable} {
		if got := dlErrorCode(t, suCall(t, "POST", u+"/api/selfupdate/download?mock="+sc, "", http.StatusConflict)); got != code {
			t.Errorf("%s: %s, want %s", sc, got, code)
		}
		if st := suStatus(t, u, sc); st.State == "downloading" {
			t.Errorf("%s: downloading after a refused download", sc)
		}
	}

	// su_dlfail: the first download fails with a wrong SHA-256, the next works
	suCall(t, "POST", u+"/api/selfupdate/download?mock=su_dlfail", "", http.StatusAccepted)
	st = suWait(t, u, "su_dlfail", "error")
	if st.Error == nil || st.Error.Code != selfupdate.CodeChecksum || st.Error.Params["file"] != st.Available.File {
		t.Errorf("su_dlfail: %+v", st.Error)
	}
	suCall(t, "POST", u+"/api/selfupdate/download?mock=su_dlfail", "", http.StatusAccepted)
	suWait(t, u, "su_dlfail", "pending")

	// su_downloading moves on by itself
	suWait(t, u, "su_downloading", "pending")
}

func suMultipart(t *testing.T, names ...string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, n := range names {
		w, err := mw.CreateFormFile("file", n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestMockSelfUpdateImport(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	post := func(sc, ct string, body *bytes.Buffer, want int) []byte {
		t.Helper()
		res, err := http.Post(u+"/api/selfupdate/import?mock="+sc, ct, body)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		if res.StatusCode != want {
			t.Fatalf("import (%s): %d, want %d: %s", sc, res.StatusCode, want, out.Bytes())
		}
		return out.Bytes()
	}
	cases := []struct {
		names  []string
		status int
		code   string
		param  string
	}{
		{[]string{"update.json", "update.json.minisig"}, 400, selfupdate.CodeImportIncomplete, "package"},
		{[]string{"update.json", "LectureFlow-Setup-3.2.0-x64.exe"}, 400, selfupdate.CodeImportIncomplete, "signature"},
		{[]string{"update.json.minisig", "LectureFlow-Setup-3.2.0-x64.exe"}, 400, selfupdate.CodeImportIncomplete, "manifest"},
		{[]string{"a.json", "b.minisig", "c.exe", "d.exe"}, 400, selfupdate.CodeImportInvalid, "d.exe"},
		{[]string{"update.json", "update.json.minisig", "bad.exe"}, 400, selfupdate.CodeSignatureInvalid, ""},
		{[]string{"update-old.json", "update.json.minisig", "x.exe"}, 409, selfupdate.CodeNotNewer, ""},
	}
	for _, c := range cases {
		ct, body := suMultipart(t, c.names...)
		b := post("su_uptodate", ct, body, c.status)
		if got := dlErrorCode(t, b); got != c.code {
			t.Errorf("%v: %s, want %s", c.names, got, c.code)
		}
		if c.param != "" && !strings.Contains(string(b), `"`+c.param+`"`) {
			t.Errorf("%v: %s lacks %q", c.names, b, c.param)
		}
	}
	if st := suStatus(t, u, "su_uptodate"); st.State != "up_to_date" || st.Pending != nil {
		t.Errorf("failed imports changed the status: %+v", st)
	}
	// one zip is a complete bundle
	ct, body := suMultipart(t, "LectureFlow-3.2.0-update.zip")
	st := suDecode(t, post("su_uptodate", ct, body, http.StatusOK))
	if st.State != "pending" || st.Pending == nil || st.Pending.Origin != "file" || st.Pending.Version != "3.2.0" {
		t.Errorf("zip import: %+v", st)
	}
	// desktop mode: the local paths
	b := suCall(t, "POST", u+"/api/selfupdate/import?mock=su_idle", `{"paths":["C:\\Users\\Student\\Downloads\\update.json","C:\\Users\\Student\\Downloads\\update.json.minisig","C:\\Users\\Student\\Downloads\\LectureFlow-Setup-3.2.0-x64.exe"]}`, http.StatusOK)
	if st := suDecode(t, b); st.State != "pending" || st.Pending.Origin != "file" {
		t.Errorf("paths import: %+v", st)
	}
	// not configured
	ct, body = suMultipart(t, "update.zip")
	if got := dlErrorCode(t, post("1", ct, body, http.StatusConflict)); got != selfupdate.CodeNotConfigured {
		t.Errorf("web mode import: %s", got)
	}
}

func TestMockSelfUpdateSettings(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	get := func() selfupdate.Settings {
		t.Helper()
		var s selfupdate.Settings
		if err := json.Unmarshal(suCall(t, "GET", u+"/api/selfupdate/settings?mock=su_idle", "", http.StatusOK), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := get(); !s.AutoCheck || s.Sources == nil || len(s.Sources) != 0 {
		t.Fatalf("defaults: %+v", s)
	}
	p := u + "/api/selfupdate/settings?mock=su_idle"
	suCall(t, "PATCH", p, `{}`, http.StatusBadRequest)
	b := suCall(t, "PATCH", p, `{"sources":["https://mirror.example.org/lf/","ftp://example.org/"]}`, http.StatusBadRequest)
	if code := dlErrorCode(t, b); code != selfupdate.CodeSourcesInvalid || !strings.Contains(string(b), `"index":1`) {
		t.Errorf("invalid source: %s", b)
	}
	b = suCall(t, "PATCH", p, `{"sources":["https://user:pw@example.org/"]}`, http.StatusBadRequest)
	if !strings.Contains(string(b), `"index":0`) {
		t.Errorf("user info: %s", b)
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "https://m" + string(rune('a'+i)) + ".example.org/"
	}
	nb, _ := json.Marshal(map[string]any{"sources": nine})
	if b := suCall(t, "PATCH", p, string(nb), http.StatusBadRequest); !strings.Contains(string(b), `"index":8`) {
		t.Errorf("nine sources: %s", b)
	}
	var s selfupdate.Settings
	json.Unmarshal(suCall(t, "PATCH", p, `{"sources":[" https://a.example.org/ ","https://a.example.org/","https://proxy.example.org/{url}"],"auto_check":false}`, http.StatusOK), &s)
	if s.AutoCheck || len(s.Sources) != 2 || s.Sources[0] != "https://a.example.org/" {
		t.Errorf("saved: %+v", s)
	}
	if g := get(); g.AutoCheck || len(g.Sources) != 2 {
		t.Errorf("read back: %+v", g)
	}
	if st := suStatus(t, u, "su_idle"); st.AutoCheck {
		t.Errorf("the status still says auto_check")
	}
}

// Every change is announced as SSE changed {kind: "selfupdate"}.
func TestMockSelfUpdateEvents(t *testing.T) {
	fastSelfUpdate(t)
	u := testServer(t).URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u+"/api/events?mock=su_available", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	if !sc.Scan() { // ": connected"
		t.Fatal("no SSE stream")
	}
	go func() {
		if res, err := http.Post(u+"/api/selfupdate/download?mock=su_available", "", nil); err == nil {
			res.Body.Close()
		}
	}()
	seen := 0
	for sc.Scan() && seen < 3 {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") && strings.Contains(line, `"kind":"selfupdate"`) {
			seen++
		}
	}
	if seen < 3 {
		t.Errorf("%d changed selfupdate events during a download, want at least 3", seen)
	}
}
