// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
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
	// colab-obs: the remote stage names the accelerator borrowed (colab_running: the T4 picked)
	if st.Stats.Remote == nil || len(st.Stats.Remote.Steps) != 5 || st.Stats.Remote.Accelerator != r.Remote.EffectiveAccelerator || st.Stats.Remote.Accelerator != "T4" || st.Model == nil || st.Model.ASR == nil || st.Model.ASR.DeviceID != model.DeviceColab {
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
	if code, b, _ = mockDo(t, "POST", u+"/api/colab/auth/import?mock=colab_off", `{"lifecycle":"days","days":3}`); code != 200 || json.Unmarshal(b, &info) != nil || info.Lifecycle != "3d" || info.RemainingSec < 3*86400-60 ||
		info.Source != colab.SourceGcloud {
		t.Fatalf("import: %d %s", code, b)
	}
	if strings.Contains(string(b), "refresh_token") || strings.Contains(string(b), "client_secret") {
		t.Errorf("secret field in %s", b)
	}
	// colab-obs: a PUT tier is ignored — the tier follows the account, read at
	// once after authorizing (a paid account in the mock)
	if code, b, _ = mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_off", `{"enabled":true,"tier":"free","max_queue_wait_min":45,"battery_prefer":true}`); code != 200 || json.Unmarshal(b, &v) != nil ||
		!v.Settings.Enabled || v.Settings.Tier != colab.TierPaid || v.Status.Account.Level != colab.LevelPaid || v.Settings.MaxQueueWaitMin != 45 || !v.Settings.BatteryPrefer ||
		!v.Settings.MemoryMode {
		t.Fatalf("settings: %d %s", code, b)
	}
	// memory mode: on by default, the switch turns it off
	if code, b, _ = mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_off", `{"memory_mode":false}`); code != 200 || json.Unmarshal(b, &v) != nil ||
		v.Settings.MemoryMode || !v.Settings.Enabled {
		t.Fatalf("memory mode off: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_off", `{"accelerator":"V5E1"}`); code != 400 || !strings.Contains(string(b), colab.CodeSettingsInvalid) {
		t.Errorf("bad accelerator: %d %s", code, b)
	}
	if r := colabResources(t, u, "colab_off"); r.Remote == nil || !r.Remote.Configured || r.Remote.BalanceCU == nil || !r.Remote.Account.ReadOK || r.Remote.Account.ReadAt == nil {
		t.Errorf("after switching on (the account read): %+v", r.Remote)
	}
	var d api.ColabDisconnectView
	if code, b, _ = mockDo(t, "DELETE", u+"/api/colab/auth?mock=colab_off", ""); code != 200 || json.Unmarshal(b, &d) != nil || !d.Revoked || len(d.Notes) != 1 || d.Notes[0].Code != "remote.gcloud_signed_out" {
		t.Fatalf("disconnect: %d %s", code, b)
	}
	if r := colabResources(t, u, "colab_off"); r.Remote == nil || r.Remote.Configured {
		t.Errorf("after disconnecting: %+v", r.Remote)
	}
	// the user's own gcloud was signed out with it: its file needs a new login
	if code, b, _ = mockDo(t, "GET", u+"/api/colab?mock=colab_off", ""); code != 200 || json.Unmarshal(b, &v) != nil || v.ADC.Detected {
		t.Errorf("gcloud's credential after disconnecting: %d %s", code, b)
	}
}

// The pre-authorized scenarios hold a credential of the in-app sign-in
// (source app): disconnecting it has no remote.gcloud_signed_out note and
// leaves the user's own gcloud file alone, as the real server does.
func TestMockColabDisconnectAppCredential(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	var v api.ColabView
	if code, b, _ := mockDo(t, "GET", u+"/api/colab?mock=colab_none", ""); code != 200 || json.Unmarshal(b, &v) != nil ||
		!v.Status.Auth.Present || v.Status.Auth.Source != colab.SourceApp || !strings.Contains(string(b), `"source":"app"`) {
		t.Fatalf("colab_none: %d %s", code, b)
	}
	var d api.ColabDisconnectView
	code, b, _ := mockDo(t, "DELETE", u+"/api/colab/auth?mock=colab_none", "")
	if code != 200 || json.Unmarshal(b, &d) != nil || !d.Revoked || len(d.Notes) != 0 || !strings.Contains(string(b), `"notes":[]`) {
		t.Fatalf("disconnect: %d %s", code, b)
	}
	var after api.ColabView
	if code, b, _ := mockDo(t, "GET", u+"/api/colab?mock=colab_none", ""); code != 200 || json.Unmarshal(b, &after) != nil ||
		after.Status.Auth.Present || after.Status.Auth.Source != "" || !after.ADC.Detected {
		t.Fatalf("after disconnecting: %d %s", code, b)
	}
	// a second disconnect: nothing to revoke, no note
	if code, b, _ := mockDo(t, "DELETE", u+"/api/colab/auth?mock=colab_none", ""); code != 200 || json.Unmarshal(b, &d) != nil || d.Revoked || len(d.Notes) != 0 {
		t.Fatalf("second disconnect: %d %s", code, b)
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

// ---------- colab-obs (colab-obs-web, 2026-10-27) ----------

// obsRes is the resource snapshot's Colab parts: the status and remotes[]
// (internal/remote.Card; the jobs with the lecture titles).
type obsRes struct {
	Remote  *colab.Status `json:"remote"`
	Remotes []struct {
		remote.Card
		Jobs []struct {
			remote.CardJob
			LectureTitle string `json:"lecture_title"`
		} `json:"jobs"`
	} `json:"remotes"`
}

func obsResources(t *testing.T, u, sc string) obsRes {
	t.Helper()
	code, b, _ := mockDo(t, "GET", u+"/api/resources?mock="+sc, "")
	var r obsRes
	if code != 200 || json.Unmarshal(b, &r) != nil || r.Remote == nil {
		t.Fatalf("resources %s: %d %s", sc, code, b)
	}
	if len(r.Remotes) != 1 || r.Remotes[0].Kind != remote.KindColab || r.Remotes[0].ID != model.DeviceColab || r.Remotes[0].State != r.Remote.State {
		t.Fatalf("%s: remotes = %+v", sc, r.Remotes)
	}
	return r
}

func colabGetView(t *testing.T, u, sc string) api.ColabView {
	t.Helper()
	var v api.ColabView
	if code, b, _ := mockDo(t, "GET", u+"/api/colab?mock="+sc, ""); code != 200 || json.Unmarshal(b, &v) != nil {
		t.Fatalf("GET /api/colab %s: %d %s", sc, code, b)
	}
	return v
}

// checkUsage: the memory segments add up to the machine's memory in the
// bar's order, the histories hold 30 points.
func checkUsage(t *testing.T, sc string, m *remote.Machine, u *remote.Usage, kinds []string) {
	t.Helper()
	if m == nil || u == nil || m.MemTotalMB == nil {
		t.Fatalf("%s: machine %+v usage %+v", sc, m, u)
	}
	sum := 0
	var got []string
	for _, s := range u.RAM {
		if s.MB < 0 {
			t.Errorf("%s: segment %+v", sc, s)
		}
		sum += s.MB
		got = append(got, s.Kind)
	}
	if sum != *m.MemTotalMB || !slices.Equal(got, kinds) {
		t.Errorf("%s: segments %v (sum %d, machine %d MB), want %v", sc, u.RAM, sum, *m.MemTotalMB, kinds)
	}
	for name, h := range map[string][]*int{"cpu": u.CPUHistory, "gpu": u.GPUHistory, "vram": u.VRAMHistory} {
		if len(h) != remote.HistoryLen {
			t.Errorf("%s: %s history has %d points", sc, name, len(h))
		}
	}
}

func TestMockColabObsActive(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	r := obsResources(t, u, "colab_active")
	st := r.Remote
	if st.State != colab.StateRunning || st.EffectiveAccelerator != "L4" || st.Accelerator != "L4" || st.LockedReason != "" || st.Slots != 2 ||
		st.UploadedAhead != 1 || len(st.Jobs) != 2 || st.Current == nil || st.Account.Level != colab.LevelPaid || st.Account.PaidBalanceCU == nil ||
		*st.Account.PaidBalanceCU != 286.40 || st.Account.AccountRateCUPerHour == nil || *st.Account.AccountRateCUPerHour <= 0 {
		t.Fatalf("colab_active status: %+v", st)
	}
	m := st.Machine
	if m == nil || m.GPUName != "NVIDIA L4" || m.Requested != "L4" || m.Source != remote.SourceNvidiaSMI || m.VRAMTotalMB == nil || *m.VRAMTotalMB != 23034 || m.Storage != remote.StorageMemory {
		t.Fatalf("machine: %+v", m)
	}
	checkUsage(t, "colab_active", m, st.Usage, []string{remote.SegRAMDisk, remote.SegWhisper, remote.SegSystem, remote.SegData, remote.SegFree})
	if us := st.Usage; us.Source != remote.SourceWorker || us.CPUPct == nil || *us.CPUPct != *us.CPUHistory[remote.HistoryLen-1] || us.GPUUtilPct == nil || us.VRAMUsedMB == nil {
		t.Errorf("usage: %+v", us)
	}
	// the data segment is narrow (about 0.9%): the bar keeps 3 px for it
	for _, s := range st.Usage.RAM {
		if s.Kind == remote.SegData && float64(s.MB)/float64(*m.MemTotalMB) > 0.012 {
			t.Errorf("data segment %d MB is not the narrow one the bar test needs", s.MB)
		}
	}
	card := r.Remotes[0]
	if card.Machine == nil || card.Usage == nil || card.Queued != 3 || card.UploadedAhead != 1 || card.Slots != 2 || len(card.Jobs) != 2 {
		t.Fatalf("card: %+v", card)
	}
	for _, j := range card.Jobs {
		if j.LectureTitle == "" || j.LectureID == "" || j.Progress == nil || j.Stage == "" {
			t.Errorf("card job: %+v", j)
		}
		// both lectures show their asr running on Colab
		if s := asrStage(t, u, "colab_active", j.LectureID); s.Status != model.StatusRunning || s.Stats.Remote == nil || s.Stats.Remote.Accelerator != "L4" {
			t.Errorf("%s: asr %s %+v", j.LectureID, s.Status, s.Stats.Remote)
		}
	}
	// a done lecture was transcribed in speed mode
	found := 0
	for _, id := range colabLecturesByASR(t, u, "colab_active")[model.StatusDone] {
		code, b, _ := mockDo(t, "GET", u+"/api/lectures/"+id+"?mock=colab_active", "")
		var l struct {
			Stages []struct {
				Stage     string `json:"stage"`
				ASRTiming *struct {
					Postprocess *struct {
						Split *struct {
							Parts  int   `json:"parts"`
							CutsMS []int `json:"cuts_ms"`
						} `json:"split"`
					} `json:"postprocess"`
				} `json:"asr_timing"`
			} `json:"stages"`
		}
		if code != 200 || json.Unmarshal(b, &l) != nil {
			t.Fatalf("lecture %s: %d", id, code)
		}
		for _, s := range l.Stages {
			if s.Stage == model.StageASR && s.ASRTiming != nil && s.ASRTiming.Postprocess != nil && s.ASRTiming.Postprocess.Split != nil {
				if sp := s.ASRTiming.Postprocess.Split; sp.Parts != 4 || len(sp.CutsMS) != 3 {
					t.Errorf("split: %+v", sp)
				}
				found++
			}
		}
	}
	if found != 1 {
		t.Errorf("%d lectures with speed mode, want 1", found)
	}
	// the SSE snapshot has remotes[] too
	req, _ := http.NewRequest("GET", u+"/api/events?mock=colab_active", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sc.Scan() {
		if sc.Text() != "event: resources" || !sc.Scan() {
			continue
		}
		var ev obsRes
		if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &ev); err != nil || len(ev.Remotes) != 1 || ev.Remotes[0].Usage == nil || len(ev.Remotes[0].Jobs) != 2 {
			t.Errorf("SSE resources: %v %+v", err, ev.Remotes)
		}
		return
	}
	t.Error("no resources event")
}

func TestMockColabObsAccounts(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	for _, tc := range []struct {
		sc, level, effective, locked, picked string
		known, readOK                        bool
		selectable                           []string
	}{
		{"colab_idle", colab.LevelPaid, "L4", "", "L4", true, true, []string{"T4", "L4", "A100", "G4"}},
		{"colab_unknown", colab.LevelFree, "T4", colab.LockAccountUnknown, "L4", false, false, []string{"T4"}},
		{"colab_free", colab.LevelFree, "T4", "", "T4", true, true, []string{"T4"}},
		{"colab_locked", colab.LevelFree, "T4", colab.LockNoBalance, "A100", true, true, []string{"T4"}},
	} {
		r := obsResources(t, u, tc.sc)
		st := r.Remote
		a := st.Account
		if st.State != colab.StateNone || st.Machine != nil || st.Usage != nil || r.Remotes[0].Machine != nil {
			t.Errorf("%s: an idle runtime with readings: %+v", tc.sc, st)
		}
		if a.Level != tc.level || a.EligibilityKnown != tc.known || a.ReadOK != tc.readOK || st.EffectiveAccelerator != tc.effective || st.Accelerator != tc.effective ||
			st.LockedReason != tc.locked || !slices.Equal(st.SelectableAccelerators, tc.selectable) {
			t.Errorf("%s: account %+v / effective %s locked %q selectable %v", tc.sc, a, st.EffectiveAccelerator, st.LockedReason, st.SelectableAccelerators)
		}
		// the pick is kept (the settings page shows it), the tier follows the account
		v := colabGetView(t, u, tc.sc)
		wantTier := colab.TierFree
		if tc.level == colab.LevelPaid {
			wantTier = colab.TierPaid
		}
		if v.Settings.Accelerator != tc.picked || v.Settings.Tier != wantTier || v.Status.EffectiveAccelerator != tc.effective {
			t.Errorf("%s: settings %+v / effective %s", tc.sc, v.Settings.Settings, v.Status.EffectiveAccelerator)
		}
	}
	// colab_unknown: never read, why; "重新查询" fails again
	st := obsResources(t, u, "colab_unknown").Remote
	if st.Account.ReadAt != nil || st.Account.PaidBalanceCU != nil || st.Account.ReadError == nil || st.Account.ReadError.Code != colab.CodeUnavailable || st.BalanceCU != nil {
		t.Errorf("colab_unknown: %+v", st.Account)
	}
	var v api.ColabView
	if code, b, _ := mockDo(t, "POST", u+"/api/colab/account/refresh?mock=colab_unknown", ""); code != 200 || json.Unmarshal(b, &v) != nil || v.Status.Account.ReadOK || v.Status.Account.ReadError == nil {
		t.Errorf("refresh colab_unknown: %d %s", code, b)
	}
	// colab_free: a free quota with its refill, the T4 not locked
	a := obsResources(t, u, "colab_free").Remote.Account
	if a.PaidBalanceCU == nil || *a.PaidBalanceCU != 0 || a.FreeQuotaCU == nil || *a.FreeQuotaCU != 3.2 || a.FreeRefillAt == nil || !a.FreeRefillAt.After(time.Now()) {
		t.Errorf("colab_free: %+v", a)
	}
	// colab_idle: the H100 is not offered; picked, it is kept and a T4 used
	if !slices.Contains(obsResources(t, u, "colab_idle").Remote.Account.IneligibleGPUs, "H100") {
		t.Error("colab_idle: H100 not ineligible")
	}
	if code, b, _ := mockDo(t, "PUT", u+"/api/colab/settings?mock=colab_idle", `{"accelerator":"H100","tier":"free"}`); code != 200 || json.Unmarshal(b, &v) != nil ||
		v.Settings.Accelerator != "H100" || v.Settings.Tier != colab.TierPaid || v.Status.EffectiveAccelerator != "T4" || v.Status.LockedReason != colab.LockIneligible {
		t.Errorf("pick the H100: %d %s", code, b)
	}
	// "重新查询" reads the account now
	before := colabGetView(t, u, "colab_idle").Status.Account.ReadAt
	if code, b, _ := mockDo(t, "POST", u+"/api/colab/account/refresh?mock=colab_idle", ""); code != 200 || json.Unmarshal(b, &v) != nil || !v.Status.Account.ReadOK ||
		v.Status.Account.ReadAt == nil || before == nil || !v.Status.Account.ReadAt.After(*before) {
		t.Errorf("refresh colab_idle: %d %s", code, b)
	}
}

// Between lectures the runtime reports total and free only (no worker):
// one used segment, no CPU readings. Disk mode: no memory disk, the job
// files on the disk.
func TestMockColabUsageBetweenLectures(t *testing.T) {
	now := time.Now()
	c := newColabMock("colab_active", now)
	u := c.usage(now, 0)
	checkUsage(t, "between lectures", c.machine(now), u, []string{remote.SegUsed, remote.SegFree})
	if u.Source != remote.SourceColabResources || u.CPUPct != nil || slices.ContainsFunc(u.CPUHistory, func(p *int) bool { return p != nil }) {
		t.Errorf("usage: %+v", u)
	}
	c.memoryOff = true
	u = c.usage(now, 2)
	if u.DataDiskMB == nil || c.machine(now).Storage != remote.StorageDisk {
		t.Errorf("disk mode: %+v", u)
	}
	checkUsage(t, "disk mode", c.machine(now), u, []string{remote.SegWhisper, remote.SegSystem, remote.SegFree})
}
