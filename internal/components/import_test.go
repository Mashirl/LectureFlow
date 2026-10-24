// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func componentJSON(t *testing.T, id, version string) []byte {
	b, err := json.Marshal(OfflineInfo{Schema: 1, ID: id, Version: version, Platform: PlatformWindowsX64})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeArchive(t *testing.T, files map[string][]byte) (string, *Offline) {
	t.Helper()
	data := makeZip(t, files)
	p := filepath.Join(t.TempDir(), "component.zip")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, &Offline{Name: "component.zip", SHA256: shaHex(data), Size: int64(len(data))}
}

// offlineEnv publishes offline entries for the openvino model, asr-cuda and
// python, and returns the archives.
func offlineEnv(t *testing.T) (*testEnv, map[string]string) {
	e := newTestEnv(t)
	wheels := map[string][]byte{
		"wheels/pkg_a-1.0.0-py3-none-any.whl":          []byte("wheel a"),
		"wheels/pkg_b-2.0.0-cp312-cp312-win_amd64.whl": []byte("wheel b"),
	}
	ovZip, ovOff := writeArchive(t, map[string][]byte{
		"component.json":                   componentJSON(t, ovModel, "1.0.0"),
		"files/openvino_encoder_model.xml": ovContent(e),
	})
	cudaFiles := map[string][]byte{"component.json": componentJSON(t, IDASRCUDA, "1.0.0")}
	for k, v := range wheels {
		cudaFiles[k] = v
	}
	cudaZip, cudaOff := writeArchive(t, cudaFiles)
	pyZip, pyOff := writeArchive(t, map[string][]byte{
		"component.json":                       componentJSON(t, IDPython, "1.0.0"),
		"files/cpython-1.tar.gz":               e.content["python"],
		"wheels/pypdf-6.21.2-py3-none-any.whl": []byte("pypdf wheel"),
	})
	s := defaultSpec()
	s.offline = map[string]*Offline{ovModel: ovOff, IDASRCUDA: cudaOff, IDPython: pyOff}
	e.writeBuiltin(s)
	return e, map[string]string{ovModel: ovZip, IDASRCUDA: cudaZip, IDPython: pyZip}
}

func importAndWait(t *testing.T, m *Manager, path string) TaskStatus {
	t.Helper()
	id, err := m.ImportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return wait(t, m, id, TaskDone, TaskFailed)
}

func TestImportModel(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	before := len(e.srv.log())
	st := importAndWait(t, m, zips[ovModel])
	if st.State != TaskDone || st.Kind != TaskImport || len(st.Components) != 1 || st.Components[0] != ovModel {
		t.Fatalf("%+v", st)
	}
	checkModelFile(t, m, ovContent(e))
	if len(e.srv.log()) != before {
		t.Error("an import must not download")
	}
	// importing it again does nothing
	if st := importAndWait(t, m, zips[ovModel]); st.State != TaskDone || st.Items[0].Phase != PhaseSkipped {
		t.Errorf("second import: %+v", st)
	}
}

func TestImportVenvAndPythonOffline(t *testing.T) {
	e, zips := offlineEnv(t)
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", log)
	m := e.open()
	st := importAndWait(t, m, zips[IDASRCUDA])
	if st.State != TaskFailed || st.Error.Code != CodeImportMissingDeps || st.Error.Params["dependents"] != IDPython {
		t.Fatalf("without python: %+v", st.Error)
	}
	if st := importAndWait(t, m, zips[IDPython]); st.State != TaskDone {
		t.Fatalf("python: %+v", st.Error)
	}
	if st := importAndWait(t, m, zips[IDASRCUDA]); st.State != TaskDone {
		t.Fatalf("asr-cuda: %+v", st.Error)
	}
	if n := len(e.srv.log()); n != 0 {
		t.Errorf("%d network requests during an offline install", n)
	}
	py, ok := m.Python(BackendFasterWhisper)
	if !ok {
		t.Fatal("asr-cuda not installed")
	}
	data, _ := os.ReadFile(filepath.Join(sitePackages(py), "fake_installed.txt"))
	if !strings.Contains(string(data), "index=find-links") {
		t.Errorf("installed from %q", data)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), `"--no-index"`) || !strings.Contains(string(calls), `"--offline"`) {
		t.Errorf("uv calls %s", calls)
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.Contains(f, "/wheels/") || strings.HasPrefix(f, "downloads/") {
			t.Errorf("left behind: %s", f)
		}
	}
}

func TestImportChecksumMismatch(t *testing.T) {
	e, _ := offlineEnv(t)
	m := e.open()
	// same component.json, different content
	bad, _ := writeArchive(t, map[string][]byte{
		"component.json":                   componentJSON(t, ovModel, "1.0.0"),
		"files/openvino_encoder_model.xml": []byte("tampered"),
	})
	st := importAndWait(t, m, bad)
	if st.State != TaskFailed || st.Error.Code != CodeImportChecksum || st.Error.Params["id"] != ovModel {
		t.Fatalf("%+v", st.Error)
	}
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Error("installed from a tampered archive")
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		t.Errorf("left behind: %s", f)
	}
}

func TestImportUnknownAndInvalid(t *testing.T) {
	e, _ := offlineEnv(t)
	m := e.open()
	other, _ := writeArchive(t, map[string][]byte{"component.json": componentJSON(t, "model:openvino:base:int4", "9.9.9")})
	if st := importAndWait(t, m, other); st.Error == nil || st.Error.Code != CodeImportUnknown {
		t.Errorf("unknown: %+v", st.Error)
	}
	// a known component without an offline entry
	noEntry, _ := writeArchive(t, map[string][]byte{"component.json": componentJSON(t, "tool", "1.0.0")})
	if st := importAndWait(t, m, noEntry); st.Error == nil || st.Error.Code != CodeImportUnknown {
		t.Errorf("no offline entry: %+v", st.Error)
	}
	junk := filepath.Join(t.TempDir(), "junk.zip")
	os.WriteFile(junk, []byte("this is not a zip"), 0o644)
	if st := importAndWait(t, m, junk); st.Error == nil || st.Error.Code != CodeImportInvalid {
		t.Errorf("junk: %+v", st.Error)
	}
	if _, err := m.ImportFile(filepath.Join(t.TempDir(), "missing.zip")); CodeOf(err) != CodeImportInvalid {
		t.Errorf("missing file: %v", err)
	}
}

// tamperedOV is an archive that names the openvino model like the real one
// but has other content (its SHA-256 is not in the manifest).
func tamperedOV(t *testing.T) []byte {
	return makeZip(t, map[string][]byte{
		"component.json":                   componentJSON(t, ovModel, "1.0.0"),
		"files/openvino_encoder_model.xml": []byte("tampered"),
	})
}

func noImportLeftovers(t *testing.T, e *testEnv) {
	t.Helper()
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.HasPrefix(f, ".imports/") || strings.HasPrefix(f, "downloads/") {
			t.Errorf("left behind: %s", f)
		}
	}
}

// After the copy, the original file is replaced: the task goes on with its
// verified copy and installs the original content.
func TestImportUsesPrivateCopy(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	orig := zips[ovModel]
	m.testHook = func(ev string, _ TaskID) {
		if ev == "import.copied" {
			if err := os.WriteFile(orig, tamperedOV(t), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	st := importAndWait(t, m, orig)
	if st.State != TaskDone {
		t.Fatalf("%+v", st.Error)
	}
	checkModelFile(t, m, ovContent(e))
	noImportLeftovers(t, e)
}

// Paused right after the copy, the original is replaced; resuming uses the
// copy, not the new file.
func TestImportPauseAfterCopyThenReplace(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	orig := zips[ovModel]
	m.testHook = func(ev string, id TaskID) {
		if ev == "import.copied" {
			m.Pause(id)
		}
	}
	id, err := m.ImportFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Fatal("installed while paused")
	}
	if err := os.WriteFile(orig, tamperedOV(t), 0o644); err != nil {
		t.Fatal(err)
	}
	m.testHook = nil
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("%+v", st.Error)
	}
	checkModelFile(t, m, ovContent(e))
	noImportLeftovers(t, e)
}

// Paused while copying, the original is replaced: resuming copies and
// hashes the new file from scratch, which the manifest does not know.
func TestImportPauseDuringCopyThenReplace(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	orig := zips[ovModel]
	m.testHook = func(ev string, id TaskID) {
		if ev == "import.copying" {
			m.Pause(id)
		}
	}
	id, err := m.ImportFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	if err := os.WriteFile(orig, tamperedOV(t), 0o644); err != nil {
		t.Fatal(err)
	}
	m.testHook = nil
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeImportChecksum {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Error("installed from a replaced file")
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		t.Errorf("left behind: %s", f)
	}
}

// An archive whose SHA-256 is in the manifest but whose file does not match
// File.SHA256 (a wrong offline entry): the extracted file is checked again
// and refused before it is moved into the component.
func TestImportFileChecksumPerFile(t *testing.T) {
	e := newTestEnv(t)
	bad, off := writeArchive(t, map[string][]byte{
		"component.json":                   componentJSON(t, ovModel, "1.0.0"),
		"files/openvino_encoder_model.xml": append([]byte("X"), ovContent(e)[1:]...), // same size, other bytes
	})
	s := defaultSpec()
	s.offline = map[string]*Offline{ovModel: off}
	e.writeBuiltin(s)
	m := e.open()
	st := importAndWait(t, m, bad)
	if st.State != TaskFailed || st.Error.Code != CodeImportChecksum || st.Error.Params["id"] != ovModel {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Error("installed a file that does not match the manifest")
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		t.Errorf("left behind: %s", f)
	}
}

// ImportFile checks the disk space before it creates a task: the copy, the
// extracted entries and, for a venv, the installed environment.
func TestImportDiskFull(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	e.setFree(64 << 20)
	before := len(m.Tasks())
	_, err := m.ImportFile(zips[IDASRCUDA])
	if CodeOf(err) != CodeDiskFull {
		t.Fatalf("got %v", err)
	}
	p := ErrorInfoOf(err).Params
	req, _ := p["required_mb"].(int64)
	if req < 256+6 || p["free_mb"] != int64(64) {
		t.Errorf("params %v", p)
	}
	if len(m.Tasks()) != before {
		t.Error("a task was created")
	}
	// enough space for the copy and the model, not more
	fi, _ := os.Stat(zips[ovModel])
	e.setFree(2*fi.Size() + 256<<20 + 1)
	if st := importAndWait(t, m, zips[ovModel]); st.State != TaskDone {
		t.Fatalf("%+v", st.Error)
	}
	e.setFree(1 << 40)
	if st := importAndWait(t, m, zips[IDASRCUDA]); st.State != TaskDone {
		t.Fatalf("%+v", st.Error)
	}
}

// A failed import drops its copy; Retry copies the user's file again and
// accepts it only when it is still the archive identified before.
func TestImportRetryCopiesAgain(t *testing.T) {
	e, zips := offlineEnv(t)
	m := e.open()
	orig := zips[IDPython]
	good, _ := os.ReadFile(orig)
	t.Setenv("LF_FAKE_UV_FAIL", "sync")
	st := importAndWait(t, m, orig)
	if st.State != TaskFailed || st.Error.Code != CodeUVFailed {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	noImportLeftovers(t, e)
	t.Setenv("LF_FAKE_UV_FAIL", "")
	// the file changed meanwhile
	other := makeZip(t, map[string][]byte{"component.json": componentJSON(t, IDPython, "1.0.0")})
	os.WriteFile(orig, other, 0o644)
	if err := m.Retry(st.ID, nil); err != nil {
		t.Fatal(err)
	}
	st = wait(t, m, st.ID, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeImportChecksum {
		t.Fatalf("retry with another file: %s %+v", st.State, st.Error)
	}
	os.WriteFile(orig, good, 0o644)
	if err := m.Retry(st.ID, nil); err != nil {
		t.Fatal(err)
	}
	if st = wait(t, m, st.ID, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("retry: %+v", st.Error)
	}
	if _, ok := m.Python(BackendBase); !ok {
		t.Error("python not installed")
	}
	noImportLeftovers(t, e)
}
