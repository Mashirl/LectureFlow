// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// toolLocator is a component manager that also has tool components.
type toolLocator struct {
	fakeLocator
	tools map[string]string   // id → executable
	env   map[string][]string // id → ToolEnv
}

func (l toolLocator) ToolPath(id string) (string, bool) {
	p, ok := l.tools[id]
	return p, ok
}

func (l toolLocator) ToolEnv(id string) []string { return l.env[id] }

func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	old := lookPath
	lookPath = fn
	t.Cleanup(func() { lookPath = old })
}

func envValue(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			val, n = v, n+1
		}
	}
	return val, n
}

// 3.1.0 (v31-brief 2.1): the gcloud component first, else gcloud on PATH.
func TestGcloudPathComponentFirst(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), "gcloud.cmd")
	withLookPath(t, func(name string) (string, error) {
		if name != "gcloud" {
			t.Errorf("looked up %q", name)
		}
		return onPath, nil
	})
	comp := filepath.Join(t.TempDir(), "components", "gcloud", "1.0.0", "tool", "bin", "gcloud.cmd")
	c := Default()
	c.Components = toolLocator{tools: map[string]string{GcloudComponent: comp},
		env: map[string][]string{GcloudComponent: {"CLOUDSDK_PYTHON=/c/python3", "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true"}}}
	if p, ok := c.GcloudPath(); !ok || p != comp {
		t.Fatalf("GcloudPath = %q %v, want the component %q", p, ok, comp)
	}
	t.Setenv("PYTHONPATH", "/somewhere/else")
	t.Setenv("CLOUDSDK_PYTHON", "/users/own/python")
	t.Setenv("LF_GCLOUD_TEST", "kept")
	env := c.GcloudEnv()
	if _, n := envValue(env, "PYTHONPATH"); n != 0 {
		t.Errorf("PYTHONPATH not removed: %v", env)
	}
	if v, _ := envValue(env, "LF_GCLOUD_TEST"); v != "kept" {
		t.Errorf("the process environment is missing: %v", env)
	}
	// the component's variables come last, so they win over the user's
	var last string
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLOUDSDK_PYTHON=") {
			last = kv
		}
	}
	if last != "CLOUDSDK_PYTHON=/c/python3" {
		t.Errorf("last CLOUDSDK_PYTHON = %q", last)
	}
	// the env really applies that way
	cmd := exec.Command("x")
	cmd.Env = env
	if v, _ := envValue(cmd.Environ(), "CLOUDSDK_PYTHON"); v != "/c/python3" {
		t.Errorf("os/exec sees CLOUDSDK_PYTHON=%q", v)
	}
}

func TestGcloudPathFallsBackToPATH(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "gcloud.cmd")
	withLookPath(t, func(string) (string, error) { return onPath, nil })
	for name, c := range map[string]*Config{
		"no locator":            Default(),
		"locator without tools": func() *Config { c := Default(); c.Components = fakeLocator{}; return c }(),
		"tool not installed":    func() *Config { c := Default(); c.Components = toolLocator{}; return c }(),
	} {
		if p, ok := c.GcloudPath(); !ok || p != onPath {
			t.Errorf("%s: GcloudPath = %q %v, want %q", name, p, ok, onPath)
		}
		t.Setenv("CLOUDSDK_PYTHON", "/users/own/python")
		// the user's own gcloud keeps the user's own settings
		if v, _ := envValue(c.GcloudEnv(), "CLOUDSDK_PYTHON"); v != "/users/own/python" {
			t.Errorf("%s: CLOUDSDK_PYTHON = %q", name, v)
		}
	}
}

func TestGcloudPathMissing(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	c := Default()
	c.Components = toolLocator{}
	if p, ok := c.GcloudPath(); ok || p != "" {
		t.Fatalf("GcloudPath = %q %v, want none", p, ok)
	}
	withLookPath(t, func(string) (string, error) { return "", errors.Join(exec.ErrDot) })
	if _, ok := c.GcloudPath(); ok {
		t.Fatal("a gcloud in the current directory must not be used")
	}
}

// The real lookup finds gcloud(.cmd) on PATH.
func TestGcloudPathRealLookup(t *testing.T) {
	dir := t.TempDir()
	name, body := "gcloud", "#!/bin/sh\necho gcloud\n"
	if runtime.GOOS == "windows" {
		name, body = "gcloud.cmd", "@echo gcloud\r\n"
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	p, ok := Default().GcloudPath()
	if !ok || !strings.EqualFold(p, filepath.Join(dir, name)) {
		t.Fatalf("GcloudPath = %q %v, want %s", p, ok, filepath.Join(dir, name))
	}
}
