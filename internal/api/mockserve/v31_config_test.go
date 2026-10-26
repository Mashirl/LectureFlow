// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/api"
)

// mockConfig is GET /api/system's config of a scenario, as the page reads it.
func mockConfig(t *testing.T, url string) (map[string]any, []api.ConfigPython, []api.ConfigDevice) {
	t.Helper()
	var sys api.System
	doJSON(t, "GET", url, nil, 200, &sys)
	b, _ := json.Marshal(sys.Config)
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	var py []api.ConfigPython
	pb, _ := json.Marshal(cfg["python"])
	if err := json.Unmarshal(pb, &py); err != nil {
		t.Fatalf("python %s: %v", pb, err)
	}
	var devs []api.ConfigDevice
	db, _ := json.Marshal(cfg["asr_devices"])
	if err := json.Unmarshal(db, &devs); err != nil {
		t.Fatalf("asr_devices %s: %v", db, err)
	}
	return cfg, py, devs
}

func backends(py []api.ConfigPython) []string {
	var out []string
	for _, p := range py {
		out = append(out, p.Backend)
	}
	return out
}

// 3.1.0: the mock's configuration has the real API's shape (internal/api
// configView): the default (Windows web), the Windows desktop app, the Mac
// desktop app with and without its components, a computer without NVIDIA.
func TestMockConfigView(t *testing.T) {
	srv := testServer(t)
	cfg, py, devs := mockConfig(t, srv.URL+"/api/system")
	wantKeys := []string{"asr", "asr_devices", "data_dir", "ffmpeg", "general", "listen", "log_level", "nvidia_smi", "python", "worker_dir"}
	if got := slices.Sorted(maps.Keys(cfg)); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys %v, want %v", got, wantKeys)
	}
	asr, _ := cfg["asr"].(map[string]any)
	for _, k := range []string{"model", "compute_type", "device", "max_parallel"} {
		if _, ok := asr[k]; ok {
			t.Errorf("asr.%s with the device table", k)
		}
	}
	if asr["vram_reserve_mb"] == nil {
		t.Error("vram_reserve_mb missing (util.js reads it)")
	}
	if got := backends(py); !slices.Equal(got, []string{"faster-whisper", "openvino", "base"}) || py[0].Source != "default" {
		t.Errorf("python %+v", py)
	}
	if len(devs) != 5 || devs[0].ID != "cuda:0" || devs[0].Model == "" || devs[0].Precision == "" {
		t.Errorf("devices %+v", devs)
	}

	_, py, _ = mockConfig(t, srv.URL+"/api/system?mock=cfg_win")
	for _, p := range py {
		if p.Source != "component" || !p.Exists || !strings.Contains(p.Path, `\components\`) {
			t.Errorf("windows desktop %+v", p)
		}
	}

	cfg, py, devs = mockConfig(t, srv.URL+"/api/system?mock=cfg_mac")
	if _, ok := cfg["nvidia_smi"]; ok {
		t.Error("nvidia_smi on a Mac")
	}
	asr, _ = cfg["asr"].(map[string]any)
	if _, ok := asr["vram_reserve_mb"]; ok {
		t.Error("VRAM settings on a Mac")
	}
	if got := backends(py); !slices.Equal(got, []string{"mlx", "base"}) || py[0].Source != "component" {
		t.Errorf("mac python %+v", py)
	}
	if len(devs) != 1 || devs[0].ID != "mlx:gpu" || devs[0].Model != "large-v3-turbo" || devs[0].Precision != "fp16" {
		t.Errorf("mac devices %+v", devs)
	}
	b, _ := json.Marshal(cfg)
	for _, bad := range []string{".venv", "cuda", "openvino", "faster-whisper"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q in the Mac configuration: %s", bad, b)
		}
	}

	_, py, devs = mockConfig(t, srv.URL+"/api/system?mock=cfg_mac_new")
	for _, p := range py {
		if p.Source != "none" || p.Path != "" {
			t.Errorf("nothing installed %+v", p)
		}
	}
	if len(devs) != 0 {
		t.Errorf("devices before anything is installed %+v", devs)
	}

	cfg, _, devs = mockConfig(t, srv.URL+"/api/system?mock=cpuonly")
	if _, ok := cfg["nvidia_smi"]; ok {
		t.Error("nvidia_smi without an NVIDIA card")
	}
	if len(devs) != 1 || devs[0].ID != "fw:cpu" {
		t.Errorf("cpuonly devices %+v", devs)
	}
}
