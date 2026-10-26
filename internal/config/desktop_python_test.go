// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// 3.1.0 (Mac acceptance): the desktop application (ResourcesDir set) gets
// no development interpreters. The desktop shell starts the service in the
// resources directory, so the defaults .venv/bin/python etc. used to become
// Resources/.venv/bin/python, which never exists; the diagnostics page
// showed them as the interpreters in use.

var interpBackends = []struct{ backend, key, venv string }{
	{model.BackendFasterWhisper, "python", ".venv"},
	{ComponentBase, "python", ".venv"},
	{model.BackendOpenVINO, "python_openvino", ".venv-ov"},
	{model.BackendMLX, "python_mlx", ".venv-mlx"},
}

// devTail is the end of a development default: .venv/Scripts/python.exe
// on Windows, .venv/bin/python elsewhere.
func devTail(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

// Development (no ResourcesDir): exactly as before, the .venv defaults of
// the working directory, made absolute; their source is "default".
func TestPythonDefaultsDevelopment(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range interpBackends {
		p, key, comp := c.WorkerPython(k.backend)
		want := filepath.Join(cwd, devTail(k.venv))
		if p != want || key != k.key || comp {
			t.Errorf("%s: WorkerPython = %q %q %v, want %q", k.backend, p, key, comp, want)
		}
		if sp, src := c.PythonSource(k.backend); sp != want || src != PythonFromDefault {
			t.Errorf("%s: PythonSource = %q %q", k.backend, sp, src)
		}
	}
	if c.PDFPython() != filepath.Join(cwd, devTail(".venv")) {
		t.Errorf("PDF python %q", c.PDFPython())
	}
	// Default() (no absPaths): the relative defaults, unchanged.
	d := Default()
	if d.Python != devTail(".venv") || d.PythonOpenVINO != devTail(".venv-ov") || d.PythonMLX != devTail(".venv-mlx") {
		t.Errorf("Default(): %q %q %q", d.Python, d.PythonOpenVINO, d.PythonMLX)
	}
	if _, src := d.PythonSource(model.BackendMLX); src != PythonFromDefault {
		t.Errorf("Default() source %q", src)
	}
	// A field the program replaced is no longer the default.
	d.PythonMLX = "/opt/mlx/bin/python"
	if p, src := d.PythonSource(model.BackendMLX); p != "/opt/mlx/bin/python" || src != PythonFromConfig {
		t.Errorf("replaced field: %q %q", p, src)
	}
}

// Desktop (ResourcesDir set), nothing in config.toml: the three keys stay
// empty whatever the working directory (the shell's is the resources
// directory); WorkerPython and PDFPython return "" (not installed) until
// a component is installed.
func TestPythonDefaultsDesktop(t *testing.T) {
	res := t.TempDir()
	t.Chdir(res) // as the desktop shell starts the service
	c, err := LoadWith("", Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	if c.Python != "" || c.PythonOpenVINO != "" || c.PythonMLX != "" {
		t.Fatalf("desktop defaults: python %q python_openvino %q python_mlx %q", c.Python, c.PythonOpenVINO, c.PythonMLX)
	}
	for _, k := range interpBackends {
		if p, key, comp := c.WorkerPython(k.backend); p != "" || key != k.key || comp {
			t.Errorf("%s: WorkerPython = %q %q %v", k.backend, p, key, comp)
		}
		if p, src := c.PythonSource(k.backend); p != "" || src != PythonNotInstalled {
			t.Errorf("%s: PythonSource = %q %q", k.backend, p, src)
		}
	}
	if c.PDFPython() != "" {
		t.Errorf("PDF python %q", c.PDFPython())
	}
	// The other defaults are unchanged.
	if c.NvidiaSMI != exe("nvidia-smi") || c.FFmpeg != exe("ffmpeg") || c.Listen != "127.0.0.1:8420" {
		t.Errorf("other defaults: %+v", c)
	}
	// Installed components are used as before.
	c.Components = fakeLocator{py: map[string]string{ComponentBase: "/c/python", model.BackendMLX: "/c/mlx"}}
	if p, src := c.PythonSource(model.BackendMLX); p != "/c/mlx" || src != PythonFromComponent {
		t.Errorf("mlx component: %q %q", p, src)
	}
	if c.PDFPython() != "/c/python" {
		t.Errorf("PDF python with the component: %q", c.PDFPython())
	}
	if p, src := c.PythonSource(model.BackendOpenVINO); p != "" || src != PythonNotInstalled {
		t.Errorf("openvino without its component: %q %q", p, src)
	}
	// Nothing under the resources directory is named as an interpreter.
	for _, p := range []string{c.Python, c.PythonOpenVINO, c.PythonMLX} {
		if strings.HasPrefix(p, res) {
			t.Errorf("interpreter under the resources directory: %q", p)
		}
	}
}

// Desktop with an interpreter of the user's own in config.toml: it is kept
// (made absolute, .exe on Windows) and reported as config.toml's; the
// keys it does not set stay empty.
func TestPythonDesktopConfigured(t *testing.T) {
	res := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("python_mlx = \"envs/mlx/bin/python\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	c, err := LoadWith(path, Overrides{ResourcesDir: res})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "envs", "mlx", "bin", exe("python"))
	if c.PythonMLX != want {
		t.Fatalf("python_mlx %q, want %q", c.PythonMLX, want)
	}
	if p, src := c.PythonSource(model.BackendMLX); p != want || src != PythonFromConfig {
		t.Errorf("mlx: %q %q", p, src)
	}
	if c.Python != "" || c.PythonOpenVINO != "" {
		t.Errorf("unset keys: %q %q", c.Python, c.PythonOpenVINO)
	}
	// In development the same file keeps python_mlx and defaults the rest.
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, src := c.PythonSource(model.BackendMLX); src != PythonFromConfig || c.PythonMLX != want {
		t.Errorf("development python_mlx %q %q", c.PythonMLX, src)
	}
	if p, src := c.PythonSource(model.BackendFasterWhisper); src != PythonFromDefault || p != filepath.Join(dir, devTail(".venv")) {
		t.Errorf("development python %q %q", p, src)
	}
}
