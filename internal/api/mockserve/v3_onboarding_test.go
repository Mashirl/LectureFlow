// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
)

func shortOnboardingClock(t *testing.T) {
	t.Helper()
	d, p := obInstallDuration, obPhase
	obInstallDuration, obPhase = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { obInstallDuration, obPhase = d, p })
}

func TestMockOnboardingStateAndApp(t *testing.T) {
	srv := testServer(t)
	var app api.AppInfo
	doJSON(t, "GET", srv.URL+"/api/app?mock=empty", nil, 200, &app)
	if app.OnboardingDone {
		t.Fatal("onboarding done before it started")
	}
	var st api.OnboardingState
	doJSON(t, "GET", srv.URL+"/api/onboarding?mock=1", nil, 200, &st)
	if st.Step != "welcome" || st.Done {
		t.Fatalf("initial state %+v", st)
	}
	doJSON(t, "PUT", srv.URL+"/api/onboarding?mock=1", map[string]any{"step": "computer", "done": false, "data": map[string]any{"tier": "standard"}}, 200, &st)
	doJSON(t, "GET", srv.URL+"/api/onboarding?mock=empty", nil, 200, &st)
	if st.Step != "computer" || !strings.Contains(string(st.Data), `"tier":"standard"`) {
		t.Fatalf("saved state %+v %s", st, st.Data)
	}
	doJSON(t, "PUT", srv.URL+"/api/onboarding?mock=1", map[string]any{"step": "nowhere", "done": false}, 400, nil)
	doJSON(t, "PUT", srv.URL+"/api/onboarding?mock=1", map[string]any{"step": "done", "done": "yes"}, 400, nil)
	doJSON(t, "PUT", srv.URL+"/api/onboarding?mock=1", map[string]any{"step": "done", "done": true, "data": map[string]any{}}, 200, &st)
	doJSON(t, "GET", srv.URL+"/api/app?mock=1", nil, 200, &app)
	if !app.OnboardingDone {
		t.Fatal("GET /api/app does not follow onboarding done")
	}
	doJSON(t, "DELETE", srv.URL+"/api/onboarding?mock=1", nil, 204, nil)
	doJSON(t, "GET", srv.URL+"/api/app?mock=1", nil, 200, &app)
	if app.OnboardingDone {
		t.Fatal("reset kept done")
	}
}

func TestMockHardwareAndPlan(t *testing.T) {
	srv := testServer(t)
	var hw obHardwareView
	doJSON(t, "GET", srv.URL+"/api/hardware?mock=1", nil, 200, &hw)
	if len(hw.HW.NVIDIA) != 1 || !hw.HW.IntelGPU || !hw.HW.IntelNPU {
		t.Fatalf("hardware %+v", hw.HW)
	}
	if hw.Recommendation.DownloadBytes <= 0 || hw.Recommendation.Reasons[0].Code != "hw.reason.nvidia" {
		t.Fatalf("recommendation %+v", hw.Recommendation)
	}
	doJSON(t, "POST", srv.URL+"/api/hardware/detect?mock=ob_cpu", nil, 200, &hw)
	if len(hw.HW.NVIDIA) != 0 || hw.Recommendation.Notes[0].Code != "hw.note.cpu_slow" {
		t.Fatalf("cpu scenario %+v", hw)
	}

	var plan obPlanView
	doJSON(t, "POST", srv.URL+"/api/components/plan?mock=1", map[string]any{"ids": []string{"model:faster-whisper:large-v3:float16"}}, 200, &plan)
	if strings.Join(plan.IDs, ",") != "python,asr-cuda,model:faster-whisper:large-v3:float16" || !plan.EnoughSpace || plan.DownloadBytes <= 0 {
		t.Fatalf("plan %+v", plan)
	}
	doJSON(t, "POST", srv.URL+"/api/components/plan?mock=ob_disk", map[string]any{"ids": []string{"asr-cuda"}}, 200, &plan)
	if plan.EnoughSpace || plan.NeedBytes <= plan.FreeBytes {
		t.Fatalf("disk plan %+v", plan)
	}
	doJSON(t, "POST", srv.URL+"/api/components/plan?mock=1", map[string]any{"ids": []string{"asr-mlx"}}, 400, nil)
	doJSON(t, "POST", srv.URL+"/api/components/plan?mock=1", map[string]any{"ids": []string{"nope"}}, 400, nil)
}

func waitTask(t *testing.T, url, id string, want string) obTaskView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var tasks []obTaskView
		doJSON(t, "GET", url+"/api/components/tasks?mock=1", nil, 200, &tasks)
		for _, tk := range tasks {
			if tk.ID == id && tk.State == want {
				return tk
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never reached %s: %+v", id, want, tasks)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMockInstallProgressFailureAndRetry(t *testing.T) {
	shortOnboardingClock(t)
	srv := testServer(t)
	var r struct {
		Task obTaskView `json:"task"`
	}
	doJSON(t, "POST", srv.URL+"/api/components/install?mock=ob_fail", map[string]any{"ids": []string{"model:faster-whisper:medium:int8_float16"}}, 202, &r)
	if r.Task.State != "running" || len(r.Task.Items) != 3 || r.Task.Total <= 0 {
		t.Fatalf("task %+v", r.Task)
	}
	// installing the same components again while the task runs: busy
	doJSON(t, "POST", srv.URL+"/api/components/install?mock=1", map[string]any{"ids": []string{"python"}}, 409, nil)
	failed := waitTask(t, srv.URL, r.Task.ID, "failed")
	if failed.Error == nil || failed.Error.Code != "components.network" || failed.DoneBytes <= 0 || failed.DoneBytes >= failed.Total {
		t.Fatalf("failed task %+v", failed)
	}
	// change the source, retry: the items download from the mirror and finish
	var src obSourcesView
	doJSON(t, "PUT", srv.URL+"/api/components/sources?mock=1", map[string]any{"pypi": map[string]any{"mode": "mirror", "mirror_id": "tuna"}, "models": map[string]any{"mode": "mirror", "mirror_id": "hf-mirror"}}, 200, &src)
	if src.PyPI.Mode != "mirror" || !src.SuggestMirror {
		t.Fatalf("sources %+v", src)
	}
	doJSON(t, "PUT", srv.URL+"/api/components/sources?mock=1", map[string]any{"pypi": map[string]any{"mode": "mirror", "mirror_id": "nope"}, "models": map[string]any{"mode": "default"}}, 400, nil)
	doJSON(t, "POST", srv.URL+"/api/components/tasks/"+r.Task.ID+"/retry?mock=1", nil, 202, &r)
	if r.Task.State != "running" || r.Task.Error != nil || r.Task.Items[0].Source != "mirror" {
		t.Fatalf("retried %+v", r.Task)
	}
	waitTask(t, srv.URL, r.Task.ID, "done")
	var list obComponentsView
	doJSON(t, "GET", srv.URL+"/api/components?mock=1", nil, 200, &list)
	got := map[string]string{}
	for _, c := range list.Comps {
		got[c.ID] = c.State
	}
	if got["python"] != "installed" || got["asr-cuda"] != "installed" || got["model:faster-whisper:medium:int8_float16"] != "installed" || got["asr-mlx"] != "unavailable" || got["asr-openvino"] != "available" {
		t.Fatalf("states %v", got)
	}
	doJSON(t, "POST", srv.URL+"/api/components/tasks/"+r.Task.ID+"/pause?mock=1", nil, 409, nil)
	doJSON(t, "POST", srv.URL+"/api/components/tasks/nope/retry?mock=1", nil, 404, nil)
}

func TestMockInstallPauseMirrorAndDisk(t *testing.T) {
	shortOnboardingClock(t)
	obInstallDuration = 2 * time.Second
	srv := testServer(t)
	var r struct {
		Task obTaskView `json:"task"`
	}
	doJSON(t, "PUT", srv.URL+"/api/components/sources?mock=1", map[string]any{"pypi": map[string]any{"mode": "default"}, "models": map[string]any{"mode": "mirror", "mirror_id": "hf-mirror"}}, 200, nil)
	doJSON(t, "POST", srv.URL+"/api/components/install?mock=ob_mirror", map[string]any{"ids": []string{"asr-openvino"}}, 202, &r)
	if len(r.Task.Notes) != 1 || r.Task.Notes[0].Code != "components.mirror_fallback" || r.Task.Items[0].Source != "default" {
		t.Fatalf("mirror fallback %+v", r.Task)
	}
	doJSON(t, "POST", srv.URL+"/api/components/tasks/"+r.Task.ID+"/pause?mock=1", nil, 202, &r)
	if r.Task.State != "paused" {
		t.Fatalf("pause %+v", r.Task)
	}
	at := r.Task.DoneBytes
	time.Sleep(60 * time.Millisecond)
	doJSON(t, "POST", srv.URL+"/api/components/tasks/"+r.Task.ID+"/resume?mock=1", nil, 202, &r)
	if r.Task.State != "running" || r.Task.DoneBytes < at {
		t.Fatalf("resume %+v (paused at %d)", r.Task, at)
	}
	doJSON(t, "POST", srv.URL+"/api/components/tasks/"+r.Task.ID+"/cancel?mock=1", nil, 202, &r)
	if r.Task.State != "cancelled" {
		t.Fatalf("cancel %+v", r.Task)
	}

	doJSON(t, "POST", srv.URL+"/api/components/install?mock=ob_disk", map[string]any{"ids": []string{"model:faster-whisper:large-v3:float16"}}, 202, &r)
	if r.Task.State != "failed" || r.Task.Error == nil || r.Task.Error.Code != "components.disk_full" {
		t.Fatalf("disk %+v", r.Task)
	}
	if _, ok := r.Task.Error.Params["required_mb"]; !ok {
		t.Fatalf("disk params %+v", r.Task.Error)
	}
}

func TestMockPresetsAndEnvKey(t *testing.T) {
	srv := testServer(t)
	var p obPresets
	doJSON(t, "GET", srv.URL+"/api/components/presets?mock=1", nil, 200, &p)
	ids := []string{}
	for _, tier := range p.Tiers {
		ids = append(ids, tier.ID)
		if tier.TranscribeOnly != (len(tier.Roles) == 0) {
			t.Fatalf("tier %s: transcribe_only and roles disagree", tier.ID)
		}
		for role, r := range tier.Roles {
			if r.Provider != "$free" && r.Provider != "deepseek" {
				t.Fatalf("tier %s role %s: provider %q", tier.ID, role, r.Provider)
			}
		}
	}
	if strings.Join(ids, ",") != "standard,saver,free,transcribe_only" || p.Default != "standard" {
		t.Fatalf("tiers %v default %s", ids, p.Default)
	}
	for _, pr := range p.Providers {
		if pr.Free && (!pr.TermsNotice || pr.TermsURL == "") {
			t.Fatalf("free provider %s without terms notice", pr.ID)
		}
		if len(pr.StepsKeys) == 0 || pr.CostKey == "" || pr.SignupURL == "" || pr.KeysURL == "" {
			t.Fatalf("provider %s incomplete", pr.ID)
		}
	}

	var keys []obEnvKey
	doJSON(t, "GET", srv.URL+"/api/onboarding/env-keys?mock=1", nil, 200, &keys)
	if len(keys) != 1 || !keys[0].Present || keys[0].Masked != "sk-…abcd" {
		t.Fatalf("env keys %+v", keys)
	}
	var none []obEnvKey
	doJSON(t, "GET", srv.URL+"/api/onboarding/env-keys?mock=ob_noenv", nil, 200, &none)
	if len(none) != 1 || none[0].Present || none[0].Masked != "" {
		t.Fatalf("noenv %+v", none)
	}
	var pv api.ProviderView
	doJSON(t, "POST", srv.URL+"/api/onboarding/use-env-key?mock=empty", map[string]any{"provider_type": "deepseek"}, 200, &pv)
	if pv.KeyHint != "sk-…abcd" || !strings.HasPrefix(pv.BaseURL, "https://api.deepseek.com") {
		t.Fatalf("provider %+v", pv)
	}
	b, _ := json.Marshal(pv)
	if strings.Contains(string(b), "api_key") {
		t.Fatalf("a key in the response: %s", b)
	}
	doJSON(t, "POST", srv.URL+"/api/onboarding/use-env-key?mock=ob_noenv", map[string]any{"provider_type": "deepseek"}, 400, nil)
	doJSON(t, "POST", srv.URL+"/api/onboarding/use-env-key?mock=1", map[string]any{"provider_type": "glm-flash"}, 400, nil)
}

func TestMockImport(t *testing.T) {
	srv := testServer(t)
	doJSON(t, "GET", srv.URL+"/api/app/import?mock=1", nil, 404, nil)
	doJSON(t, "POST", srv.URL+"/api/app/import?mock=1", map[string]any{"path": `D:\fail\data`}, 400, nil)
	var v obImportView
	doJSON(t, "POST", srv.URL+"/api/app/import?mock=1", map[string]any{"path": `D:\old\data`}, 202, &v)
	if v.State != "copying" || v.TotalBytes <= 0 {
		t.Fatalf("import %+v", v)
	}
	doJSON(t, "GET", srv.URL+"/api/app/import?mock=1", nil, 200, &v)
	if v.State != "copying" && v.State != "done" {
		t.Fatalf("import status %+v", v)
	}
}
