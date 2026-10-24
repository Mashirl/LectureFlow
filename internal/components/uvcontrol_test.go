// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitPhase(t *testing.T, m *Manager, id TaskID, item, phase string) {
	t.Helper()
	waitFor(t, item+" "+phase, func() bool {
		st, _ := m.Task(id)
		for _, it := range st.Items {
			if it.ID == item && it.Phase == phase {
				return true
			}
		}
		return false
	})
}

// Pausing while uv installs stops it at once; resuming runs uv again on
// the same staging venv.
func TestPauseDuringUV(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	t.Setenv("LF_FAKE_UV_SLEEP_MS", "3000")
	id, err := m.Install([]string{IDASRCUDA}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, m, id, IDASRCUDA, PhaseInstalling)
	time.Sleep(100 * time.Millisecond) // uv is running
	start := time.Now()
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("pause took %v", d)
	}
	staging, _ := filepath.Glob(filepath.Join(e.data, "components", "asr-cuda", ".s*"))
	if len(staging) != 1 {
		t.Fatalf("staging after pause: %v", staging)
	}
	if s, _ := statusOf(m, IDASRCUDA); s.Installed {
		t.Error("installed while paused")
	}
	t.Setenv("LF_FAKE_UV_SLEEP_MS", "0")
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("%+v", st.Error)
	}
	if _, ok := m.Python(BackendFasterWhisper); !ok {
		t.Error("not installed after resume")
	}
}

// Cancelling while uv installs removes the staging venv and uv's cache.
func TestCancelDuringUV(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	t.Setenv("LF_FAKE_UV_SLEEP_MS", "3000")
	id, _ := m.Install([]string{IDASRCUDA}, Sources{})
	waitPhase(t, m, id, IDASRCUDA, PhaseInstalling)
	time.Sleep(100 * time.Millisecond)
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskCancelled)
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.HasPrefix(f, "asr-cuda/") || strings.HasPrefix(f, ".uvc/") || strings.HasPrefix(f, "downloads/") {
			t.Errorf("left behind: %s", f)
		}
	}
	if s, _ := statusOf(m, IDPython); !s.Installed {
		t.Error("python must stay")
	}
}

// Closing the manager mid-install (application exit) leaves nothing that
// looks installed; the next start cleans up and installs again.
func TestCloseDuringInstall(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	t.Setenv("LF_FAKE_UV_SLEEP_MS", "3000")
	id, _ := m.Install([]string{IDASRCUDA}, Sources{})
	waitPhase(t, m, id, IDPython, PhaseInstalling)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("close took %v", d)
	}
	if _, err := m.Install([]string{IDPython}, Sources{}); err == nil {
		t.Error("install after close")
	}
	t.Setenv("LF_FAKE_UV_SLEEP_MS", "0")
	m2 := e.open()
	for _, s := range m2.List() {
		if s.Installed && !s.Bundled {
			t.Errorf("%s looks installed after an interrupted run", s.ID)
		}
	}
	if dirs, _ := filepath.Glob(filepath.Join(e.data, "components", "python", "*")); len(dirs) != 0 {
		t.Errorf("leftovers: %v", dirs)
	}
	// the unpacked runtime was removed with the unreferenced directory; the
	// install starts over and completes
	mustInstall(t, m2, Sources{}, IDASRCUDA)
	if _, err := os.Stat(filepath.Join(e.data, "components", ".uvc")); !os.IsNotExist(err) {
		t.Error("uv cache kept")
	}
}
