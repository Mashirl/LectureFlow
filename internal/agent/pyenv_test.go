// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

import (
	"slices"
	"testing"

	"lectureflow/internal/pyenv"
)

// The remote worker and the installer's Python run with pyenv's switches
// (NOTES.md "无遥测") and keep the agent's own variables.
func TestWorkerEnvWithoutTelemetry(t *testing.T) {
	root := t.TempDir()
	env := workerEnv(Dirs{Root: root, Jobs: t.TempDir()}, t.TempDir())
	if !pyenv.Has(env) {
		t.Fatalf("workerEnv lacks %q: %q", pyenv.TelemetryOff, env)
	}
	for _, kv := range []string{"HF_HUB_OFFLINE=1", "PYTHONNOUSERSITE=1", "PYTHONIOENCODING=utf-8"} {
		if !slices.Contains(env, kv) {
			t.Errorf("workerEnv lost %s: %q", kv, env)
		}
	}
}
