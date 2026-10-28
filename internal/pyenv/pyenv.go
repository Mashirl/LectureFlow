// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package pyenv is what every Python process LectureFlow starts gets on top
// of the service's own environment: the switches that turn off the usage
// reporting ("telemetry") of the libraries the workers load.
//
// Privacy audit before 3.2.0 (NOTES.md "无遥测"): `import openvino` sent a
// Google Analytics event (openvino.tools.ovc through the openvino-telemetry
// package) on every OpenVINO probe and transcription, and nncf reports its
// compression calls the same way. openvino-telemetry has no environment
// switch (only a consent file in the user's profile), so the Python side
// makes the package unimportable instead: openvino and nncf then fall back
// to their built-in no-op stubs. The worker scripts do that themselves
// (worker/asr_common.py telemetry_off); code passed with `python -c` starts
// with BlockTelemetryModules. Hugging Face's reporting has environment
// switches, which TelemetryOff sets.
package pyenv

import (
	"os"
	"runtime"
	"strings"
)

// TelemetryOff are the environment variables (KEY=VALUE) every Python child
// gets; they replace whatever the service's environment says for the same
// keys. worker/asr_common.py TELEMETRY_OFF_ENV and the desktop shell
// (desktop/src/main/lib/serverutil.js NO_TELEMETRY_ENV) carry the same list.
var TelemetryOff = []string{
	// huggingface_hub: no /api/telemetry requests, no package versions or
	// AI-agent detection (and its registry download) in the User-Agent of
	// model downloads; transformers adds "telemetry/off" instead of details.
	"HF_HUB_DISABLE_TELEMETRY=1",
	// huggingface_hub: a token the user saved for other tools is not sent
	// with LectureFlow's model downloads (the models are public).
	"HF_HUB_DISABLE_IMPLICIT_TOKEN=1",
	// hf_xet (huggingface_hub's download engine) reports transfers to its
	// /v1/telemetry endpoint unless switched off. hf_xet 1.7 also honours
	// HF_HUB_DISABLE_TELEMETRY and DO_NOT_TRACK; this is its own switch.
	"HF_XET_TELEMETRY_ENABLED=0",
	// The cross-tool convention (https://consoledonottrack.com); honoured by
	// huggingface_hub and others.
	"DO_NOT_TRACK=1",
}

// BlockTelemetryModules is Python code that makes the telemetry packages
// unimportable in the interpreter that runs it (a None entry in
// sys.modules makes `import` raise ImportError): openvino.tools.ovc and
// nncf then use their no-op stubs. Code passed with `python -c` that may
// import openvino starts with it.
const BlockTelemetryModules = "import sys; sys.modules['openvino_telemetry'] = None"

// ImportCheck is the `python -c` code of an import check of modules:
// BlockTelemetryModules, then `import a, b`.
func ImportCheck(modules []string) string {
	return BlockTelemetryModules + "; import " + strings.Join(modules, ", ")
}

// Env returns base without the keys of TelemetryOff, then TelemetryOff,
// then extra (KEY=VALUE entries, e.g. PYTHONIOENCODING=utf-8).
func Env(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(TelemetryOff)+len(extra))
	for _, kv := range base {
		if !overridden(kv) {
			out = append(out, kv)
		}
	}
	out = append(out, TelemetryOff...)
	return append(out, extra...)
}

// Environ is Env(os.Environ(), extra...): the environment of a Python child.
func Environ(extra ...string) []string { return Env(os.Environ(), extra...) }

// overridden: kv sets one of TelemetryOff's keys (case-insensitively on
// Windows, whose environment keys are).
func overridden(kv string) bool {
	k, _, ok := strings.Cut(kv, "=")
	if !ok || k == "" { // Windows "=C:=C:\..." entries have an empty key
		return false
	}
	for _, t := range TelemetryOff {
		tk, _, _ := strings.Cut(t, "=")
		if k == tk || (runtime.GOOS == "windows" && strings.EqualFold(k, tk)) {
			return true
		}
	}
	return false
}

// Has reports whether env (KEY=VALUE entries; the last entry of a key wins,
// as with os/exec) sets every TelemetryOff entry to its value. For tests of
// the packages that start Python.
func Has(env []string) bool {
	last := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		last[k] = v
	}
	for _, t := range TelemetryOff {
		k, v, _ := strings.Cut(t, "=")
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
		if got, ok := last[k]; !ok || got != v {
			return false
		}
	}
	return true
}
