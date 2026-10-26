// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"lectureflow/internal/model"
)

// SPEC-v3 V3 step 5 (v5-integration-b1): the worker interpreters and the
// model locations come from the component manager when the matching
// component is installed, else from the configured paths (the development
// environments .venv, .venv-ov, .venv-mlx).

// ComponentLocator finds installed components. *components.Manager
// implements it (Python, ModelDir); the interface keeps this package free
// of that dependency and lets tests fake it.
type ComponentLocator interface {
	// Python returns the interpreter of a backend's environment
	// (ComponentBase, model.BackendFasterWhisper, model.BackendOpenVINO,
	// model.BackendMLX); false when that component is not installed.
	Python(backend string) (string, bool)
	// ModelDir returns the directory of an installed model; false when it
	// is not installed.
	ModelDir(backend, name, precision string) (string, bool)
}

// ComponentBase is the backend of the python component's base environment
// (pypdf: the PDF extraction worker), components.BackendBase.
const ComponentBase = "base"

// ComponentsManifestFile is the built-in manifest's name in the resources
// directory (v3-brief §1).
const ComponentsManifestFile = "components.json"

// DevComponentsManifest is where the built-in manifest lives in the
// repository (relative to the working directory; development runs from
// the repository root).
var DevComponentsManifest = filepath.Join("packaging", "components", ComponentsManifestFile)

// DevUV is the uv of the repository (tools/uv, the version the lock files
// were made with); used by ComponentsUV in development.
var DevUV = filepath.Join("tools", "uv", exe("uv"))

// ComponentsUV is the uv the component manager runs: UV (resources/bin/uv
// or the configured one), else DevUV made absolute. The file may not
// exist: installing a component then fails with components.uv_missing,
// the service itself starts normally.
func (c *Config) ComponentsUV() string {
	if c.UV != "" {
		return c.UV
	}
	if abs, err := filepath.Abs(DevUV); err == nil {
		return abs
	}
	return DevUV
}

// WorkerPython returns the interpreter of a backend's worker: the
// component's environment when it is installed (component = true), else
// the configured path ("" in the desktop application when config.toml
// does not set it: not installed). key is the config.toml key of that configured path
// ("python", "python_openvino", "python_mlx"), named in the messages about
// a missing interpreter. Backends: ComponentBase and faster-whisper fall
// back to python, openvino to python_openvino, mlx to python_mlx.
func (c *Config) WorkerPython(backend string) (path, key string, component bool) {
	if c.Components != nil {
		if p, ok := c.Components.Python(backend); ok && p != "" {
			return p, PythonKey(backend), true
		}
	}
	switch backend {
	case model.BackendOpenVINO:
		return c.PythonOpenVINO, PythonKey(backend), false
	case model.BackendMLX:
		return c.PythonMLX, PythonKey(backend), false
	}
	return c.Python, PythonKey(backend), false
}

// Where the interpreter of WorkerPython comes from (PythonSource; the
// diagnostics page's configuration, 3.1.0).
const (
	PythonFromComponent = "component" // the backend's component environment
	PythonFromConfig    = "config"    // the key in config.toml (or a field the program set)
	PythonFromDefault   = "default"   // the development default (.venv, .venv-ov, .venv-mlx)
	PythonNotInstalled  = "none"      // no component and nothing configured (the desktop application)
)

// PythonSource is WorkerPython with where the interpreter comes from: the
// component, config.toml, the development default, or nothing (path "":
// in the desktop application without the component and without the key
// in config.toml; callers treat it as "not installed").
func (c *Config) PythonSource(backend string) (path, source string) {
	p, key, component := c.WorkerPython(backend)
	switch {
	case component:
		return p, PythonFromComponent
	case p == "":
		return "", PythonNotInstalled
	case c.devPython[key] != "" && c.devPython[key] == p:
		return p, PythonFromDefault
	}
	return p, PythonFromConfig
}

// PythonKey is the config.toml key of a backend's configured interpreter.
func PythonKey(backend string) string {
	switch backend {
	case model.BackendOpenVINO:
		return "python_openvino"
	case model.BackendMLX:
		return "python_mlx"
	}
	return "python"
}

// DevVenv is the development environment a config key's default points
// into (.venv, .venv-ov, .venv-mlx), named in the messages about a missing
// interpreter.
func DevVenv(key string) string {
	switch key {
	case "python_openvino":
		return ".venv-ov"
	case "python_mlx":
		return ".venv-mlx"
	}
	return ".venv"
}

// PDFPython is the interpreter of worker/extract_pdf.py: the python
// component's base environment (pypdf, SPEC-v3 V6) when installed, else
// the configured python ("" = not installed, see WorkerPython).
func (c *Config) PDFPython() string {
	p, _, _ := c.WorkerPython(ComponentBase)
	return p
}

// ComponentModelDir returns the directory of an installed model component;
// false without a locator or when the model is not installed.
func (c *Config) ComponentModelDir(backend, name, precision string) (string, bool) {
	if c == nil || c.Components == nil || name == "" {
		return "", false
	}
	dir, ok := c.Components.ModelDir(backend, name, precision)
	return dir, ok && dir != ""
}

// ToolLocator finds installed tool components (3.1.0: gcloud).
// *components.Manager implements it; a ComponentLocator that does not
// (the fakes of other packages' tests) has no tools.
type ToolLocator interface {
	// ToolPath returns the executable of an installed tool component.
	ToolPath(id string) (string, bool)
	// ToolEnv returns the variables ("KEY=value") the tool runs with.
	ToolEnv(id string) []string
}

// GcloudComponent is the id of the Google Cloud CLI component
// (components.IDGcloud).
const GcloudComponent = "gcloud"

// lookPath finds a program on PATH (tests replace it).
var lookPath = exec.LookPath

// GcloudPath returns the gcloud to run: the gcloud component's executable
// when it is installed, else the gcloud on PATH (gcloud.cmd on Windows);
// false when there is neither. Run it with GcloudEnv.
func (c *Config) GcloudPath() (string, bool) {
	if p, ok := c.componentTool(GcloudComponent); ok {
		return p, true
	}
	if p, err := lookPath("gcloud"); err == nil && p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p, true
	}
	return "", false
}

// GcloudEnv returns the environment to run the gcloud of GcloudPath with:
// the process environment without the variables that would make its
// Python load other code (PYTHONHOME, PYTHONPATH, PYTHONSTARTUP,
// PYTHONUSERBASE, VIRTUAL_ENV, CONDA_PREFIX), plus, for the component, its
// variables (on macOS CLOUDSDK_PYTHON = the python component's
// interpreter: Google's macOS archive has no Python; on Windows its
// bundled Python; the update check off). Append your own variables (e.g.
// CLOUDSDK_CONFIG) after these: os/exec keeps the last value of a key.
func (c *Config) GcloudEnv() []string {
	env := toolBaseEnv(os.Environ())
	if _, ok := c.componentTool(GcloudComponent); ok {
		env = append(env, c.Components.(ToolLocator).ToolEnv(GcloudComponent)...)
	}
	return env
}

// componentTool returns an installed tool component's executable.
func (c *Config) componentTool(id string) (string, bool) {
	if c == nil || c.Components == nil {
		return "", false
	}
	tl, ok := c.Components.(ToolLocator)
	if !ok {
		return "", false
	}
	p, ok := tl.ToolPath(id)
	return p, ok && p != ""
}

// toolBaseEnv is components.ToolBaseEnv (this package does not import
// components).
func toolBaseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && k != "" {
			switch strings.ToUpper(k) {
			case "PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONUSERBASE", "VIRTUAL_ENV", "CONDA_PREFIX":
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}
