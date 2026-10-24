// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// v3 Gate round 2: the detection reports the interpreters it runs, so the
// component manager does not remove an environment under a probe.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// fakeWaitProbeEnv set to a directory: the test binary plays a probe that
// creates {dir}/started, waits for {dir}/release, then answers like the
// "ok" probe.
const fakeWaitProbeEnv = "LF_DEVICES_FAKE_PROBE_WAIT"

func init() {
	dir := os.Getenv(fakeWaitProbeEnv)
	if dir == "" {
		return
	}
	if os.WriteFile(filepath.Join(dir, "started"), nil, 0o644) != nil {
		os.Exit(8)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(9)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("PROBE " + fakeProbeJSON)
	os.Exit(0)
}

func TestWorkerProbeReportsRunningPython(t *testing.T) {
	cfg := probeConfig(t)
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())
	other := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{})
	signal := t.TempDir()
	t.Setenv(fakeWaitProbeEnv, signal)
	type result struct {
		p   *OVProbe
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, err := r.workerProbe(tctx())
		done <- result{p, err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(signal, "started")); err == nil {
			break
		}
		select {
		case res := <-done:
			t.Fatalf("the probe ended before it started: %+v", res)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake probe never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.RunningPythons(); !slices.Equal(got, []string{cfg.PythonOpenVINO}) {
		t.Errorf("RunningPythons during the probe = %v, want %s", got, cfg.PythonOpenVINO)
	}
	if got := other.RunningPythons(); len(got) != 0 {
		t.Errorf("another registry reports %v", got)
	}
	if err := os.WriteFile(filepath.Join(signal, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.err != nil || res.p == nil {
		t.Fatalf("probe: %+v", res)
	}
	if got := r.RunningPythons(); len(got) != 0 {
		t.Errorf("RunningPythons after the probe = %v", got)
	}
}

func TestMLXCheckReportsRunningPython(t *testing.T) {
	st := regStore(t)
	o := newWorld().opts()
	o.MLX = nil
	cfg := probeConfig(t)
	cfg.ASR.FakeResult = ""
	comp := filepath.Join(t.TempDir(), "components", "asr-mlx", "1.0.0", "venv", "bin", "python")
	cfg.Components = compLocator{py: map[string]string{model.BackendMLX: comp}}
	r := NewRegistry(st, cfg, t.TempDir(), o)
	var during []string
	prev := detectMLXFn
	t.Cleanup(func() { detectMLXFn = prev })
	detectMLXFn = func(_ context.Context, python string) (string, bool) {
		during = r.RunningPythons()
		return "", false
	}
	r.mlx(tctx())
	if !slices.Equal(during, []string{comp}) {
		t.Errorf("RunningPythons during the MLX check = %v, want %s", during, comp)
	}
	if got := r.RunningPythons(); len(got) != 0 {
		t.Errorf("after the MLX check: %v", got)
	}
}
