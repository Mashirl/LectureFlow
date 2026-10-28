// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/nmhost"
)

// ext web3 (ext_web3.go): the second angle, its file, adding, replacing and
// removing one; the diagnostics rows aria2c and browser_ext and the Native
// Messaging status.

const lectureNoChapters = "01JD3631000000000000000003" // CSC3631 L03: video, no chapters

func fastAngles(t *testing.T) {
	old := angleCopyDelay
	angleCopyDelay = 10 * time.Millisecond
	t.Cleanup(func() { angleCopyDelay = old })
}

func TestMockLectureAngles(t *testing.T) {
	srv := testServer(t)
	var l api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+angleLecture+"?mock=angles", nil, http.StatusOK, &l)
	if len(l.Angles) != 1 || l.Angles[0].Name != angleName {
		t.Errorf("scenario angles: %+v", l.Angles)
	}
	if l.ID != angleLecture || !l.Playable || l.Label == "" || l.CourseID == "" {
		t.Errorf("the lecture lost its fields: %+v", l)
	}
	// no angles elsewhere: the field is absent
	for _, u := range []string{"/api/lectures/" + angleLecture + "?mock=1", "/api/lectures/" + mockLectureStaleChapters + "?mock=angles"} {
		var raw map[string]any
		res, err := http.Get(srv.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(res.Body).Decode(&raw)
		res.Body.Close()
		if _, ok := raw["angles"]; ok || raw["id"] == nil {
			t.Errorf("%s: angles %v", u, raw["angles"])
		}
	}
}

func TestMockAngleFile(t *testing.T) {
	srv := testServer(t)
	get := func(u string, rng string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+u, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	u := "/api/lectures/" + angleLecture + "/angles/" + angleName + "?mock=angles"
	// no -angle2 and no -media: 404 with a reason
	if st, _ := get(u, ""); st != http.StatusNotFound {
		t.Errorf("without a file: %d", st)
	}
	f := filepath.Join(t.TempDir(), "angle2.mp4")
	if err := os.WriteFile(f, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := *angle2Flag
	*angle2Flag = f
	t.Cleanup(func() { *angle2Flag = old })
	if st, body := get(u, "bytes=4-7"); st != http.StatusPartialContent || body != "4567" {
		t.Errorf("Range: %d %q", st, body)
	}
	if st, body := get(u, ""); st != http.StatusOK || body != "0123456789abcdef" {
		t.Errorf("whole file: %d %q", st, body)
	}
	// unknown angle, a lecture without one, the default scenario: angle.not_found
	for _, bad := range []string{
		"/api/lectures/" + angleLecture + "/angles/angle3?mock=angles",
		"/api/lectures/" + mockLectureStaleChapters + "/angles/" + angleName + "?mock=angles",
		"/api/lectures/" + angleLecture + "/angles/" + angleName + "?mock=1",
	} {
		var body api.ErrorBody
		doJSON(t, "GET", srv.URL+bad, nil, http.StatusNotFound, &body)
		if body.Error.Code != "angle.not_found" || body.Error.Params["name"] == nil {
			t.Errorf("%s: %+v", bad, body.Error)
		}
	}
	if st, _ := get("/api/lectures/nope/angles/"+angleName+"?mock=angles", ""); st != http.StatusNotFound {
		t.Errorf("missing lecture: %d", st)
	}
}

func TestMockAddReplaceRemoveAngle(t *testing.T) {
	fastAngles(t)
	srv := testServer(t)
	codes := catalogCodes(t)
	id := mockLectureStaleChapters
	u := srv.URL + "/api/lectures/" + id + "/angles?mock=1"
	cases := []struct {
		path string
		want int
		code string
	}{
		{"", http.StatusBadRequest, "upload.no_file"},
		{"relative/angle.mp4", http.StatusBadRequest, "upload.path_invalid"},
		{`C:\lectures\missing.mp4`, http.StatusNotFound, "upload.path_not_found"},
		{`C:\lectures\locked.mp4`, http.StatusForbidden, "upload.path_unreadable"},
		{`C:\lectures\audio-only.m4a`, http.StatusBadRequest, "angle.no_video"},
		{`C:\lectures\fail.mkv`, http.StatusBadRequest, "angle.remux_failed"},
	}
	for _, c := range cases {
		var body api.ErrorBody
		doJSON(t, "POST", u, map[string]string{"path": c.path}, c.want, &body)
		if body.Error.Code != c.code {
			t.Errorf("%q: %+v, want %s", c.path, body.Error, c.code)
		}
		if _, ok := codes[c.code]; !ok {
			t.Errorf("%s is not in error_codes.json", c.code)
		}
	}
	var l api.Lecture
	doJSON(t, "POST", u, map[string]string{"path": `C:\lectures\CSC1003-L04-camera2.mp4`}, http.StatusOK, &l)
	if l.ID != id || len(l.Angles) != 1 || l.Angles[0].Name != angleName {
		t.Errorf("added: %+v", l.Angles)
	}
	// it stays, in every scenario sharing the store
	for _, sc := range []string{"1", "angles", "ext_ok"} {
		var again api.Lecture
		doJSON(t, "GET", srv.URL+"/api/lectures/"+id+"?mock="+sc, nil, http.StatusOK, &again)
		if len(again.Angles) != 1 {
			t.Errorf("%s: after adding %+v", sc, again.Angles)
		}
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/nope/angles?mock=1", map[string]string{"path": `C:\x.mp4`}, http.StatusNotFound, nil)

	// web mode: a multipart "file"
	upload := func(lecture, name string, want int) api.Lecture {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte("not really a video"))
		mw.Close()
		res, err := http.Post(srv.URL+"/api/lectures/"+lecture+"/angles?mock=1", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("upload %s: %d %s", name, res.StatusCode, b)
		}
		var out api.Lecture
		json.Unmarshal(b, &out)
		return out
	}
	if l := upload(lectureNoChapters, "camera2.mp4", http.StatusOK); len(l.Angles) != 1 {
		t.Errorf("multipart: %+v", l.Angles)
	}
	upload(lectureNoChapters, "fail.mkv", http.StatusBadRequest)
	// replacing: another POST, still one angle
	if l := upload(lectureNoChapters, "camera2-better.mp4", http.StatusOK); len(l.Angles) != 1 {
		t.Errorf("replaced: %+v", l.Angles)
	}

	// DELETE: gone everywhere; a second time angle.not_found (fresh
	// variables: "angles" is left out when there is none)
	var del api.Lecture
	doJSON(t, "DELETE", srv.URL+"/api/lectures/"+id+"/angles/"+angleName+"?mock=1", nil, http.StatusOK, &del)
	if del.ID != id || len(del.Angles) != 0 {
		t.Errorf("after DELETE: %+v", del.Angles)
	}
	var body api.ErrorBody
	doJSON(t, "DELETE", srv.URL+"/api/lectures/"+id+"/angles/"+angleName+"?mock=1", nil, http.StatusNotFound, &body)
	if body.Error.Code != "angle.not_found" {
		t.Errorf("DELETE again: %+v", body.Error)
	}
	// the scenario's own second angle can be removed as well
	doJSON(t, "DELETE", srv.URL+"/api/lectures/"+angleLecture+"/angles/"+angleName+"?mock=angles", nil, http.StatusOK, nil)
	var after api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+angleLecture+"?mock=angles", nil, http.StatusOK, &after)
	if len(after.Angles) != 0 {
		t.Errorf("scenario angles after DELETE: %+v", after.Angles)
	}
}

func systemChecks(t *testing.T, base, sc string) map[string]api.SystemCheck {
	t.Helper()
	var sys api.System
	doJSON(t, "GET", base+"/api/system?mock="+sc, nil, http.StatusOK, &sys)
	out := map[string]api.SystemCheck{}
	names := mockCheckNames(sys.Checks)
	for _, c := range sys.Checks {
		if _, dup := out[c.Name]; dup {
			t.Errorf("%s: %s twice", sc, c.Name)
		}
		out[c.Name] = c
	}
	if _, ok := out["aria2c"]; ok {
		// aria2c right after ffmpeg; browser_ext before the providers
		if i := slices.Index(names, "aria2c"); i < 1 || names[i-1] != "ffmpeg" {
			t.Errorf("%s: aria2c not after ffmpeg: %v", sc, names)
		}
		if i := slices.Index(names, "browser_ext"); i < 0 || !strings.HasPrefix(names[i+1], "api") {
			t.Errorf("%s: browser_ext not before the providers: %v", sc, names)
		}
	}
	return out
}

func nmStatusOf(t *testing.T, base, sc string) nmhost.Status {
	t.Helper()
	var st nmhost.Status
	doJSON(t, "GET", base+"/api/nmhost?mock="+sc, nil, http.StatusOK, &st)
	return st
}

func TestMockDiagnosticsExtRows(t *testing.T) {
	fastAngles(t)
	srv := testServer(t)
	codes := catalogCodes(t)
	// the default data keeps its rows
	for _, sc := range []string{"1", "diag"} {
		cs := systemChecks(t, srv.URL, sc)
		if _, ok := cs["aria2c"]; ok {
			t.Errorf("%s has aria2c", sc)
		}
		if _, ok := cs["browser_ext"]; ok {
			t.Errorf("%s has browser_ext", sc)
		}
	}
	type row struct{ status, code string }
	want := map[string][2]row{ // aria2c, browser_ext
		"ext_ok":           {{"ok", "check.aria2c_verified"}, {"ok", "check.browser_ext_connected"}},
		"ext_unregistered": {{"ok", "check.aria2c_verified"}, {"warn", "check.browser_ext_not_registered"}},
		"ext_broken":       {{"error", "check.aria2c_mismatch"}, {"warn", "check.browser_ext_host_missing"}},
		"ext_unmanaged":    {{"ok", "check.aria2c_verified"}, {"ok", "check.browser_ext_unmanaged"}},
		"ext_nmoff":        {{"ok", "check.aria2c_verified"}, {"warn", "check.browser_ext_unavailable"}},
		"ext_mac":          {{"ok", "check.aria2c_verified"}, {"warn", "check.browser_ext_blocked"}},
		"ext_dmg":          {{"ok", "check.aria2c_verified"}, {"warn", "check.browser_ext_app_location"}},
	}
	for sc, w := range want {
		cs := systemChecks(t, srv.URL, sc)
		for i, name := range []string{"aria2c", "browser_ext"} {
			c := cs[name]
			if c.Status != w[i].status || c.DetailInfo == nil || c.DetailInfo.Code != w[i].code {
				t.Errorf("%s %s: %+v %+v, want %v", sc, name, c, c.DetailInfo, w[i])
				continue
			}
			wantCatalogCode(t, sc+" "+name, c.DetailInfo, codes)
			if c.HintInfo != nil {
				wantCatalogCode(t, sc+" "+name+" hint", c.HintInfo, codes)
			}
			if c.Status != "ok" && c.HintInfo == nil {
				t.Errorf("%s %s: a problem without a hint", sc, name)
			}
		}
	}
	// ext_ok: three browsers in nmhost's order, a connection
	st := nmStatusOf(t, srv.URL, "ext_ok")
	var browsers []string
	for _, b := range st.Browsers {
		browsers = append(browsers, b.Browser)
	}
	if !st.Managed || !slices.Equal(browsers, nmhost.Browsers) || st.LastConnection == nil || !st.LastConnection.Accepted || st.LastConnection.ExtensionVersion == "" {
		t.Errorf("ext_ok: %+v", st)
	}
	// outside ext_*: ext_ok's status
	if got := nmStatusOf(t, srv.URL, "1"); !got.Managed || len(got.Browsers) != 3 {
		t.Errorf("scenario 1: %+v", got)
	}
	// ext_unregistered: registering fixes it, the row turns ok
	st = nmStatusOf(t, srv.URL, "ext_unregistered")
	if st.LastConnection != nil || st.Browsers[0].State != nmhost.StateNotRegistered {
		t.Errorf("ext_unregistered: %+v", st)
	}
	doJSON(t, "POST", srv.URL+"/api/nmhost/register?mock=ext_unregistered", nil, http.StatusOK, &st)
	if st.Browsers[0].State != nmhost.StateRegistered || st.Browsers[1].State != nmhost.StateRegistered || st.Browsers[2].State != nmhost.StateNotInstalled || st.LastRegister == nil || st.LastRegister.Error != "" {
		t.Errorf("after registering: %+v", st)
	}
	if cs := systemChecks(t, srv.URL, "ext_unregistered"); cs["browser_ext"].Status != "ok" || cs["browser_ext"].DetailInfo.Code != "check.browser_ext_registered" {
		t.Errorf("after registering: %+v", cs["browser_ext"])
	}
	// ext_broken: registering fails (the host program is missing); the
	// reason is the row's detail ("查看详情")
	doJSON(t, "POST", srv.URL+"/api/nmhost/register?mock=ext_broken", nil, http.StatusOK, &st)
	if st.LastRegister == nil || !strings.Contains(st.LastRegister.Error, "host program") || st.HostPresent {
		t.Errorf("ext_broken: %+v", st)
	}
	if c := systemChecks(t, srv.URL, "ext_broken")["browser_ext"]; c.Status != "warn" || !strings.Contains(c.DetailInfo.Detail, "host program") {
		t.Errorf("ext_broken after registering: %+v %+v", c, c.DetailInfo)
	}
	// ext_unmanaged: no registration here; ext_nmoff: no service
	if st := nmStatusOf(t, srv.URL, "ext_unmanaged"); st.Managed || len(st.Browsers) != 0 {
		t.Errorf("ext_unmanaged: %+v", st)
	}
	var body api.ErrorBody
	doJSON(t, "POST", srv.URL+"/api/nmhost/register?mock=ext_unmanaged", nil, http.StatusServiceUnavailable, &body)
	if body.Error.Code != "server.unavailable" {
		t.Errorf("ext_unmanaged register: %+v", body)
	}
	doJSON(t, "GET", srv.URL+"/api/nmhost?mock=ext_nmoff", nil, http.StatusServiceUnavailable, &body)
	if body.Error.Code != "server.unavailable" || body.Error.Params["service"] != "nmhost" {
		t.Errorf("ext_nmoff: %+v", body)
	}
	// ext_mac (mac-fix): registering again changes nothing until the user
	// acts; the result names the blocked and the not yet ready browsers
	st = nmStatusOf(t, srv.URL, "ext_mac")
	if st.Browsers[0].State != nmhost.StateBlocked || st.Browsers[1].State != nmhost.StateNotReady || st.Browsers[2].State != nmhost.StateRegistered {
		t.Errorf("ext_mac: %+v", st.Browsers)
	}
	doJSON(t, "POST", srv.URL+"/api/nmhost/register?mock=ext_mac", nil, http.StatusOK, &st)
	if st.Browsers[0].State != nmhost.StateBlocked || st.LastRegister == nil || !slices.Equal(st.LastRegister.Blocked, []string{"chrome"}) || !slices.Equal(st.LastRegister.NotReady, []string{"edge"}) {
		t.Errorf("ext_mac after registering: %+v", st)
	}
	if st := nmStatusOf(t, srv.URL, "ext_dmg"); st.LauncherBlocked != nmhost.LauncherDiskImage {
		t.Errorf("ext_dmg: %+v", st)
	}
	// the JSON is nmhost.Status as the real handler writes it
	res, err := http.Get(srv.URL + "/api/nmhost?mock=ext_ok")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"managed", "browsers", "host", "host_present", "auto_start", "listening", "last_connection", "last_register"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("GET /api/nmhost has no %q: %v", k, raw)
		}
	}
}

// browserExtRow follows the real API's rules: same status and code for the
// same nmhost.Status.
func TestMockBrowserExtRowRules(t *testing.T) {
	base := func() *nmhost.Status {
		return &nmhost.Status{Managed: true, Listening: true, HostPresent: true, Host: mockHostPath, Browsers: []nmhost.BrowserStatus{
			{Browser: nmhost.BrowserChrome, State: nmhost.StateRegistered}, {Browser: nmhost.BrowserEdge, State: nmhost.StateNotInstalled}, {Browser: nmhost.BrowserFirefox, State: nmhost.StateNotInstalled}}}
	}
	cases := []struct {
		name   string
		change func(*nmhost.Status)
		status string
		code   string
	}{
		{"registered", func(*nmhost.Status) {}, "ok", "check.browser_ext_registered"},
		{"connected", func(s *nmhost.Status) {
			s.LastConnection = &nmhost.HelloInfo{Browser: "chrome", ExtensionVersion: "0.3.0", Accepted: true, At: time.Now()}
		}, "ok", "check.browser_ext_connected"},
		{"protocol", func(s *nmhost.Status) {
			s.LastConnection = &nmhost.HelloInfo{Browser: "chrome", ExtensionVersion: "0.9.0", Protocol: 2, Accepted: false, At: time.Now()}
		}, "warn", "check.browser_ext_protocol"},
		{"no browser", func(s *nmhost.Status) { s.Browsers[0].State = nmhost.StateNotInstalled }, "ok", "check.browser_ext_no_browser"},
		{"not registered", func(s *nmhost.Status) { s.Browsers[0].State = nmhost.StateNotRegistered }, "warn", "check.browser_ext_not_registered"},
		{"host missing", func(s *nmhost.Status) { s.Browsers[0].State = nmhost.StateHostMissing }, "warn", "check.browser_ext_host_missing"},
		{"unmanaged", func(s *nmhost.Status) { s.Managed = false }, "ok", "check.browser_ext_unmanaged"},
		{"blocked", func(s *nmhost.Status) {
			s.Browsers[0].State = nmhost.StateBlocked
			s.Browsers[1].State = nmhost.StateHostMissing
		}, "warn", "check.browser_ext_blocked"},
		{"not ready", func(s *nmhost.Status) { s.Browsers[1].State = nmhost.StateNotReady }, "warn", "check.browser_ext_not_ready"},
		{"not registered before not ready", func(s *nmhost.Status) {
			s.Browsers[1].State = nmhost.StateNotReady
			s.Browsers[2].State = nmhost.StateNotRegistered
		}, "warn", "check.browser_ext_not_registered"},
		{"disk image", func(s *nmhost.Status) { s.LauncherBlocked = nmhost.LauncherDiskImage }, "warn", "check.browser_ext_app_location"},
	}
	for _, c := range cases {
		st := base()
		c.change(st)
		got := browserExtRow(st)
		if got.Status != c.status || got.DetailInfo.Code != c.code {
			t.Errorf("%s: %s %s, want %s %s", c.name, got.Status, got.DetailInfo.Code, c.status, c.code)
		}
	}
	if got := browserExtRow(nil); got.Status != "warn" || got.DetailInfo.Code != "check.browser_ext_unavailable" {
		t.Errorf("no service: %+v", got)
	}
}
