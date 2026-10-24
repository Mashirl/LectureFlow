// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// Example bodies of the component, hardware and env-key endpoints for the
// front end (testdata/api/v3_components.json), written by the real view
// functions from fixed data. Regenerate with
// LECTUREFLOW_UPDATE_EXAMPLES=1 go test ./internal/api -run TestComponentExampleFiles.

func componentExamples() map[string]any {
	statuses := []components.Status{
		{ID: "core", Kind: "binary", Version: "3.0.0", Bundled: true, Installed: true, InstalledVersion: "3.0.0", Available: true, License: "AGPL-3.0-or-later"},
		{ID: "python", Kind: "python-runtime", Version: "1.0.0", Upstream: "CPython 3.12.15", NameKey: "compui.name.python", Installed: true,
			InstalledVersion: "1.0.0", Available: true, License: "PSF-2.0", DownloadSize: 21 << 20, InstalledSize: 160 << 20, DiskUsage: 158 << 20},
		{ID: "asr-cuda", Kind: "venv", Version: "1.0.1", Backend: "faster-whisper", Depends: []string{"python"}, Installed: true,
			InstalledVersion: "1.0.0", UpdateAvailable: true, Available: true, License: "MIT", DownloadSize: 1290 << 20, InstalledSize: 2900 << 20, DiskUsage: 2870 << 20},
		{ID: "model:faster-whisper:large-v3:float16", Kind: "model", Version: "1.0.0", Backend: "faster-whisper", Model: "large-v3",
			Precision: "float16", Depends: []string{"asr-cuda"}, Available: true, License: "MIT", DownloadSize: 3087 << 20, InstalledSize: 3087 << 20,
			Task: "t1761292800000-1"},
		{ID: "asr-mlx", Kind: "venv", Version: "1.0.0", Backend: "mlx", Depends: []string{"python"}, UnavailableReason: "platform", License: "MIT"},
	}
	var comps []ComponentView
	for _, s := range statuses {
		comps = append(comps, componentView(s))
	}
	running := components.TaskStatus{
		ID: "t1761292800000-1", Kind: "install", State: "running", Components: []string{"model:faster-whisper:large-v3:float16"},
		Items: []components.ItemStatus{{ID: "model:faster-whisper:large-v3:float16", Version: "1.0.0", Phase: "downloading",
			Downloaded: 1200 << 20, Total: 3087 << 20, Source: "mirror"}},
		Downloaded: 1200 << 20, Total: 3087 << 20, SpeedBps: 12 << 20, ETASeconds: 158,
		Notices:   []model.ErrorInfo{{Code: components.CodeMirrorFallback, Params: map[string]any{"group": "pypi", "mirror": "tuna", "reason": components.CodeNetwork}}},
		CreatedAt: "2026-10-24T08:00:00Z", UpdatedAt: "2026-10-24T08:01:40Z",
	}
	failed := components.TaskStatus{
		ID: "t1761292700000-0", Kind: "import", State: "failed", Components: []string{},
		Items: []components.ItemStatus{{ID: "tool", Version: "1.0.0", Phase: "failed", Total: 2048, Downloaded: 2048, Source: "offline"}},
		Total: 2048, Downloaded: 2048, ETASeconds: -1,
		Error:     &model.ErrorInfo{Code: components.CodeImportChecksum, Params: map[string]any{"id": "tool"}},
		CreatedAt: "2026-10-24T07:58:20Z", UpdatedAt: "2026-10-24T07:58:21Z",
	}
	tasks := taskViews([]components.TaskStatus{failed, running})
	hw := hwdetect.HW{OS: "windows", Arch: "amd64", CPU: "Intel(R) Core(TM) Ultra 9 285H", RAMMB: 32446, DiskFreeMB: 214500,
		NVIDIA:   []hwdetect.GPU{{Name: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", VRAMMB: 12227, Driver: "592.00", Source: "nvidia-smi"}},
		IntelGPU: true, IntelGPUName: "Intel(R) Arc(TM) 140T GPU", HasBattery: true}
	srv := &Server{}
	hv := srv.hardwareView(hw)
	hv.Recommendation.DownloadBytes = 4398 << 20 // what the manifest's plan gives (no manager here)
	return map[string]any{
		"GET /api/components": ComponentsView{Platform: "windows-x64",
			Manifest:   ComponentsManifestView{AppVersion: "3.0.0", PublishedAt: "2026-10-20T08:00:00Z", UpdateAvailable: true, Serial: 3, Builtin: true},
			Components: comps, Tasks: tasks},
		"POST /api/components/plan": planView(components.Plan{
			Items: []components.PlanItem{
				{ID: "python", Version: "1.0.0", Kind: "python-runtime", Dependency: true, Installed: true},
				{ID: "asr-cuda", Version: "1.0.1", Kind: "venv", Dependency: true, Installed: true},
				{ID: "model:faster-whisper:large-v3:float16", Version: "1.0.0", Kind: "model", DownloadBytes: 3087 << 20, InstalledBytes: 3087 << 20},
			},
			DownloadBytes: 3087 << 20, InstalledBytes: 3087 << 20, RequiredBytes: 3087<<20 + 256<<20, FreeBytes: 214500 << 20, Enough: true}),
		"POST /api/components/install": ComponentTaskResult{Task: tasks[1]},
		"GET /api/components/sources": SourcesView{
			PyPI: SourceChoice{Mode: "mirror", MirrorID: "tuna"}, Models: SourceChoice{Mode: "default"},
			Options: map[string][]SourceOption{
				"pypi":   {{ID: "tuna", Label: "pypi.tuna.tsinghua.edu.cn", NameKey: "compui.source.name.tuna", URL: "https://mirrors.tuna.tsinghua.edu.cn/help/pypi/", Region: "cn"}},
				"models": {{ID: "hf-mirror", Label: "hf-mirror.com", NameKey: "compui.source.name.hf_mirror", URL: "https://hf-mirror.com/", Region: "cn"}},
			},
			SuggestMirror: true,
			Suggested:     SourcesSetting{PyPI: SourceChoice{Mode: "mirror", MirrorID: "tuna"}, Models: SourceChoice{Mode: "mirror", MirrorID: "hf-mirror"}},
		},
		"event: components": ComponentsEvent{Tasks: tasks},
		"GET /api/hardware": hv,
		"GET /api/onboarding/env-keys": []EnvKeyView{
			{ProviderType: "deepseek", Env: "DEEPSEEK_API_KEY", Present: true, Masked: "sk-…abcd"},
			{ProviderType: "gemini", Env: "GEMINI_API_KEY"},
			{ProviderType: "zhipu", Env: "ZHIPUAI_API_KEY"},
		},
	}
}

func TestComponentExampleFiles(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(componentExamples()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "testdata", "api", "v3_components.json")
	if os.Getenv("LECTUREFLOW_UPDATE_EXAMPLES") != "" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), buf.Bytes()) {
		t.Fatalf("%s is out of date: LECTUREFLOW_UPDATE_EXAMPLES=1 go test ./internal/api -run TestComponentExampleFiles", path)
	}
}
