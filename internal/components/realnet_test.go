// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package components

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealNetInstall installs real components from the real sources with the
// shipped manifest and the pinned uv (tools/uv). Not part of the normal
// suite (build tag realnet; it downloads):
//
//	go test -tags realnet -run TestRealNetInstall -v ./internal/components/
//
// LF_REALNET_IDS (comma-separated, default "python") selects the components,
// e.g. "python,asr-openvino,model:openvino:small:int8" (about 400 MB);
// LF_REALNET_PYPI / LF_REALNET_MODELS choose a source ("tuna", "hf-mirror").
// LF_REALNET_DATA keeps the data directory for inspection.
func TestRealNetInstall(t *testing.T) {
	uv, _ := filepath.Abs(filepath.Join("..", "..", "tools", "uv", "uv"+exeSuffix))
	if !fileExists(uv) {
		t.Fatalf("uv not found at %s (see packaging/components/README.md)", uv)
	}
	manifest, _ := filepath.Abs(builtinManifest)
	data := os.Getenv("LF_REALNET_DATA")
	if data == "" {
		data = t.TempDir()
	}
	m, err := New(Options{DataDir: data, AppVersion: "3.0.0", BuiltinManifest: manifest, UV: uv})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ids := strings.Split(os.Getenv("LF_REALNET_IDS"), ",")
	if ids[0] == "" {
		ids = []string{IDPython}
	}
	src := Sources{PyPI: os.Getenv("LF_REALNET_PYPI"), Models: os.Getenv("LF_REALNET_MODELS")}
	p, err := m.Plan(ids)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: download %d MB, installed %d MB, need %d MB, free %d MB", mb(p.DownloadBytes), mb(p.InstalledBytes), mb(p.RequiredBytes), mb(p.FreeBytes))
	start := time.Now()
	id, err := m.Install(ids, src)
	if err != nil {
		t.Fatal(err)
	}
	var st TaskStatus
	for {
		st, _ = m.Task(id)
		if st.State == TaskDone || st.State == TaskFailed || st.State == TaskCancelled {
			break
		}
		time.Sleep(2 * time.Second)
		t.Logf("%s %d/%d MB %d KB/s", st.State, mb(st.Downloaded), mb(st.Total), st.SpeedBps>>10)
	}
	if st.State != TaskDone {
		t.Fatalf("%s %+v notices %+v", st.State, st.Error, st.Notices)
	}
	t.Logf("installed in %v; notices %+v", time.Since(start).Round(time.Second), st.Notices)
	py, ok := m.Python(BackendBase)
	if !ok {
		t.Fatal("no base python")
	}
	out, err := exec.Command(py, "-I", "-c", "import sys, pypdf, cryptography; print(sys.version, pypdf.__version__)").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Logf("base python: %s", strings.TrimSpace(string(out)))
	for _, s := range m.List() {
		if s.Installed {
			t.Logf("%-45s %-10s %6d MB broken=%v", s.ID, s.InstalledVersion, mb(s.DiskUsage), s.Broken)
			if s.Broken {
				t.Errorf("%s is broken", s.ID)
			}
		}
	}
}
