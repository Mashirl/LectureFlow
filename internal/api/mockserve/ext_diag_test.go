// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/resources"
)

// ext-step1: the diagnostics of a pure Intel laptop, an Apple silicon Mac
// and "only Colab" (the rows of internal/api/system_env.go), and the
// transcription mode endpoints.

func mockCheckNames(cs []api.SystemCheck) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// noNVIDIA fails when v names CUDA, cuDNN or NVIDIA.
func noNVIDIA(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	low := strings.ToLower(string(b))
	for _, w := range []string{"cuda", "cudnn", "nvidia"} {
		if i := strings.Index(low, w); i >= 0 {
			t.Errorf("%s names %q: …%s…", what, w, b[max(0, i-60):min(len(b), i+60)])
		}
	}
}

var providerTail = []string{"api_key:DeepSeek", "api:DeepSeek", "api_key:Anthropic", "api:Anthropic", "disk"}

func TestMockDiagEnvironments(t *testing.T) {
	srv := testServer(t)
	codes := catalogCodes(t)
	cases := []struct {
		scenario string
		want     []string
		devices  int // device rows (-1: no device block)
		openvino bool
	}{
		{"diag_intel", append([]string{"data_dir", "ffmpeg", "python", "pypdf", "openvino_venv", "openvino", "openvino_devices", "npu_driver", "cpu_selftest"}, providerTail...), 4, true},
		{"diag_mac", append([]string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "mlx_python", "mlx", "mlx_model", "mlx_selftest", "browser_ext"}, providerTail...), 1, false},
		{"diag_cloud", append([]string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "gcloud", "colab_auth", "colab_reach", "browser_ext"}, providerTail...), -1, false},
		{"cpuonly", append([]string{"data_dir", "ffmpeg", "python", "pypdf", "cpu_selftest"}, providerTail...), 1, false},
	}
	for _, c := range cases {
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system?mock="+c.scenario, nil, http.StatusOK, &sys)
		got := slices.DeleteFunc(mockCheckNames(sys.Checks), func(n string) bool { return strings.HasPrefix(n, "role_model:") })
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: checks %v, want %v", c.scenario, got, c.want)
		}
		for _, ch := range sys.Checks {
			raw := ch.Name == "ffmpeg" || ch.Name == "python" || ch.Name == "mlx_python" || strings.HasSuffix(ch.Name, "_selftest")
			if !raw {
				wantCatalogCode(t, c.scenario+" "+ch.Name, ch.DetailInfo, codes)
			}
			if ch.HintInfo != nil {
				wantCatalogCode(t, c.scenario+" "+ch.Name+" hint", ch.HintInfo, codes)
			}
		}
		noNVIDIA(t, c.scenario+" checks", sys.Checks)
		noNVIDIA(t, c.scenario+" configuration", sys.Config)
		noNVIDIA(t, c.scenario+" devices", sys.Devices)
		noNVIDIA(t, c.scenario+" openvino", sys.OpenVINO)
		if c.devices < 0 && (sys.Devices != nil || sys.Power != nil) || c.devices >= 0 && len(sys.Devices) != c.devices {
			t.Errorf("%s: devices %+v power %+v", c.scenario, sys.Devices, sys.Power)
		}
		if (sys.OpenVINO != nil) != c.openvino || sys.GPU.TotalMB != 0 {
			t.Errorf("%s: openvino %+v gpu %+v", c.scenario, sys.OpenVINO, sys.GPU)
		}
		var res resources.Snapshot
		doJSON(t, "GET", srv.URL+"/api/resources?mock="+c.scenario, nil, http.StatusOK, &res)
		if res.GPU.Available {
			t.Errorf("%s: an NVIDIA card in the resource bar %+v", c.scenario, res.GPU)
		}
		noNVIDIA(t, c.scenario+" resource devices", res.Devices)
	}
	// The reference laptop keeps its NVIDIA rows, now with cuDNN.
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system", nil, http.StatusOK, &sys)
	if got := strings.Join(mockCheckNames(sys.Checks), ","); !strings.Contains(got, "faster-whisper,pypdf,gpu,cudnn,cuda,openvino_venv") {
		t.Errorf("reference checks %s", got)
	}
}

func TestMockASRMode(t *testing.T) {
	srv := testServer(t)
	var v api.ASRModeView
	doJSON(t, "GET", srv.URL+"/api/asr-mode?mock=diag_intel", nil, http.StatusOK, &v)
	if v != (api.ASRModeView{Mode: "local", Source: "default"}) {
		t.Fatalf("diag_intel %+v", v)
	}
	doJSON(t, "GET", srv.URL+"/api/asr-mode?mock=diag_cloud", nil, http.StatusOK, &v)
	if v != (api.ASRModeView{Mode: "remote", Source: "onboarding"}) {
		t.Fatalf("diag_cloud %+v", v)
	}
	wantCode(t, "PUT", srv.URL+"/api/asr-mode?mock=diag_intel", map[string]any{"mode": "cloud"}, 400, "request.bad_param")
	// "only Colab" in the Intel scenario: the diagnostics of diag_cloud
	doJSON(t, "PUT", srv.URL+"/api/asr-mode?mock=diag_intel", map[string]any{"mode": "remote"}, http.StatusOK, &v)
	if v != (api.ASRModeView{Mode: "remote", Source: "settings"}) {
		t.Fatalf("put %+v", v)
	}
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system?mock=diag_intel", nil, http.StatusOK, &sys)
	if names := mockCheckNames(sys.Checks); !slices.Contains(names, "colab_reach") || slices.Contains(names, "cpu_selftest") || sys.Devices != nil {
		t.Errorf("remote diag_intel %v devices %v", names, sys.Devices)
	}
	// other scenarios keep their own mode
	doJSON(t, "GET", srv.URL+"/api/asr-mode?mock=diag_mac", nil, http.StatusOK, &v)
	if v.Mode != "local" {
		t.Errorf("diag_mac %+v", v)
	}
}
