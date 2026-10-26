// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// 3.1.0 (Mac acceptance): the read-only configuration of the diagnostics
// page shows what is in effect on this computer: the interpreter each
// back end of the platform runs (with its source; "none" without a path),
// nvidia-smi and the VRAM settings only with an NVIDIA GPU, and with the
// device table the devices' own model / precision / parallel tasks instead
// of the legacy global ones.

// configMap is the configuration as the page receives it (JSON).
func configMap(t *testing.T, s *Server) map[string]any {
	t.Helper()
	b, err := json.Marshal(s.configView())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// pythonRows decodes the python list.
func pythonRows(t *testing.T, cfg map[string]any) []ConfigPython {
	t.Helper()
	b, _ := json.Marshal(cfg["python"])
	var rows []ConfigPython
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("python %s: %v", b, err)
	}
	return rows
}

func deviceRows(t *testing.T, cfg map[string]any) []ConfigDevice {
	t.Helper()
	b, _ := json.Marshal(cfg["asr_devices"])
	var rows []ConfigDevice
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("asr_devices %s: %v", b, err)
	}
	return rows
}

// The desktop application's configuration: config.LoadWith with -resources
// leaves the interpreters empty (config TestPythonDefaultsDesktop).
func desktopInterpreters(s *Server) {
	s.desktop = &DesktopOptions{Token: "t", Port: 1}
	s.cfg.Python, s.cfg.PythonOpenVINO, s.cfg.PythonMLX = "", "", ""
}

var (
	// the startup keys on a computer with an NVIDIA GPU and the device table
	configKeysNVIDIA = []string{"asr", "asr_devices", "data_dir", "ffmpeg", "general", "listen", "log_level", "nvidia_smi", "python", "worker_dir"}
	// the asr settings in effect with the device table on such a computer
	asrKeysNVIDIA = []string{"battery_aware", "device_mode", "language", "mem_reserve_mb", "multi_device", "vram_per_job_mb", "vram_reserve_mb"}
	generalKeys   = []string{"daily_budget", "keep_original_media", "notifications"}
)

// The reference Windows machine (NVIDIA + Intel GPU and NPU, web mode):
// the startup keys stay; python / python_openvino became the python list
// (faster-whisper, openvino, base: the development .venv defaults); the
// global model, compute_type, device and max_parallel gave way to
// asr_devices (each device's own); the VRAM settings stay (the scheduler
// uses them for cuda:0; util.js reads vram_reserve_mb).
func TestConfigViewWindowsReference(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	var sys System
	de.call("GET", "/api/system", nil, 200, &sys)
	cfg, _ := sys.Config.(map[string]any)
	if got := sortedKeys(cfg); !slices.Equal(got, configKeysNVIDIA) {
		t.Fatalf("keys %v, want %v", got, configKeysNVIDIA)
	}
	if cfg["listen"] != de.cfg.Listen || cfg["data_dir"] != de.cfg.DataDir || cfg["ffmpeg"] != de.cfg.FFmpeg ||
		cfg["nvidia_smi"] != de.cfg.NvidiaSMI || cfg["worker_dir"] != de.cfg.WorkerDir || cfg["log_level"] != de.cfg.LogLevel {
		t.Errorf("startup keys %v", cfg)
	}
	asr, _ := cfg["asr"].(map[string]any)
	if got := sortedKeys(asr); !slices.Equal(got, asrKeysNVIDIA) {
		t.Errorf("asr keys %v, want %v", got, asrKeysNVIDIA)
	}
	a := de.set.ASR()
	if asr["language"] != a.Language || asr["vram_reserve_mb"] != float64(a.VRAMReserveMB) || asr["vram_per_job_mb"] != float64(a.VRAMPerJobMB) ||
		asr["device_mode"] != a.DeviceMode || asr["mem_reserve_mb"] != float64(a.MemReserveMB) {
		t.Errorf("asr %v (settings %+v)", asr, a)
	}
	if got := sortedKeys(cfg["general"].(map[string]any)); !slices.Equal(got, generalKeys) {
		t.Errorf("general keys %v", got)
	}
	want := []ConfigPython{
		{Backend: model.BackendFasterWhisper, Path: de.cfg.Python, Source: config.PythonFromDefault},
		{Backend: model.BackendOpenVINO, Path: de.cfg.PythonOpenVINO, Source: config.PythonFromDefault},
		{Backend: config.ComponentBase, Path: de.cfg.Python, Source: config.PythonFromDefault},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, want) { // the test's .venv does not exist: exists false
		t.Errorf("python %+v, want %+v", got, want)
	}
	devs := deviceRows(t, cfg)
	wantDevs := []ConfigDevice{
		{ID: model.DeviceCUDA0, Label: nvidiaName, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: "auto", MaxParallel: 1, Enabled: true},
		{ID: model.DeviceOVGPU, Label: igpuName, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Enabled: true},
		{ID: model.DeviceOVNPU, Label: npuName, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Enabled: true},
		{ID: model.DeviceOVCPU, Label: "Intel(R) Core(TM) Ultra 9 386H", Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1, Enabled: false},
	}
	if !slices.Equal(devs, wantDevs) {
		t.Errorf("asr_devices %+v, want %+v", devs, wantDevs)
	}
	// Without the NVIDIA GPU (no cuda:0, nvidia-smi gives no name): no
	// nvidia_smi and no VRAM settings; the rest unchanged.
	de.reg.mu.Lock()
	de.reg.rows = slices.DeleteFunc(de.reg.rows, func(d model.ASRDevice) bool { return d.ID == model.DeviceCUDA0 })
	de.reg.det.CUDAName = ""
	de.reg.mu.Unlock()
	cfg = configMap(t, de.srv)
	if _, ok := cfg["nvidia_smi"]; ok {
		t.Errorf("nvidia_smi without an NVIDIA GPU: %v", cfg)
	}
	asr, _ = cfg["asr"].(map[string]any)
	if _, ok := asr["vram_reserve_mb"]; ok {
		t.Errorf("VRAM settings without an NVIDIA GPU: %v", asr)
	}
	if _, ok := asr["vram_per_job_mb"]; ok {
		t.Errorf("VRAM settings without an NVIDIA GPU: %v", asr)
	}
	if len(deviceRows(t, cfg)) != 3 || len(pythonRows(t, cfg)) != 3 {
		t.Errorf("without NVIDIA: %v", cfg)
	}
}

// Windows desktop: the components' interpreters with their source; a
// component that is not installed has no path (never Resources\.venv).
func TestConfigViewWindowsDesktop(t *testing.T) {
	loc := &mapLocator{py: map[string]string{model.BackendFasterWhisper: compFWPython, config.ComponentBase: compBasePython}}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		desktopInterpreters(s)
		s.cfg.Components = loc
	}})
	cfg := configMap(t, de.srv)
	if got := sortedKeys(cfg); !slices.Equal(got, configKeysNVIDIA) {
		t.Fatalf("keys %v, want %v", got, configKeysNVIDIA)
	}
	want := []ConfigPython{
		{Backend: model.BackendFasterWhisper, Path: compFWPython, Source: config.PythonFromComponent, Exists: true},
		{Backend: model.BackendOpenVINO, Path: "", Source: config.PythonNotInstalled},
		{Backend: config.ComponentBase, Path: compBasePython, Source: config.PythonFromComponent, Exists: true},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, want) {
		t.Errorf("python %+v, want %+v", got, want)
	}
	if b, _ := json.Marshal(cfg); strings.Contains(string(b), ".venv") {
		t.Errorf("a development interpreter in the desktop configuration: %s", b)
	}
	// Nothing installed: every row "none".
	loc.mu.Lock()
	loc.py = map[string]string{}
	loc.mu.Unlock()
	for _, r := range pythonRows(t, configMap(t, de.srv)) {
		if r.Path != "" || r.Source != config.PythonNotInstalled || r.Exists {
			t.Errorf("nothing installed: %+v", r)
		}
	}
}

// macOS desktop (the friend's Mac): MLX and the python component only; no
// nvidia_smi, no VRAM settings, no legacy cuda / large-v3; the device
// table's mlx:gpu with its own model and precision.
func TestConfigViewMacDesktop(t *testing.T) {
	pinGOOS(t, "darwin")
	const basePy = "/Users/u/Library/Application Support/LectureFlow/components/python/3.12.10/venv/bin/python"
	const mlxPy = "/Users/u/Library/Application Support/LectureFlow/components/asr-mlx/1.0.0/venv/bin/python"
	loc := &mapLocator{py: map[string]string{}, models: map[string]string{}}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		desktopInterpreters(s)
		s.cfg.Components = loc
	}})
	macDevices(de, false, false)
	cfg := configMap(t, de.srv)
	wantKeys := []string{"asr", "asr_devices", "data_dir", "ffmpeg", "general", "listen", "log_level", "python", "worker_dir"}
	if got := sortedKeys(cfg); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys %v, want %v", got, wantKeys)
	}
	asr, _ := cfg["asr"].(map[string]any)
	if got, want := sortedKeys(asr), []string{"battery_aware", "device_mode", "language", "mem_reserve_mb", "multi_device"}; !slices.Equal(got, want) {
		t.Errorf("asr keys %v, want %v", got, want)
	}
	// Nothing installed yet: both rows "not installed", no device.
	want := []ConfigPython{
		{Backend: model.BackendMLX, Source: config.PythonNotInstalled},
		{Backend: config.ComponentBase, Source: config.PythonNotInstalled},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, want) {
		t.Errorf("python %+v, want %+v", got, want)
	}
	if devs := deviceRows(t, cfg); len(devs) != 0 {
		t.Errorf("devices %+v", devs)
	}
	// python, asr-mlx and the model installed; mlx:gpu detected.
	loc.install(config.ComponentBase, basePy, "", "")
	loc.install(model.BackendMLX, mlxPy, "", "")
	macDevices(de, true, false)
	cfg = configMap(t, de.srv)
	want = []ConfigPython{
		{Backend: model.BackendMLX, Path: mlxPy, Source: config.PythonFromComponent, Exists: true},
		{Backend: config.ComponentBase, Path: basePy, Source: config.PythonFromComponent, Exists: true},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, want) {
		t.Errorf("python %+v, want %+v", got, want)
	}
	wantDevs := []ConfigDevice{{ID: model.DeviceMLXGPU, Label: "Apple M6", Backend: model.BackendMLX, Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Enabled: true}}
	if devs := deviceRows(t, cfg); !slices.Equal(devs, wantDevs) {
		t.Errorf("devices %+v, want %+v", devs, wantDevs)
	}
	b, _ := json.Marshal(cfg)
	for _, bad := range []string{".venv", "nvidia", "cuda", `"large-v3"`, "faster-whisper", "openvino"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%q in the Mac configuration: %s", bad, b)
		}
	}
}

// Development without the device registry (tests, builds before 3b): the
// legacy global fields are what transcribes and stay, with nvidia_smi and
// the VRAM settings; no asr_devices; the interpreters are the .venv
// defaults.
func TestConfigViewWithoutRegistry(t *testing.T) {
	e := newEnv(t)
	cfg := configMap(t, e.srv)
	wantKeys := []string{"asr", "data_dir", "ffmpeg", "general", "listen", "log_level", "nvidia_smi", "python", "worker_dir"}
	if got := sortedKeys(cfg); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys %v, want %v", got, wantKeys)
	}
	asr, _ := cfg["asr"].(map[string]any)
	wantASR := []string{"battery_aware", "compute_type", "device", "device_mode", "language", "max_parallel", "mem_reserve_mb", "model", "multi_device", "vram_per_job_mb", "vram_reserve_mb"}
	if got := sortedKeys(asr); !slices.Equal(got, wantASR) {
		t.Errorf("asr keys %v, want %v", got, wantASR)
	}
	a := e.set.ASR()
	if asr["model"] != a.Model || asr["device"] != a.Device || asr["compute_type"] != a.ComputeType || asr["max_parallel"] != float64(a.MaxParallel) {
		t.Errorf("legacy asr %v (settings %+v)", asr, a)
	}
	// faster-whisper and the python component: without the registry there
	// are no OpenVINO devices (no OpenVINO field at all, SPEC-v2 I10).
	want := []ConfigPython{
		{Backend: model.BackendFasterWhisper, Path: e.cfg.Python, Source: config.PythonFromDefault},
		{Backend: config.ComponentBase, Path: e.cfg.Python, Source: config.PythonFromDefault},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, want) {
		t.Errorf("python %+v, want %+v", got, want)
	}
}

// macOS in development (web mode): the configured .venv-mlx / .venv
// defaults with their source, marked when the file is missing.
func TestConfigViewMacWeb(t *testing.T) {
	pinGOOS(t, "darwin")
	de := newDevEnv(t, envConfig{})
	macDevices(de, true, false)
	want := []ConfigPython{
		{Backend: model.BackendMLX, Path: de.cfg.PythonMLX, Source: config.PythonFromDefault},
		{Backend: config.ComponentBase, Path: de.cfg.Python, Source: config.PythonFromDefault},
	}
	if got := pythonRows(t, configMap(t, de.srv)); !slices.Equal(got, want) {
		t.Errorf("python %+v, want %+v", got, want)
	}
}

// 3.1.0: a PDF syllabus in the desktop application without the python
// component: 422 worker.python_not_installed (names the component to
// install), not syllabus.pdf_failed with an "exec: no command" detail; no
// process runs.
func TestSyllabusPDFWithoutPythonComponent(t *testing.T) {
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		// the desktop application's interpreters (desktop mode itself
		// would want the token)
		s.cfg.Python, s.cfg.PythonOpenVINO, s.cfg.PythonMLX = "", "", ""
		s.extractPDF = s.runExtractPDF // the real extraction (helpers fake it)
	}})
	c := e.course("C")
	res, b := e.upload("/api/courses/"+c.ID+"/syllabus", nil, "outline.pdf", []byte("%PDF-1.4 fake"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var er ErrorBody
	if err := json.Unmarshal(b, &er); err != nil || er.Error.Code != stages.CodeWorkerPythonNotInstalled || len(er.Error.Params) != 0 {
		t.Fatalf("error %s (%v)", b, err)
	}
	if got := e.srv.RunningPythons(); len(got) != 0 {
		t.Errorf("running %+v", got)
	}
}

// 3.1.0: an empty interpreter (config with a resources directory and no
// component) is never run, in web mode too: the checks say which
// component is missing instead of "无法运行 ：exec: no command".
func TestDiagnosticsEmptyInterpreters(t *testing.T) {
	for _, goos := range []string{"windows", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			pinGOOS(t, goos)
			log := &cmdLog{}
			de := newDevEnv(t, envConfig{server: func(s *Server) {
				s.runCmd = log.run
				s.cfg.Python, s.cfg.PythonOpenVINO, s.cfg.PythonMLX = "", "", ""
			}})
			if goos == "darwin" {
				macDevices(de, false, false)
			} else { // the registry's probe without an interpreter (devices workerProbe)
				de.reg.mu.Lock()
				de.reg.det.Python, de.reg.det.OV = "", nil
				de.reg.det.OVErr = &model.ErrorInfo{Code: devices.CodeOVNotInstalled}
				de.reg.mu.Unlock()
			}
			_, by := diagNow(t, de.env, true)
			wantCheck(t, by["python"], codePythonNotInstalled, codeHintInstallPython)
			wantCheck(t, by["pypdf"], codePythonNotInstalled, codeHintInstallPython)
			if goos == "darwin" {
				wantCheck(t, by[checkMLXPython], codeMLXNotInstalled, codeHintInstallMLX)
			} else {
				wantCheck(t, by["faster-whisper"], codeFWNotInstalled, codeHintInstallFW)
				wantCheck(t, by["cuda"], codeFWNotInstalled, codeHintInstallFW)
				// web mode keeps the developer's hint; the detail names the component
				wantCheck(t, by["openvino_venv"], codeOpenVINONotInstalled, "check.hint_openvino_venv")
			}
			for _, r := range log.list() {
				if strings.HasPrefix(r, " ") || !strings.HasSuffix(r, " -version") {
					t.Errorf("ran %q; want ffmpeg -version only", r)
				}
			}
		})
	}
}
