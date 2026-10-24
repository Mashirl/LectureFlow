// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"testing"
	"time"

	"lectureflow/internal/api"
)

func deviceIDs(vs []api.ASRDeviceView) map[string]bool {
	out := map[string]bool{}
	for _, v := range vs {
		out[v.ID] = true
	}
	return out
}

// ob_staged: the back ends are installed one after the other and the
// device list follows them.
func TestMockStagedInstallDevices(t *testing.T) {
	d, p := obInstallDuration, obPhase
	obInstallDuration, obPhase = 1200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { obInstallDuration, obPhase = d, p })
	srv := testServer(t)

	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=ob_staged", nil, 200, &vs)
	if len(vs) != 0 {
		t.Fatalf("devices before any back end: %v", deviceIDs(vs))
	}
	ids := []string{"python", "asr-cuda", "asr-openvino"}
	var started struct {
		Task obTaskView `json:"task"`
	}
	doJSON(t, "POST", srv.URL+"/api/components/install?mock=ob_staged", map[string]any{"ids": ids}, 202, &started)

	sawCUDAOnly := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=ob_staged", nil, 200, &vs)
		got := deviceIDs(vs)
		if got["cuda:0"] && !got["ov:GPU"] {
			sawCUDAOnly = true
			var list obComponentsView
			doJSON(t, "GET", srv.URL+"/api/components?mock=ob_staged", nil, 200, &list)
			if list.Tasks[0].State != "running" {
				t.Fatalf("task already %s while only CUDA is in", list.Tasks[0].State)
			}
		}
		if got["ov:GPU"] && got["ov:NPU"] {
			if !got["cuda:0"] {
				t.Fatal("CUDA device lost")
			}
			if !sawCUDAOnly {
				t.Fatal("the back ends did not come one after the other")
			}
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("OpenVINO devices never appeared: %v", deviceIDs(vs))
}

// Other scenarios keep the full list.
func TestMockDevicesUnfilteredElsewhere(t *testing.T) {
	srv := testServer(t)
	var vs []api.ASRDeviceView
	doJSON(t, "GET", srv.URL+"/api/asr/devices?mock=1", nil, 200, &vs)
	if got := deviceIDs(vs); !got["cuda:0"] || !got["ov:GPU"] {
		t.Fatalf("devices %v", got)
	}
}

// ob_desktop: desktop mode and the failing component checks.
func TestMockDesktopScenario(t *testing.T) {
	srv := testServer(t)
	var app api.AppInfo
	doJSON(t, "GET", srv.URL+"/api/app?mock=ob_desktop", nil, 200, &app)
	if app.Mode != "desktop" || app.ResourcesDir == "" {
		t.Fatalf("app %+v", app)
	}
	doJSON(t, "GET", srv.URL+"/api/app?mock=1", nil, 200, &app)
	if app.Mode != "web" {
		t.Fatalf("web scenario mode %q", app.Mode)
	}
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system?mock=ob_desktop", nil, 200, &sys)
	want := map[string]string{"python": "check.hint_python", "faster-whisper": "check.hint_pip", "pypdf": "check.hint_pip"}
	for _, c := range sys.Checks {
		if code, ok := want[c.Name]; ok {
			if c.Status != "error" || c.HintInfo == nil || c.HintInfo.Code != code {
				t.Fatalf("check %s: %+v", c.Name, c)
			}
			delete(want, c.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing checks %v", want)
	}
}
