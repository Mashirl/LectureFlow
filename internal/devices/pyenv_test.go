// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/pyenv"
)

// The OpenVINO probe imports openvino, which reported to Google Analytics:
// it runs with pyenv's switches even when the service's own environment
// turns them off (NOTES.md "无遥测").
func TestProbeRunsWithoutTelemetry(t *testing.T) {
	cfg := probeConfig(t)
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())

	out := filepath.Join(t.TempDir(), "env.json")
	t.Setenv(fakeProbeEnv, "env")
	t.Setenv(fakeProbeEnvOut, out)
	t.Setenv("HF_HUB_DISABLE_TELEMETRY", "0")
	t.Setenv("DO_NOT_TRACK", "0")
	if _, err := r.workerProbe(tctx()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if !pyenv.Has(env) {
		t.Fatalf("the probe ran without %q: %q", pyenv.TelemetryOff, env)
	}
}
