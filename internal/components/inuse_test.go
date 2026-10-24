// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func inUseParams(t *testing.T, err error, id, reason string) {
	t.Helper()
	if CodeOf(err) != CodeInUseByTask {
		t.Fatalf("got %v, want %s", err, CodeInUseByTask)
	}
	p := ErrorInfoOf(err).Params
	if p["id"] != id || p["reason"] != reason {
		t.Errorf("params %v, want id %s reason %s", p, id, reason)
	}
}

// Uninstall asks Options.InUse and refuses while a task uses the component.
func TestUninstallInUseByTask(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	var asked []string
	var mu sync.Mutex
	e.setInUse(func(id string) (bool, string) {
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		// InUse runs without the manager's lock: calling back must not block
		m.ModelDir(BackendOpenVINO, "tiny", "int8")
		m.Python(BackendFasterWhisper)
		m.List()
		return id == ovModel, "asr"
	})
	inUseParams(t, m.Uninstall(ovModel), ovModel, "asr")
	if s, _ := statusOf(m, ovModel); !s.Installed || s.Broken {
		t.Fatalf("after a refused uninstall: %+v", s)
	}
	if _, ok := m.ModelDir(BackendOpenVINO, "tiny", "int8"); !ok {
		t.Error("model directory gone")
	}
	// a component that is not installed is not asked about
	mu.Lock()
	asked = nil
	mu.Unlock()
	if err := m.Uninstall(fwFloat16); CodeOf(err) != CodeNotInstalled {
		t.Errorf("not installed: %v", err)
	}
	mu.Lock()
	if len(asked) != 0 {
		t.Errorf("InUse asked about %v", asked)
	}
	mu.Unlock()
	// the task ended
	e.setInUse(func(string) (bool, string) { return false, "" })
	if err := m.Uninstall(ovModel); err != nil {
		t.Fatal(err)
	}
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Error("still installed")
	}
}

// An empty reason still gives a reason parameter.
func TestInUseDefaultReason(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	e.setInUse(func(string) (bool, string) { return true, "" })
	inUseParams(t, m.Uninstall(ovModel), ovModel, "task")
}

// Update is refused at once while the component is in use.
func TestUpdateRefusedInUse(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, fwFloat16)
	e.setInUse(func(id string) (bool, string) { return id == fwFloat16, "device_test" })
	_, err := m.Update(fwFloat16)
	inUseParams(t, err, fwFloat16, "device_test")
	if len(m.Tasks()) != 1 { // the install only
		t.Errorf("a task was created: %+v", m.Tasks())
	}
}

// Updating python is refused while a venv that would be rebuilt is in use.
func TestUpdatePythonRefusedWhileVenvInUse(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.pyVersion, s.pySeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, IDASRCUDA)
	e.setInUse(func(id string) (bool, string) { return id == IDASRCUDA, "asr" })
	_, err := m.Update(IDPython)
	inUseParams(t, err, IDASRCUDA, "asr")
}

// Something starts using the component while the update downloads and
// tests the new version: the switch is refused, the old version stays and
// the new one is removed; once the component is free the update succeeds.
func TestUpdateSwitchRefusedInUse(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, fwFloat16)
	oldDir, oldBytes := modelBin(t, m)
	busy := true
	var mu sync.Mutex
	e.setSelfTest(func(context.Context, SelfTestRequest) error {
		// an asr job started meanwhile
		e.setInUse(func(id string) (bool, string) {
			mu.Lock()
			defer mu.Unlock()
			return busy && id == fwFloat16, "asr"
		})
		return nil
	})
	id, err := m.Update(fwFloat16)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error == nil || st.Error.Code != CodeInUseByTask || st.Error.Params["reason"] != "asr" {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	dir, b := modelBin(t, m)
	if dir != oldDir || string(b) != string(oldBytes) {
		t.Errorf("switched to %s", dir)
	}
	if st, _ := statusOf(m, fwFloat16); st.InstalledVersion != "1.0.0" || !st.UpdateAvailable || st.Broken {
		t.Errorf("status %+v", st)
	}
	vers, _ := filepath.Glob(filepath.Join(e.data, "components", "model__faster-whisper__tiny__float16", "*"))
	if len(vers) != 1 {
		t.Errorf("version directories: %v", vers)
	}
	// the job ended: retrying the same task switches
	mu.Lock()
	busy = false
	mu.Unlock()
	if err := m.Retry(id, nil); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("retry: %+v", st.Error)
	}
	if dir, _ := modelBin(t, m); dir == oldDir {
		t.Error("not switched after the retry")
	}
}
