// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
)

func shortLoginClock(t *testing.T) {
	t.Helper()
	w, i := colabLoginWait, colabLoginImport
	colabLoginWait, colabLoginImport = 150*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { colabLoginWait, colabLoginImport = w, i })
}

func installGcloud(t *testing.T, url string) {
	t.Helper()
	var r struct {
		Task obTaskView `json:"task"`
	}
	doJSON(t, "POST", url+"/api/components/install?mock=1", map[string]any{"ids": []string{"gcloud"}}, 202, &r)
	if strings.Join(r.Task.IDs, ",") != "gcloud" {
		t.Fatalf("gcloud task %+v", r.Task)
	}
	waitTask(t, url, r.Task.ID, "done")
}

func waitLogin(t *testing.T, url, sc, want string) colabLoginView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v colabLoginView
		doJSON(t, "GET", url+"/api/colab/auth/login?mock="+sc, nil, 200, &v)
		if v.State == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in never reached %s: %+v", want, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMockGcloudComponent(t *testing.T) {
	shortOnboardingClock(t)
	srv := testServer(t)
	var list obComponentsView
	doJSON(t, "GET", srv.URL+"/api/components?mock=1", nil, 200, &list)
	var g *obComp
	for i := range list.Comps {
		if list.Comps[i].ID == "gcloud" {
			g = &list.Comps[i]
		}
	}
	if g == nil || g.Kind != "tool" || g.NameKey != "compui.name.gcloud" || g.DescKey != "compui.hint.gcloud" || g.State != "available" || len(g.Requires) != 0 {
		t.Fatalf("gcloud component %+v", g)
	}
	var plan obPlanView
	doJSON(t, "POST", srv.URL+"/api/components/plan?mock=1", map[string]any{"ids": []string{"gcloud"}}, 200, &plan)
	if strings.Join(plan.IDs, ",") != "gcloud" || plan.DownloadBytes != g.DownloadBytes || plan.InstallBytes != g.InstallBytes || !plan.EnoughSpace {
		t.Fatalf("gcloud plan %+v", plan)
	}
	start := time.Now()
	installGcloud(t, srv.URL)
	if el := time.Since(start); el > obInstallDuration+3*obPhase+time.Second {
		t.Fatalf("a tool install took %v", el)
	}
	doJSON(t, "GET", srv.URL+"/api/components?mock=1", nil, 200, &list)
	for _, c := range list.Comps {
		if c.ID == "gcloud" && c.State != "installed" {
			t.Fatalf("gcloud after install: %+v", c)
		}
		if c.ID == "python" && c.State != "available" {
			t.Fatalf("installing gcloud installed python: %+v", c)
		}
	}
}

func TestMockColabLogin(t *testing.T) {
	shortOnboardingClock(t)
	shortLoginClock(t)
	srv := testServer(t)
	u := srv.URL
	// no gcloud yet
	code, b := rawCall(t, "POST", u+"/api/colab/auth/login?mock=colab_off", nil)
	if code != 409 || !strings.Contains(string(b), `"colab.gcloud_missing"`) {
		t.Fatalf("login without gcloud: %d %s", code, b)
	}
	var idle colabLoginView
	doJSON(t, "GET", u+"/api/colab/auth/login?mock=colab_off", nil, 200, &idle)
	if idle.State != "idle" || idle.StartedAt != nil || idle.Error != nil {
		t.Fatalf("idle %+v", idle)
	}
	installGcloud(t, u)

	var v colabLoginView
	doJSON(t, "POST", u+"/api/colab/auth/login?mock=colab_off", nil, 202, &v)
	if v.State != "waiting" || !strings.HasPrefix(v.URL, "https://accounts.google.com/") || v.StartedAt == nil {
		t.Fatalf("started %+v", v)
	}
	code, b = rawCall(t, "POST", u+"/api/colab/auth/login?mock=colab_off", nil)
	if code != 409 || !strings.Contains(string(b), `"colab.login_running"`) {
		t.Fatalf("second login: %d %s", code, b)
	}
	waitLogin(t, u, "colab_off", "done")
	var cv api.ColabView
	doJSON(t, "GET", u+"/api/colab?mock=colab_off", nil, 200, &cv)
	if !cv.Status.Configured || !cv.Status.Auth.Present || cv.Status.Auth.Lifecycle != "7d" || cv.Status.Auth.Source != colab.SourceApp {
		t.Fatalf("after the sign-in: %+v", cv.Status)
	}
	// its disconnect does not sign the user's own gcloud out
	var d api.ColabDisconnectView
	doJSON(t, "DELETE", u+"/api/colab/auth?mock=colab_off", nil, 200, &d)
	if !d.Revoked || len(d.Notes) != 0 {
		t.Fatalf("disconnect after the sign-in %+v", d)
	}
	// a finished sign-in can be started again (re-authorize)
	doJSON(t, "POST", u+"/api/colab/auth/login?mock=colab_off", nil, 202, &v)

	// the raw JSON has the frozen field names, null when unset
	_, b = rawCall(t, "GET", u+"/api/colab/auth/login?mock=colab_none", nil)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "url", "started_at", "error"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("ColabLogin without %q: %s", k, b)
		}
	}
	if raw["started_at"] != nil || raw["error"] != nil {
		t.Fatalf("idle ColabLogin: %s", b)
	}
}

func TestMockColabLoginFailCancelNoURL(t *testing.T) {
	shortOnboardingClock(t)
	shortLoginClock(t)
	srv := testServer(t)
	u := srv.URL
	installGcloud(t, u)

	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_fail", nil, 202, nil)
	v := waitLogin(t, u, "ob_login_fail", "failed")
	if v.Error == nil || v.Error.Code != "colab.login_failed" || v.Error.Params["exit_code"] != float64(1) {
		t.Fatalf("failed sign-in %+v", v)
	}
	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_timeout", nil, 202, nil)
	if v = waitLogin(t, u, "ob_login_timeout", "failed"); v.Error == nil || v.Error.Code != "colab.login_timeout" || v.Error.Params["minutes"] != float64(10) {
		t.Fatalf("timed-out sign-in %+v", v)
	}
	var cv api.ColabView
	doJSON(t, "GET", u+"/api/colab?mock=ob_login_fail", nil, 200, &cv)
	if cv.Status.Configured {
		t.Fatal("a failed sign-in authorized Colab")
	}
	// retry after a failure
	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_fail", nil, 202, &v)
	if v.State != "waiting" || v.Error != nil {
		t.Fatalf("retry %+v", v)
	}

	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_wait", nil, 202, &v)
	time.Sleep(colabLoginWait + colabLoginImport + 50*time.Millisecond)
	doJSON(t, "GET", u+"/api/colab/auth/login?mock=ob_login_wait", nil, 200, &v)
	if v.State != "waiting" || v.URL == "" {
		t.Fatalf("ob_login_wait %+v", v)
	}
	doJSON(t, "DELETE", u+"/api/colab/auth/login?mock=ob_login_wait", nil, 200, &v)
	if v.State != "cancelled" {
		t.Fatalf("cancel %+v", v)
	}
	// cancelling again changes nothing
	doJSON(t, "DELETE", u+"/api/colab/auth/login?mock=ob_login_wait", nil, 200, &v)
	if v.State != "cancelled" {
		t.Fatalf("second cancel %+v", v)
	}
	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_wait", nil, 202, &v)
	if v.State != "waiting" {
		t.Fatalf("after a cancel %+v", v)
	}

	doJSON(t, "POST", u+"/api/colab/auth/login?mock=ob_login_nourl", nil, 202, &v)
	if v.State != "waiting" || v.URL != "" {
		t.Fatalf("ob_login_nourl %+v", v)
	}
	// disconnecting forgets the sign-in
	doJSON(t, "DELETE", u+"/api/colab/auth?mock=ob_login_nourl", nil, 200, nil)
	doJSON(t, "GET", u+"/api/colab/auth/login?mock=ob_login_nourl", nil, 200, &v)
	if v.State != "idle" {
		t.Fatalf("after disconnecting %+v", v)
	}
}

func TestMockDataDirCheckAndFreeBytes(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	var dd setDataDirView
	doJSON(t, "GET", u+"/api/app/data-dir?mock=ob_storage", nil, 200, &dd)
	if dd.DataDir != setDataDir || dd.FreeBytes != 118_400_000_000 || !dd.CanMove {
		t.Fatalf("data-dir %+v", dd)
	}
	doJSON(t, "GET", u+"/api/app/data-dir?mock=ob_storage_full", nil, 200, &dd)
	if dd.FreeBytes != 2048*obMB {
		t.Fatalf("full disk %+v", dd)
	}
	var app api.AppInfo
	for _, sc := range []string{"ob_storage", "ob_storage_full", "ob_noasr", "ob_desktop"} {
		doJSON(t, "GET", u+"/api/app?mock="+sc, nil, 200, &app)
		if app.Mode != "desktop" || app.DataDir != setDataDir {
			t.Fatalf("%s: app %+v", sc, app)
		}
	}
	doJSON(t, "GET", u+"/api/app?mock=1", nil, 200, &app)
	if app.Mode != "web" {
		t.Fatalf("plain scenario app %+v", app)
	}

	cases := []struct {
		path string
		ok   bool
		code string
		free int64
	}{
		{`D:\LectureFlow`, true, "", 812_000_000_000},
		{"/Volumes/Samsung SSD/LectureFlow", true, "", 812_000_000_000},
		{`C:\small\LectureFlow`, true, "", 1_500_000_000},
		{`E:\busy`, false, "data_dir.busy", 812_000_000_000},
		{`D:\notempty`, false, "data_dir.target_not_empty", 812_000_000_000},
		{"relative", false, "data_dir.path_invalid", -1},
		{setDataDir, false, "data_dir.same_dir", 118_400_000_000},
	}
	for _, c := range cases {
		var v setCheckView
		doJSON(t, "POST", u+"/api/app/data-dir/check?mock=ob_storage", map[string]any{"path": c.path}, 200, &v)
		if v.Path != c.path || v.OK != c.ok || v.FreeBytes != c.free || (c.code == "") != (v.Error == nil) || (v.Error != nil && v.Error.Code != c.code) {
			t.Errorf("check %s: %+v", c.path, v)
		}
	}
	// the check does not move anything
	doJSON(t, "GET", u+"/api/app/data-dir?mock=ob_storage", nil, 200, &dd)
	if dd.State != "idle" || dd.DataDir != setDataDir {
		t.Fatalf("after checks %+v", dd)
	}
	// an onboarding move copies a fresh install's few bytes
	var mv api.DataDirStatus
	doJSON(t, "POST", u+"/api/app/data-dir?mock=ob_storage", map[string]any{"path": `D:\LectureFlow`}, 202, &mv)
	if mv.State != "copying" || mv.TotalBytes != setFreshSize {
		t.Fatalf("onboarding move %+v", mv)
	}
	var v setCheckView
	doJSON(t, "POST", u+"/api/app/data-dir/check?mock=ob_storage", map[string]any{"path": `E:\LectureFlow`}, 200, &v)
	if v.OK || v.Error == nil || v.Error.Code != "data_dir.in_progress" {
		t.Fatalf("check while copying %+v", v)
	}
}

func TestMockPlanErrorAndSteps(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	_, b := rawCall(t, "GET", u+"/api/hardware?mock=1", nil)
	if !strings.Contains(string(b), `"plan_error":null`) {
		t.Fatalf("hardware without plan_error null: %s", b)
	}
	var hw obHardwareView
	for _, method := range []string{"GET", "POST"} {
		path := "/api/hardware"
		if method == "POST" {
			path += "/detect"
		}
		doJSON(t, method, u+path+"?mock=ob_planerr", nil, 200, &hw)
		if hw.PlanError == nil || hw.PlanError.Code != "components.unknown" || hw.PlanError.Params["id"] != obPlanErrModel {
			t.Fatalf("%s plan_error %+v", method, hw.PlanError)
		}
	}
	code, b := rawCall(t, "POST", u+"/api/components/plan?mock=ob_planerr", map[string]any{"ids": hw.Recommendation.Components})
	if code != 400 || !strings.Contains(string(b), `"components.unknown"`) {
		t.Fatalf("plan of the recommendation: %d %s", code, b)
	}
	// the new step names are accepted
	for _, step := range []string{"asr_mode", "storage"} {
		var st api.OnboardingState
		doJSON(t, "PUT", u+"/api/onboarding?mock=1", map[string]any{"step": step, "done": false, "data": map[string]any{"asr_mode": "remote"}}, 200, &st)
		if st.Step != step || !strings.Contains(string(st.Data), `"asr_mode":"remote"`) {
			t.Fatalf("step %s: %+v %s", step, st, st.Data)
		}
	}
}
