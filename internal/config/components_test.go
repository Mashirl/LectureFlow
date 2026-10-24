// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"lectureflow/internal/model"
)

// fakeLocator is a component manager with some components installed.
type fakeLocator struct {
	py     map[string]string // backend → interpreter
	models map[string]string // backend/name/precision → dir
}

func (f fakeLocator) Python(backend string) (string, bool) {
	p, ok := f.py[backend]
	return p, ok
}

func (f fakeLocator) ModelDir(backend, name, precision string) (string, bool) {
	d, ok := f.models[backend+"/"+name+"/"+precision]
	return d, ok
}

// SPEC-v3 step 5: an installed component's interpreter takes precedence;
// without it the configured path (development venvs) is used.
func TestWorkerPython(t *testing.T) {
	c := Default()
	c.Python, c.PythonOpenVINO, c.PythonMLX = "/cfg/py", "/cfg/py-ov", "/cfg/py-mlx"
	cases := []struct{ backend, path, key string }{
		{model.BackendFasterWhisper, "/cfg/py", "python"},
		{model.BackendOpenVINO, "/cfg/py-ov", "python_openvino"},
		{model.BackendMLX, "/cfg/py-mlx", "python_mlx"},
		{ComponentBase, "/cfg/py", "python"},
	}
	// no locator at all
	for _, k := range cases {
		if p, key, comp := c.WorkerPython(k.backend); p != k.path || key != k.key || comp {
			t.Errorf("no components, %s: %q %q %v", k.backend, p, key, comp)
		}
	}
	if c.PDFPython() != "/cfg/py" {
		t.Errorf("PDF python without components: %q", c.PDFPython())
	}
	// a locator with nothing installed
	c.Components = fakeLocator{}
	for _, k := range cases {
		if p, key, comp := c.WorkerPython(k.backend); p != k.path || key != k.key || comp {
			t.Errorf("nothing installed, %s: %q %q %v", k.backend, p, key, comp)
		}
	}
	// everything installed
	c.Components = fakeLocator{py: map[string]string{
		model.BackendFasterWhisper: "/c/asr-cuda", model.BackendOpenVINO: "/c/asr-ov",
		model.BackendMLX: "/c/asr-mlx", ComponentBase: "/c/python",
	}}
	want := map[string]string{model.BackendFasterWhisper: "/c/asr-cuda", model.BackendOpenVINO: "/c/asr-ov",
		model.BackendMLX: "/c/asr-mlx", ComponentBase: "/c/python"}
	for _, k := range cases {
		if p, key, comp := c.WorkerPython(k.backend); p != want[k.backend] || key != k.key || !comp {
			t.Errorf("installed, %s: %q %q %v", k.backend, p, key, comp)
		}
	}
	if c.PDFPython() != "/c/python" {
		t.Errorf("PDF python with the python component: %q", c.PDFPython())
	}
	// only the base environment: the ASR backends fall back separately
	c.Components = fakeLocator{py: map[string]string{ComponentBase: "/c/python"}}
	if p, _, comp := c.WorkerPython(model.BackendFasterWhisper); p != "/cfg/py" || comp {
		t.Errorf("faster-whisper must not use the base environment: %q", p)
	}
}

func TestComponentModelDir(t *testing.T) {
	var nilCfg *Config
	if _, ok := nilCfg.ComponentModelDir(model.BackendOpenVINO, "small", "int8"); ok {
		t.Error("nil config has no components")
	}
	c := Default()
	if _, ok := c.ComponentModelDir(model.BackendOpenVINO, "small", "int8"); ok {
		t.Error("no locator")
	}
	c.Components = fakeLocator{models: map[string]string{"openvino/small/int8": "/c/m"}}
	if d, ok := c.ComponentModelDir(model.BackendOpenVINO, "small", "int8"); !ok || d != "/c/m" {
		t.Errorf("installed model: %q %v", d, ok)
	}
	if _, ok := c.ComponentModelDir(model.BackendOpenVINO, "small", "fp16"); ok {
		t.Error("another precision is not installed")
	}
	if _, ok := c.ComponentModelDir(model.BackendOpenVINO, "", "int8"); ok {
		t.Error("no model name")
	}
}

func TestPythonKeyAndDevVenv(t *testing.T) {
	for backend, venv := range map[string]string{model.BackendFasterWhisper: ".venv", ComponentBase: ".venv",
		model.BackendOpenVINO: ".venv-ov", model.BackendMLX: ".venv-mlx"} {
		if got := DevVenv(PythonKey(backend)); got != venv {
			t.Errorf("%s: %s, want %s", backend, got, venv)
		}
	}
}

// The built-in manifest: resources/components.json (over the configured
// value), else the configured one, else the repository's; empty when the
// file does not exist.
func TestComponentsManifest(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ComponentsManifest != "" {
		t.Fatalf("the package directory has no manifest: %q", c.ComponentsManifest)
	}
	prev := DevComponentsManifest
	t.Cleanup(func() { DevComponentsManifest = prev })
	DevComponentsManifest = filepath.Join("..", "..", "packaging", "components", ComponentsManifestFile)
	if c, err = Load(""); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(DevComponentsManifest)
	if c.ComponentsManifest != want {
		t.Fatalf("development manifest %q, want %q", c.ComponentsManifest, want)
	}

	dir := t.TempDir()
	own := filepath.Join(dir, "own.json")
	if err := os.WriteFile(own, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(path, []byte("components_manifest = '"+own+"'\ncomponents_manifest_url = 'https://example.invalid/{channel}/components.json'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if c.ComponentsManifest != own || c.ComponentsManifestURL != "https://example.invalid/{channel}/components.json" {
		t.Fatalf("configured: %q %q", c.ComponentsManifest, c.ComponentsManifestURL)
	}
	res := t.TempDir()
	if err := os.WriteFile(filepath.Join(res, ComponentsManifestFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = LoadWith(path, Overrides{ResourcesDir: res}); err != nil {
		t.Fatal(err)
	}
	if c.ComponentsManifest != filepath.Join(res, ComponentsManifestFile) {
		t.Fatalf("resources manifest %q", c.ComponentsManifest)
	}
	if err := os.WriteFile(path, []byte("components_manifest = 'missing.json'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path); err != nil || c.ComponentsManifest != "" {
		t.Fatalf("missing configured manifest: %q %v", c.ComponentsManifest, err)
	}
}

// uv: the configured / resources one, else the repository's tools/uv
// (absolute; it may not exist).
func TestComponentsUV(t *testing.T) {
	c := Default()
	want, _ := filepath.Abs(DevUV)
	if got := c.ComponentsUV(); got != want {
		t.Fatalf("development uv %q, want %q", got, want)
	}
	c.UV = "/res/bin/uv"
	if got := c.ComponentsUV(); got != "/res/bin/uv" {
		t.Fatalf("configured uv %q", got)
	}
}

// The locator is run-time state: never encoded (GET /api/system's
// config view encodes Redacted as TOML), never decoded.
func TestComponentsNotSerialized(t *testing.T) {
	c := Default()
	c.Components = fakeLocator{py: map[string]string{ComponentBase: "/c/python"}}
	red := c.Redacted()
	if red.Components != nil || c.Components == nil {
		t.Fatal("Redacted must drop the locator only from the copy")
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("/c/python")) || bytes.Contains(buf.Bytes(), []byte("Components")) {
		t.Fatalf("encoded: %s", buf.String())
	}
}
