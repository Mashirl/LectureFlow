// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The read-only configuration of the diagnostics page as the real API
// sends it since 3.1.0 (internal/api configView): the interpreter of each
// back end of the platform with its source (api.ConfigPython; "none"
// without a path), the devices' own model / precision / parallel tasks
// (asr_devices, from the store's device rows) instead of the global
// model, compute_type, device and max_parallel, and nvidia_smi with the
// VRAM settings only with an NVIDIA card (not in cpuonly,
// power_ac_intel, on a Mac).
//
// The default data is the reference Windows machine in web mode (the
// development .venv defaults, testdata/api/mock_system_ok.json). Scenarios:
//
//	cfg_win      the Windows desktop app on the same machine: the
//	             components' interpreters, its data folder and resources
//	cfg_mac      the Mac desktop app (Apple silicon, data on an external
//	             disk): MLX and the python component installed, mlx:gpu
//	cfg_mac_new  the same before anything is installed: both interpreters
//	             "not installed", no device

import (
	"maps"
	"net/http"
	"sort"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

const (
	cfgWinComponents = setDataDir + `\components`
	cfgWinResources  = `C:\Program Files\LectureFlow\resources`
	cfgMacDataDir    = "/Volumes/Data/LectureFlow"
	cfgMacResources  = "/Applications/LectureFlow.app/Contents/Resources"
	cfgMacGPU        = "Apple M4"
)

// mockConfigView rewrites the configuration of the example data (sys
// holds the store's devices already).
func mockConfigView(r *http.Request, s *store, sys *api.System) any {
	base, ok := sys.Config.(map[string]any)
	if !ok {
		return sys.Config
	}
	out := maps.Clone(base)
	asr, _ := base["asr"].(map[string]any)
	asr = maps.Clone(asr)
	if asr == nil {
		asr = map[string]any{}
	}
	sc := scenario(r)
	mac := sc == "cfg_mac" || sc == "cfg_mac_new"
	if mac || s.dev.cpuOnly || s.dev.noCUDA {
		delete(out, "nvidia_smi")
		delete(asr, "vram_reserve_mb")
		delete(asr, "vram_per_job_mb")
	}
	out["asr"] = asr
	devs := make([]api.ConfigDevice, 0, len(sys.Devices))
	views := append([]api.ASRDeviceView(nil), sys.Devices...)
	sort.SliceStable(views, func(i, j int) bool { return views[i].Priority > views[j].Priority })
	for _, d := range views {
		devs = append(devs, api.ConfigDevice{ID: d.ID, Label: d.Label, Backend: d.Backend, Model: d.Model, Precision: d.Precision,
			MaxParallel: d.MaxParallel, Enabled: d.Enabled})
	}
	switch sc {
	case "cfg_win":
		out["data_dir"] = setDataDir
		out["ffmpeg"] = cfgWinResources + `\bin\ffmpeg.exe`
		out["worker_dir"] = cfgWinResources + `\worker`
		out["python"] = []api.ConfigPython{
			{Backend: model.BackendFasterWhisper, Path: cfgWinComponents + `\asr-cuda\1.1.1-1\venv\Scripts\python.exe`, Source: "component", Exists: true},
			{Backend: model.BackendOpenVINO, Path: cfgWinComponents + `\asr-openvino\2025.3-1\venv\Scripts\python.exe`, Source: "component", Exists: true},
			{Backend: "base", Path: cfgWinComponents + `\python\3.12.8-1\venv\Scripts\python.exe`, Source: "component", Exists: true},
		}
	case "cfg_mac", "cfg_mac_new":
		out["data_dir"] = cfgMacDataDir
		out["ffmpeg"] = cfgMacResources + "/bin/ffmpeg"
		out["worker_dir"] = cfgMacResources + "/worker"
		if sc == "cfg_mac" {
			out["python"] = []api.ConfigPython{
				{Backend: model.BackendMLX, Path: cfgMacDataDir + "/components/asr-mlx/0.4.2-1/venv/bin/python", Source: "component", Exists: true},
				{Backend: "base", Path: cfgMacDataDir + "/components/python/3.12.8-1/venv/bin/python", Source: "component", Exists: true},
			}
			devs = []api.ConfigDevice{{ID: model.DeviceMLXGPU, Label: cfgMacGPU, Backend: model.BackendMLX, Model: "large-v3-turbo",
				Precision: model.PrecisionFP16, MaxParallel: 1, Enabled: true}}
		} else {
			out["python"] = []api.ConfigPython{
				{Backend: model.BackendMLX, Source: "none"},
				{Backend: "base", Source: "none"},
			}
			devs = []api.ConfigDevice{}
		}
	}
	out["asr_devices"] = devs
	return out
}
