// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/hwdetect"
)

// 3.1.0 M2 (.claude/v31-brief.md §1): when the component manager cannot plan
// the recommendation, GET /api/hardware and POST /api/hardware/detect say
// why in plan_error instead of silently showing the estimate.

func TestHardwarePlanError(t *testing.T) {
	hw := hwdetect.HW{OS: "windows", Arch: "amd64", CPU: "Test CPU", RAMMB: 16384}
	ce := newCompEnv(t, func(_ *Server, cs *compState) {
		cs.detect = func(context.Context, string) (hwdetect.HW, error) { return hw, nil }
	})
	// CPU only: python, asr-openvino, the OpenVINO model; the test manifest
	// has the model only, so python cannot be planned.
	var v HardwareView
	ce.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	if v.PlanError == nil || v.PlanError.Code != components.CodeUnknown || v.PlanError.Params["id"] != "python" {
		t.Fatalf("plan_error %+v", v.PlanError)
	}
	// The size is still what could be planned (the model).
	if v.Recommendation.DownloadBytes != int64(len(ce.content[tcOV])) {
		t.Errorf("download_bytes %d", v.Recommendation.DownloadBytes)
	}
	if msg := checkErrorBody(mustJSONBytes(t, ErrorBody{Error: *v.PlanError})); msg != "" {
		t.Error(msg)
	}
	ce.call("POST", "/api/hardware/detect", nil, http.StatusOK, &v)
	if v.PlanError == nil || v.PlanError.Code != components.CodeUnknown {
		t.Fatalf("detect: plan_error %+v", v.PlanError)
	}

	// Apple Silicon against this Windows manifest: no asr-mlx, no MLX model
	// — nothing can be planned: the error, and the recommendation's estimate.
	hw = hwdetect.HW{OS: "darwin", Arch: "arm64", AppleSilicon: true, Chip: "Apple M6", RAMMB: 16384, UnifiedMemMB: 16384}
	ce.call("POST", "/api/hardware/detect", nil, http.StatusOK, &v)
	if v.PlanError == nil || v.PlanError.Code != components.CodeUnknown {
		t.Fatalf("mac: plan_error %+v", v.PlanError)
	}
	if want := int64(hwdetect.Recommend(hw).DownloadMB) << 20; v.Recommendation.DownloadBytes != want {
		t.Errorf("mac: download_bytes %d, want the estimate %d", v.Recommendation.DownloadBytes, want)
	}
}

// A recommendation the manager can plan as a whole: plan_error null and the
// plan's download size; the errors of Plan itself (here
// components.unavailable) are reported as they are.
func TestHardwarePlanOK(t *testing.T) {
	dir := t.TempDir()
	data := []byte("component bytes")
	file := components.File{Name: "c.bin", URL: "{files}/c.bin", SHA256: shaOf(data), Size: int64(len(data))}
	build := func() map[string]*components.Build {
		return map[string]*components.Build{components.PlatformWindowsX64: {DownloadSize: int64(len(data)), InstalledSize: int64(len(data)),
			Files: []components.File{file}}}
	}
	ovModel := hwdetect.ModelID(hwdetect.ModelBackendOpenVINO, "small", "int8")
	man := components.Manifest{
		Schema: 1, Serial: 1, AppVersion: "3.0.0", Compat: ">=3.0.0 <4.0.0", Published: "2026-10-20T08:00:00Z",
		Sources: map[string]components.SourceGroup{
			"files": {Primary: "files", Default: components.SourceDef{ID: "default", Values: map[string]string{"files": "http://127.0.0.1:1/files"}}},
		},
		Components: []components.Component{
			{ID: components.IDCore, Version: "3.0.0", Kind: components.KindBinary, Bundled: true},
			{ID: hwdetect.ComponentPython, Version: "1.0.0", Kind: components.KindBinary, License: "MIT", Platforms: build()},
			{ID: hwdetect.ComponentASROpenVINO, Version: "1.0.0", Kind: components.KindBinary, License: "MIT", Platforms: build()},
			{ID: ovModel, Version: "1.0.0", Kind: components.KindModel, Backend: "openvino", Model: "small", Precision: "int8",
				License: "MIT", Platforms: build()},
			{ID: hwdetect.ComponentASRMLX, Version: "1.0.0", Kind: components.KindBinary, License: "MIT", Unavailable: "no_hash",
				Platforms: map[string]*components.Build{components.PlatformWindowsX64: {}}},
		},
		Presets: components.Presets{ // what the manifest's validation needs; not used here
			Default: "standard",
			Tiers:   []components.PresetTier{{ID: "standard", NameKey: "n", DescKey: "d", CostKey: "c", TranscribeOnly: true}},
			Providers: []components.PresetProvider{{ID: "gemini", Name: "Google Gemini", Type: "openai",
				BaseURL: "https://gemini.example.invalid/v1beta/openai", DefaultModel: "g", Free: true, TermsNotice: true,
				TermsURL: "https://example.invalid/terms", MaxConcurrency: 1, RPM: 5, SignupURL: "https://example.invalid/s",
				KeysURL: "https://example.invalid/k", StepsKeys: []string{"s1"}, CostKey: "c"}},
			Free: components.FreeChoice{Default: "gemini"},
		},
	}
	b, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	manPath := filepath.Join(dir, "components.json")
	if err := os.WriteFile(manPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := components.New(components.Options{
		DataDir: filepath.Join(dir, "data"), AppVersion: "3.0.0", Platform: components.PlatformWindowsX64, BuiltinManifest: manPath,
		DiskFree: func(string) (int64, error) { return 1 << 40, nil },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	srv := &Server{comps: m}

	v := srv.hardwareView(hwdetect.HW{OS: "windows", Arch: "amd64", CPU: "Test CPU"})
	if v.PlanError != nil || v.Recommendation.DownloadBytes != 3*int64(len(data)) {
		t.Fatalf("plannable: plan_error %+v, download_bytes %d", v.PlanError, v.Recommendation.DownloadBytes)
	}
	body, _ := json.Marshal(v)
	if !bytes.Contains(body, []byte(`"plan_error":null`)) {
		t.Fatalf("plan_error not null: %s", body)
	}

	// The manifest lists asr-mlx as unavailable here: that error, as it is.
	v = srv.hardwareView(hwdetect.HW{OS: "darwin", Arch: "arm64", AppleSilicon: true, UnifiedMemMB: 16384})
	if v.PlanError == nil || v.PlanError.Code != components.CodeUnavailable || v.PlanError.Params["id"] != hwdetect.ComponentASRMLX {
		t.Fatalf("unavailable: plan_error %+v", v.PlanError)
	}
	// python could be planned: its size, not the estimate.
	if v.Recommendation.DownloadBytes != int64(len(data)) {
		t.Errorf("unavailable: download_bytes %d", v.Recommendation.DownloadBytes)
	}
}

// Without a component manager there is no plan and no plan error; the
// field is always present (null).
func TestHardwarePlanErrorWithoutComponents(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.compState().detect = func(context.Context, string) (hwdetect.HW, error) {
			return hwdetect.HW{OS: "windows", Arch: "amd64"}, nil
		}
	})
	res, b := e.do("GET", "/api/hardware", nil)
	if res.StatusCode != http.StatusOK || !bytes.Contains(b, []byte(`"plan_error":null`)) {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}
