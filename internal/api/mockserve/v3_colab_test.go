// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
	"lectureflow/internal/model"
)

type colabRes struct {
	Remote  *colab.Status `json:"remote"`
	Devices []struct {
		ID    string `json:"id"`
		Tasks []struct {
			LectureID string `json:"lecture_id"`
		} `json:"tasks"`
	} `json:"devices"`
}

func colabResources(t *testing.T, u, sc string) colabRes {
	t.Helper()
	code, b, _ := mockDo(t, "GET", u+"/api/resources?mock="+sc, "")
	var r colabRes
	if code != 200 || json.Unmarshal(b, &r) != nil {
		t.Fatalf("resources %s: %d %s", sc, code, b)
	}
	return r
}

// lectures of every course, by asr status.
func colabLecturesByASR(t *testing.T, u, sc string) map[string][]string {
	t.Helper()
	code, b, _ := mockDo(t, "GET", u+"/api/courses?mock="+sc, "")
	var cs []api.Course
	if code != 200 || json.Unmarshal(b, &cs) != nil {
		t.Fatalf("courses: %d %s", code, b)
	}
	out := map[string][]string{}
	for _, c := range cs {
		_, b, _ := mockDo(t, "GET", u+"/api/courses/"+c.ID+"/lectures?mock="+sc, "")
		var ls []api.Lecture
		if err := json.Unmarshal(b, &ls); err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			for _, s := range l.Stages {
				if s.Stage == model.StageASR {
					out[s.Status] = append(out[s.Status], l.ID)
				}
			}
		}
	}
	return out
}

func asrStage(t *testing.T, u, sc, id string) api.StageView {
	t.Helper()
	code, b, _ := mockDo(t, "GET", u+"/api/lectures/"+id+"?mock="+sc, "")
	var l api.Lecture
	if code != 200 || json.Unmarshal(b, &l) != nil {
		t.Fatalf("lecture %s: %d %s", id, code, b)
	}
	for _, s := range l.Stages {
		if s.Stage == model.StageASR {
			return s
		}
	}
	t.Fatalf("no asr stage in %s", id)
	return api.StageView{}
}

func TestMockColabResourcesScenarios(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	if r := colabResources(t, u, "1"); r.Remote != nil {
		t.Errorf("default scenario: remote = %+v", r.Remote)
	}
	if r := colabResources(t, u, "colab_off"); r.Remote != nil {
		t.Errorf("colab_off: remote = %+v", r.Remote)
	}
	if r := colabResources(t, u, "colab_none"); r.Remote == nil || r.Remote.State != colab.StateNone || !r.Remote.Configured {
		t.Errorf("colab_none: remote = %+v", r.Remote)
	}
	if r := colabResources(t, u, "colab_queued"); r.Remote == nil || r.Remote.State != colab.StateQueued {
		t.Errorf("colab_queued: remote = %+v", r.Remote)
	}
	r := colabResources(t, u, "colab_running")
	if r.Remote == nil || r.Remote.State != colab.StateRunning || r.Remote.WindowUsedSec < 106*60 || r.Remote.WindowTotalSec != 4*3600 ||
		r.Remote.BalanceCU == nil || r.Remote.Auth.RemainingSec <= 0 || r.Remote.LecturesLeft < 1 {
		t.Fatalf("colab_running: remote = %+v", r.Remote)
	}
	if r.Remote.Current == nil {
		t.Fatal("colab_running: no current lecture (the fixtures have a running asr stage)")
	}
	running := colabLecturesByASR(t, u, "colab_running")[model.StatusRunning]
	var id string
	for _, l := range running {
		if asrJobID(l) == r.Remote.Current.ID {
			id = l
		}
	}
	if id == "" {
		t.Fatalf("current %q is none of the running lectures %v", r.Remote.Current.ID, running)
	}
	for _, d := range r.Devices {
		for _, tk := range d.Tasks {
			if tk.LectureID == id {
				t.Errorf("the remote lecture is still a task of %s", d.ID)
			}
		}
	}
	st := asrStage(t, u, "colab_running", id)
	if st.Stats.Remote == nil || len(st.Stats.Remote.Steps) != 5 || st.Stats.Remote.Accelerator != "T4" || st.Model == nil || st.Model.ASR == nil || st.Model.ASR.DeviceID != model.DeviceColab {
		t.Errorf("remote asr stage: %+v / %+v", st.Stats.Remote, st.Model)
	}
	if st := asrStage(t, u, "colab_queued", id); st.Stats.Remote == nil || st.Stats.Remote.Step != model.RemoteStepWaitAccelerator || st.Stats.Remote.WaitingFor != model.WaitRemoteQueue {
		t.Errorf("queued remote stage: %+v", st.Stats.Remote)
	}
	if st := asrStage(t, u, "1", id); st.Stats.Remote != nil {
		t.Errorf("default scenario: stats.remote = %+v", st.Stats.Remote)
	}
	// the device list has the colab:remote row while on and authorized
	for sc, want := range map[string]bool{"colab_running": true, "colab_off": false} {
		_, b, _ := mockDo(t, "GET", u+"/api/asr/devices?mock="+sc, "")
		if got := strings.Contains(string(b), `"colab:remote"`); got != want {
			t.Errorf("%s: colab:remote row = %v", sc, got)
		}
	}
}

func TestMockColabSettingsFlow(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	var v api.ColabView
	code, b, _ := mockDo(t, "GET", u+"/api/colab?mock=colab_off", "")
	if code != 200 || json.Unmarshal(b, &v) != nil || v.Settings.Enabled || v.Status.Configured || v.ADC.Detected || !strings.Contains(v.ADC.LoginCommand, "--scopes=") {
		t.Fatalf("colab_off: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "POST", u+"/api/colab/auth/import?mock=colab_off", `{"lifecycle":"days","days":3}`); code != 400 || !strings.Contains(string(b), colab.CodeADCNotFound) {
		t.Errorf("import before the file: %d %s", code, b)
	}
	var chk api.ColabCheckView
	if code, b, _ = mockDo(t, "POST", u+"/api/colab/auth/check?mock=colab_off", ""); code != 200 || json.Unmarshal(b, &chk) != nil || !chk.ADC.Detected || chk.Auth.Present {
		t.Fatalf("check: %d %s", code, b)
	}
	var info colab.CredentialInfo
	if code, b, _ = mockDo(t, "POST", u+"/api/colab/auth/import?mock=colab_off", `{"lifecycle":"days","days":3}`); code != 200 || json.Unmarshal(b, &info) != nil || info.Lifecycle != "3d" || info.RemainingSec < 3*86400-60 {
		t.Fatalf("import: %d %s", code, b)
	}
	if strings.Contains(string(b), "refresh_token") || strings.Contains(string(b), "client_secret") {
		t.Errorf("secret field in %s", b)
	}
	if code, b, _ = mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_off", `{"enabled":true,"tier":"free","max_queue_wait_min":45,"battery_prefer":true}`); code != 200 || json.Unmarshal(b, &v) != nil ||
		!v.Settings.Enabled || v.Settings.Tier != colab.TierFree || v.Settings.MaxQueueWaitMin != 45 || !v.Settings.BatteryPrefer {
		t.Fatalf("settings: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_off", `{"accelerator":"V5E1"}`); code != 400 || !strings.Contains(string(b), colab.CodeSettingsInvalid) {
		t.Errorf("bad accelerator: %d %s", code, b)
	}
	if r := colabResources(t, u, "colab_off"); r.Remote == nil || !r.Remote.Configured || r.Remote.BalanceCU != nil {
		t.Errorf("after switching on (free tier): %+v", r.Remote)
	}
	var d api.ColabDisconnectView
	if code, b, _ = mockDo(t, "DELETE", u+"/api/colab/auth?mock=colab_off", ""); code != 200 || json.Unmarshal(b, &d) != nil || !d.Revoked || len(d.Notes) != 1 || d.Notes[0].Code != "remote.gcloud_signed_out" {
		t.Fatalf("disconnect: %d %s", code, b)
	}
	if r := colabResources(t, u, "colab_off"); r.Remote == nil || r.Remote.Configured {
		t.Errorf("after disconnecting: %+v", r.Remote)
	}
}

func TestMockColabUseAndShutdown(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	by := colabLecturesByASR(t, u, "colab_none")
	if len(by[model.StatusDone]) == 0 || len(by[model.StatusQueued]) == 0 {
		t.Fatalf("fixtures: %v", by)
	}
	done, queued := by[model.StatusDone][0], by[model.StatusQueued][0]
	if code, b, _ := mockDo(t, "POST", u+"/api/lectures/"+queued+"/use-colab?mock=colab_off", ""); code != 409 || !strings.Contains(string(b), `"remote.disabled"`) {
		t.Errorf("off: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/lectures/"+done+"/use-colab?mock=colab_none", ""); code != 409 || !strings.Contains(string(b), "state.conflict") {
		t.Errorf("done: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/lectures/"+queued+"/use-colab?mock=colab_exhausted", ""); code != 409 || !strings.Contains(string(b), `"remote_window"`) {
		t.Errorf("exhausted: %d %s", code, b)
	}
	// colab_none notes the first queued asr stage that Colab handed back
	if st := asrStage(t, u, "colab_none", queued); st.NoteInfo == nil || st.NoteInfo.Code != "remote.back_to_local" {
		t.Errorf("no remote.back_to_local note in colab_none: %+v", st.NoteInfo)
	}
	code, b, _ := mockDo(t, "POST", u+"/api/lectures/"+queued+"/use-colab?mock=colab_none", "")
	var l api.Lecture
	if code != 200 || json.Unmarshal(b, &l) != nil || l.ID != queued {
		t.Fatalf("use-colab: %d %s", code, b)
	}
	if st := asrStage(t, u, "colab_none", queued); st.Model == nil || st.Model.ASR == nil || st.Model.ASR.DeviceID != model.DeviceColab {
		t.Errorf("pinned: %+v", st.Model)
	}

	var v api.ColabView
	if code, b, _ := mockDo(t, "POST", u+"/api/colab/shutdown?mock=colab_running", `{"mode":"later"}`); code != 400 {
		t.Errorf("bad mode: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/colab/shutdown?mock=colab_running", `{"mode":"now"}`); code != 200 || json.Unmarshal(b, &v) != nil || v.Status.State != colab.StateStopping || !v.Held {
		t.Fatalf("shutdown now: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/colab/shutdown?mock=colab_queued", `{"mode":"after_current"}`); code != 200 || json.Unmarshal(b, &v) != nil || !v.Status.ShutdownAfter {
		t.Fatalf("after current: %d %s", code, b)
	}
}

func TestMockColabSSEResources(t *testing.T) {
	srv := testServer(t)
	for sc, want := range map[string]bool{"colab_running": true, "1": false} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/events?mock="+sc, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		sc2 := bufio.NewScanner(res.Body)
		sc2.Buffer(make([]byte, 1<<20), 1<<22)
		deadline := time.Now().Add(5 * time.Second)
		got := false
		for time.Now().Before(deadline) && sc2.Scan() {
			if sc2.Text() != "event: resources" || !sc2.Scan() {
				continue
			}
			data := strings.TrimPrefix(sc2.Text(), "data: ")
			var r colabRes
			if err := json.Unmarshal([]byte(data), &r); err != nil {
				t.Fatalf("%s: bad event %q", sc, data)
			}
			if (r.Remote != nil) != want {
				t.Errorf("%s: remote = %+v", sc, r.Remote)
			}
			got = true
			break
		}
		res.Body.Close()
		if !got {
			t.Errorf("%s: no resources event", sc)
		}
	}
}
