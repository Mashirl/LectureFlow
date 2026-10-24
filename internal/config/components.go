// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"path/filepath"

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
// the configured path. key is the config.toml key of that configured path
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
// the configured python.
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
