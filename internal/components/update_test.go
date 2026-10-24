// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCheckManifestRejectsIncompatible(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.compat = 2, ">=9.0.0 <10.0.0"
	e.serveManifest(s)
	ok, err := m.CheckManifest(context.Background())
	if ok || CodeOf(err) != CodeManifestIncompatible {
		t.Fatalf("got %v %v", ok, err)
	}
	if info := m.ManifestInfo(); info.Serial != 1 || !info.Builtin {
		t.Errorf("manifest changed: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(e.data, "components", cachedManifestName)); !os.IsNotExist(err) {
		t.Error("an incompatible manifest was stored")
	}
	// a schema this build cannot read
	e.srv.putExact("/manifest.json", []byte(`{"schema": 2, "serial": 9}`))
	if _, err := m.CheckManifest(context.Background()); CodeOf(err) != CodeManifestSchema {
		t.Errorf("schema 2: %v", err)
	}
	// broken JSON
	e.srv.putExact("/manifest.json", []byte(`{"schema": 1, "serial": `))
	if _, err := m.CheckManifest(context.Background()); CodeOf(err) != CodeManifestInvalid {
		t.Errorf("garbage: %v", err)
	}
	// not newer
	e.serveManifest(defaultSpec())
	if ok, err := m.CheckManifest(context.Background()); ok || err != nil {
		t.Errorf("same serial: %v %v", ok, err)
	}
}

// A newer compatible manifest is adopted, kept for the next start, and its
// updates become visible.
func TestCheckManifestAdoptsNewer(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	var mu sync.Mutex
	var types []string
	unsub := m.Subscribe(func(ev Event) { mu.Lock(); types = append(types, ev.Type); mu.Unlock() })
	defer unsub()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	e.serveManifest(s)
	ok, err := m.CheckManifest(context.Background())
	if !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	if info := m.ManifestInfo(); info.Serial != 2 || info.Builtin {
		t.Errorf("info %+v", info)
	}
	waitFor(t, "manifest event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ty := range types {
			if ty == EventManifest {
				return true
			}
		}
		return false
	})
	if st, _ := statusOf(m, fwFloat16); st.Version != "1.1.0" {
		t.Errorf("status %+v", st)
	}
	m.Close()
	m2 := e.open()
	if info := m2.ManifestInfo(); info.Serial != 2 {
		t.Errorf("after restart: %+v", info)
	}
	// an application version outside the cached manifest's range falls back
	// to the built-in one
	opts := e.options()
	opts.AppVersion = "4.0.0"
	m2.Close()
	m3, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m3.Close()
	if info := m3.ManifestInfo(); info.Serial != 1 || !info.Builtin {
		t.Errorf("app 4.0.0: %+v", info)
	}
}

func installThenNewManifest(t *testing.T, e *testEnv, m *Manager, s manifestSpec, ids ...string) {
	t.Helper()
	mustInstall(t, m, Sources{}, ids...)
	e.serveManifest(s)
	if ok, err := m.CheckManifest(context.Background()); !ok || err != nil {
		t.Fatalf("CheckManifest: %v %v", ok, err)
	}
}

func modelBin(t *testing.T, m *Manager) (string, []byte) {
	dir, ok := m.ModelDir(BackendFasterWhisper, "tiny", "float16")
	if !ok {
		t.Fatal("model missing")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "model.bin"))
	return dir, b
}

func TestUpdateSwitchesAfterSelfTest(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, fwFloat16)
	oldDir, _ := modelBin(t, m)
	if st, _ := statusOf(m, fwFloat16); !st.UpdateAvailable || st.InstalledVersion != "1.0.0" {
		t.Fatalf("status %+v", st)
	}
	var got SelfTestRequest
	e.setSelfTest(func(_ context.Context, req SelfTestRequest) error {
		got = req
		// the new model is complete and the old one still active
		b, err := os.ReadFile(filepath.Join(req.ModelDir, "model.bin"))
		if err != nil || !bytes.Equal(b, randBytes(2, 700<<10+123)) {
			return errors.New("new model incomplete")
		}
		if _, ok := m.ModelDir(BackendFasterWhisper, "tiny", "float16"); !ok {
			return errors.New("old model not active")
		}
		return nil
	})
	id, err := m.Update(fwFloat16)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskDone || st.Kind != TaskUpdate {
		t.Fatalf("%+v", st)
	}
	if got.ID != fwFloat16 || got.Version != "1.1.0" || got.Backend != BackendFasterWhisper ||
		!strings.Contains(got.Python, "asr-cuda") {
		t.Errorf("self-test request %+v", got)
	}
	dir, b := modelBin(t, m)
	if !strings.Contains(dir, "1.1.0") || !bytes.Equal(b, randBytes(2, 700<<10+123)) {
		t.Errorf("after update: %s", dir)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old version not removed after the switch")
	}
	if st, _ := statusOf(m, fwFloat16); st.UpdateAvailable || st.InstalledVersion != "1.1.0" {
		t.Errorf("status %+v", st)
	}
	if _, err := m.Update(fwFloat16); CodeOf(err) != CodeUpToDate {
		t.Errorf("second update: %v", err)
	}
}

func TestUpdateSelfTestFailureKeepsOld(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, fwFloat16)
	oldDir, oldBytes := modelBin(t, m)
	e.setSelfTest(func(context.Context, SelfTestRequest) error { return errors.New("worker crashed on silence") })
	id, err := m.Update(fwFloat16)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeSelfTestFailed || !strings.Contains(st.Error.Detail, "worker crashed") {
		t.Fatalf("%+v %+v", st.State, st.Error)
	}
	dir, b := modelBin(t, m)
	if dir != oldDir || !bytes.Equal(b, oldBytes) {
		t.Errorf("rolled back to %s", dir)
	}
	if st, _ := statusOf(m, fwFloat16); st.InstalledVersion != "1.0.0" || !st.UpdateAvailable || st.Broken {
		t.Errorf("status %+v", st)
	}
	matches, _ := filepath.Glob(filepath.Join(e.data, "components", "model__faster-whisper__tiny__float16", "*"))
	if len(matches) != 1 {
		t.Errorf("version directories: %v", matches)
	}
	// the failed version's staging directory is gone; a retry downloads
	// and tests again
	e.setSelfTest(nil)
	id, _ = m.Update(fwFloat16)
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("retry: %+v", st.Error)
	}
}

func TestUpdateErrors(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	if _, err := m.Update(ovModel); CodeOf(err) != CodeNotInstalled {
		t.Errorf("not installed: %v", err)
	}
	if _, err := m.Update(IDCore); CodeOf(err) != CodeBundled {
		t.Errorf("core: %v", err)
	}
	if _, err := m.Update("ghost"); CodeOf(err) != CodeUnknown {
		t.Errorf("ghost: %v", err)
	}
	mustInstall(t, m, Sources{}, ovModel)
	if _, err := m.Update(ovModel); CodeOf(err) != CodeUpToDate {
		t.Errorf("up to date: %v", err)
	}
}

func homeOf(t *testing.T, venv string) string {
	data, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == "home" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Updating python rebuilds the venvs on the new runtime and removes the old
// runtime once nothing uses it.
func TestUpdatePythonRebuildsVenvs(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.pyVersion, s.pySeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, IDASRCUDA)
	oldRuntime := filepath.Join(e.data, "components", "python", "1.0.0")
	var tested []string
	var mu sync.Mutex
	e.setSelfTest(func(_ context.Context, req SelfTestRequest) error {
		mu.Lock()
		tested = append(tested, req.ID+"@"+req.Version)
		mu.Unlock()
		return nil
	})
	id, err := m.Update(IDPython)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskDone || len(st.Items) != 2 || st.Items[1].ID != IDASRCUDA || !st.Items[1].Update {
		t.Fatalf("%+v", st)
	}
	if strings.Join(tested, ",") != "python@1.1.0,asr-cuda@1.0.0" {
		t.Errorf("self-tests %v", tested)
	}
	py, _ := m.Python(BackendFasterWhisper)
	home := homeOf(t, filepath.Dir(filepath.Dir(py)))
	if !strings.Contains(home, filepath.Join("python", "1.1.0", "runtime")) {
		t.Errorf("asr-cuda home %s", home)
	}
	if !strings.Contains(py, filepath.Join("asr-cuda", "1.0.0-r2")) {
		t.Errorf("rebuilt venv at %s", py)
	}
	if _, err := os.Stat(oldRuntime); !os.IsNotExist(err) {
		t.Error("old runtime kept although nothing uses it")
	}
	if _, err := os.Stat(filepath.Join(e.data, "components", "asr-cuda", "1.0.0")); !os.IsNotExist(err) {
		t.Error("old venv kept")
	}
	for _, s := range m.List() {
		if s.RebuildNeeded || s.Broken {
			t.Errorf("%s: %+v", s.ID, s)
		}
	}
}

// When the venv rebuild fails, python is updated but the venv keeps working
// on the old runtime, which stays until the venv is rebuilt.
func TestUpdatePythonVenvRebuildFails(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.pyVersion, s.pySeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, IDASRCUDA)
	t.Setenv("LF_FAKE_PY_FAIL", "faster_whisper")
	id, _ := m.Update(IDPython)
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeVerifyFailed || st.Items[0].Phase != PhaseDone {
		t.Fatalf("%+v", st)
	}
	if s, _ := statusOf(m, IDPython); s.InstalledVersion != "1.1.0" {
		t.Errorf("python %+v", s)
	}
	cuda, _ := statusOf(m, IDASRCUDA)
	if !cuda.RebuildNeeded || cuda.Broken {
		t.Errorf("asr-cuda %+v", cuda)
	}
	py, ok := m.Python(BackendFasterWhisper)
	if !ok || !strings.Contains(homeOf(t, filepath.Dir(filepath.Dir(py))), filepath.Join("python", "1.0.0")) {
		t.Fatalf("asr-cuda python %s %v", py, ok)
	}
	oldRuntime := filepath.Join(e.data, "components", "python", "1.0.0")
	if _, err := os.Stat(oldRuntime); err != nil {
		t.Fatal("old runtime removed while asr-cuda uses it")
	}
	// survives a restart (retained in state.json)
	m.Close()
	m = e.open()
	if _, err := os.Stat(oldRuntime); err != nil {
		t.Fatal("old runtime removed at start")
	}
	os.Unsetenv("LF_FAKE_PY_FAIL")
	id, err := m.Update(IDASRCUDA)
	if err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("rebuild: %+v", st.Error)
	}
	if _, err := os.Stat(oldRuntime); !os.IsNotExist(err) {
		t.Error("old runtime kept after the rebuild")
	}
	if cuda, _ := statusOf(m, IDASRCUDA); cuda.RebuildNeeded {
		t.Error("still needs a rebuild")
	}
}

// An update self-test failure of python keeps the old runtime active.
func TestUpdatePythonSelfTestFails(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	s := defaultSpec()
	s.serial, s.pyVersion, s.pySeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, IDPython)
	e.setSelfTest(func(context.Context, SelfTestRequest) error { return errors.New("no") })
	id, _ := m.Update(IDPython)
	if st := wait(t, m, id, TaskDone, TaskFailed); st.Error == nil || st.Error.Code != CodeSelfTestFailed {
		t.Fatalf("%+v", st)
	}
	if s, _ := statusOf(m, IDPython); s.InstalledVersion != "1.0.0" || s.Broken {
		t.Errorf("%+v", s)
	}
	if _, err := os.Stat(filepath.Join(e.data, "components", "python", "1.1.0")); !os.IsNotExist(err) {
		t.Error("the failed version's directory stays")
	}
}

// Without a self-test function an update switches after the install check
// and says that the self-test was skipped.
func TestUpdateWithoutSelfTest(t *testing.T) {
	e := newTestEnv(t)
	opts := e.options()
	opts.SelfTest = nil
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := defaultSpec()
	s.serial, s.modelVersion, s.modelSeed = 2, "1.1.0", 2
	installThenNewManifest(t, e, m, s, fwFloat16)
	id, _ := m.Update(fwFloat16)
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskDone || len(st.Notices) != 1 || st.Notices[0].Code != CodeSelfTestSkipped {
		t.Fatalf("%+v", st)
	}
}

// A newer manifest drops asr-cuda: the installed copy stays usable, is
// listed as orphaned and can be uninstalled.
func TestOrphanedComponent(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDASRCUDA)
	s := defaultSpec()
	s.serial = 2
	man := e.manifest(s)
	var keep []Component
	for _, c := range man.Components {
		if c.ID != IDASRCUDA && !contains(c.Depends, IDASRCUDA) {
			keep = append(keep, c)
		}
	}
	man.Components = keep
	e.srv.putExact("/manifest.json", marshalManifest(t, man))
	if ok, err := m.CheckManifest(context.Background()); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	st, ok := statusOf(m, IDASRCUDA)
	if !ok || !st.Orphaned || !st.Installed || st.Kind != KindVenv {
		t.Fatalf("%+v", st)
	}
	if _, ok := m.Python(BackendFasterWhisper); !ok {
		t.Error("an orphaned venv still serves its backend")
	}
	if err := m.Uninstall(IDPython); CodeOf(err) != CodeInUse {
		t.Errorf("python is still needed by the orphan: %v", err)
	}
	if err := m.Uninstall(IDASRCUDA); err != nil {
		t.Fatal(err)
	}
	if _, ok := statusOf(m, IDASRCUDA); ok {
		t.Error("still listed")
	}
}
