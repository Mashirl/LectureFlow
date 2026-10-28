// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/nmhost"
)

type fakeNMHost struct {
	st         nmhost.Status
	registered int
	err        error
}

func (f *fakeNMHost) Status() nmhost.Status { return f.st }

func (f *fakeNMHost) Register() (nmhost.Status, error) {
	f.registered++
	if f.err == nil {
		f.st.LastRegister = &nmhost.RegisterResult{At: time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)}
	}
	return f.st, f.err
}

func TestNMHostEndpoints(t *testing.T) {
	e := newEnv(t)
	// Before cmd hands the service over.
	if info := e.callErrInfo("GET", "/api/nmhost", nil, 503); info.Code != "server.unavailable" || info.Params["service"] != "nmhost" {
		t.Fatalf("GET without the service: %+v", info)
	}
	if info := e.callErrInfo("POST", "/api/nmhost/register", nil, 503); info.Code != "server.unavailable" {
		t.Fatalf("POST without the service: %+v", info)
	}
	e.srv.SetNMHost((*nmhost.Service)(nil)) // a nil service stays "none"
	e.callErrInfo("GET", "/api/nmhost", nil, 503)

	f := &fakeNMHost{st: nmhost.Status{
		Managed: true,
		Browsers: []nmhost.BrowserStatus{
			{Browser: "chrome", State: nmhost.StateRegistered, Manifest: `C:\x\org.lectureflow.nmhost.chromium.json`},
			{Browser: "edge", State: nmhost.StateNotInstalled},
			{Browser: "firefox", State: nmhost.StateHostMissing},
		},
		Host: `C:\LF\resources\bin\lectureflow-nmhost.exe`, HostPresent: true, AutoStart: true, Listening: true,
		LastConnection: &nmhost.HelloInfo{Browser: "chrome", ExtensionVersion: "0.1.0", Protocol: 1, Accepted: true, At: time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)},
	}}
	e.srv.SetNMHost(f)
	var st nmhost.Status
	e.call("GET", "/api/nmhost", nil, 200, &st)
	if !st.Managed || len(st.Browsers) != 3 || st.Browsers[2].State != "host_missing" || st.LastConnection.ExtensionVersion != "0.1.0" || st.LastRegister != nil {
		t.Fatalf("status %+v", st)
	}
	_, raw := e.do("GET", "/api/nmhost", nil)
	for _, key := range []string{`"browsers"`, `"host_present"`, `"auto_start"`, `"listening"`, `"last_connection"`, `"extension_version"`, `"last_register":null`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("no %s in %s", key, raw)
		}
	}

	e.call("POST", "/api/nmhost/register", nil, 200, &st)
	if f.registered != 1 || st.LastRegister == nil {
		t.Fatalf("register: %d %+v", f.registered, st)
	}
	// A failed run is still 200: the status carries the reason.
	f.err = nmhost.ErrHostMissing
	f.st.LastRegister = &nmhost.RegisterResult{Error: "nmhost: the host program is missing"}
	e.call("POST", "/api/nmhost/register", nil, 200, &st)
	if st.LastRegister.Error == "" {
		t.Fatal("failure not reported")
	}
	// Not this instance's registration.
	f.err = nmhost.ErrNotManaged
	e.callErrInfo("POST", "/api/nmhost/register", nil, 503)

	// Another site's page cannot trigger it.
	f.err = nil
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/nmhost/register", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("cross-site: %d", res.StatusCode)
	}
	if f.registered != 3 {
		t.Errorf("registered %d times, want 3 (not for the cross-site request)", f.registered)
	}
	e.srv.SetNMHost(nil)
	e.callErrInfo("GET", "/api/nmhost", nil, 503)
}
