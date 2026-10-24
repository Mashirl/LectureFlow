// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ovModel = "model:openvino:tiny:int8"

// ovFile is the served path of the openvino test model (default source).
const ovFile = "/default/hf/ov-tiny/model.xml"

func checkModelFile(t *testing.T, m *Manager, want []byte) {
	t.Helper()
	dir, ok := m.ModelDir(BackendOpenVINO, "tiny", "int8")
	if !ok {
		t.Fatal("model not installed")
	}
	got, err := os.ReadFile(filepath.Join(dir, "openvino_encoder_model.xml"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("installed file differs (err %v, %d bytes)", err, len(got))
	}
}

func ovContent(e *testEnv) []byte {
	e.srv.mu.Lock()
	defer e.srv.mu.Unlock()
	return e.srv.files[ovFile]
}

// The server breaks the connection mid-way; the download resumes with a
// Range request and the result has the right SHA-256.
func TestDownloadResumesAfterBrokenConnection(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.drop[ovFile] = 100_000
	e.srv.mu.Unlock()
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	checkModelFile(t, m, ovContent(e))
	var ranges []string
	for _, r := range e.srv.log() {
		if r.Path == ovFile {
			ranges = append(ranges, r.Range)
		}
	}
	if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "bytes=100000-" {
		t.Fatalf("requests: %q, want a full request then bytes=100000-", ranges)
	}
	if files := listFiles(t, filepath.Join(e.data, "components", "downloads")); len(files) != 0 {
		t.Errorf("downloads left: %v", files)
	}
}

// A server that ignores Range starts the file over; the hash still matches.
func TestDownloadServerIgnoresRange(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.drop[ovFile] = 50_000
	e.srv.noRange = true
	e.srv.mu.Unlock()
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	checkModelFile(t, m, ovContent(e))
}

func TestPauseAndResume(t *testing.T) {
	e := newTestEnv(t)
	hit, release := e.srv.gate(ovFile)
	m := e.open()
	id, err := m.Install([]string{ovModel}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit
	waitFor(t, "progress", func() bool { st, _ := m.Task(id); return st.Downloaded > 0 })
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskPaused)
	release()
	part := filepath.Join(e.data, "components", "downloads", shaHex(ovContent(e))+".part")
	fi, err := os.Stat(part)
	if err != nil || fi.Size() == 0 || fi.Size() >= int64(len(ovContent(e))) {
		t.Fatalf("partial file after pause: %v %v", fi, err)
	}
	if st.Downloaded != fi.Size() {
		t.Errorf("paused status shows %d bytes, the partial file has %d", st.Downloaded, fi.Size())
	}
	if s, _ := statusOf(m, ovModel); s.Installed || s.Task != id {
		t.Errorf("while paused: %+v", s)
	}
	if err := m.Pause(id); CodeOf(err) != CodeTaskState {
		t.Errorf("pausing a paused task: %v", err)
	}
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("after resume: %+v", st)
	}
	checkModelFile(t, m, ovContent(e))
	log := e.srv.log()
	last := log[len(log)-1]
	if last.Path != ovFile || last.Range != fmt.Sprintf("bytes=%d-", fi.Size()) {
		t.Errorf("resume request %+v, want Range bytes=%d-", last, fi.Size())
	}
	if err := m.Resume(id); CodeOf(err) != CodeTaskState {
		t.Errorf("resuming a finished task: %v", err)
	}
}

// Cancelling a running download deletes everything the task created.
func TestCancelLeavesNoFiles(t *testing.T) {
	e := newTestEnv(t)
	hit, release := e.srv.gate(ovFile)
	defer release()
	m := e.open()
	id, err := m.Install([]string{ovModel}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit
	waitFor(t, "progress", func() bool { st, _ := m.Task(id); return st.Downloaded > 0 })
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskCancelled)
	if st.Error == nil || st.Error.Code != CodeCancelled {
		t.Errorf("cancel error: %+v", st.Error)
	}
	waitFor(t, "cleanup", func() bool {
		files := listFiles(t, filepath.Join(e.data, "components"))
		return len(files) == 0
	})
	if s, _ := statusOf(m, ovModel); s.Installed || s.Task != "" {
		t.Errorf("after cancel: %+v", s)
	}
	if err := m.Cancel(id); CodeOf(err) != CodeTaskState {
		t.Errorf("cancelling twice: %v", err)
	}
}

// Cancelling a paused task (python and venv done, model half downloaded)
// removes the partial files but keeps what was installed.
func TestCancelPausedKeepsInstalled(t *testing.T) {
	e := newTestEnv(t)
	model := "/default/hf/tiny/1/model.bin"
	hit, release := e.srv.gate(model)
	defer release()
	m := e.open()
	id, err := m.Install([]string{"model:faster-whisper:tiny:float16"}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskCancelled)
	for _, c := range []string{IDPython, IDASRCUDA} {
		if s, _ := statusOf(m, c); !s.Installed {
			t.Errorf("%s should stay installed", c)
		}
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.HasPrefix(f, "downloads/") || isStagingPath(f) || strings.HasPrefix(f, "model__") {
			t.Errorf("left behind: %s", f)
		}
	}
}

func TestChecksumRetryOnce(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.corrupt[ovFile] = 1
	e.srv.mu.Unlock()
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	checkModelFile(t, m, ovContent(e))
	if n := e.srv.count(ovFile); n != 2 {
		t.Errorf("%d requests, want 2 (the damaged one and the retry)", n)
	}
}

func TestChecksumFailsTwice(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.corrupt[ovFile] = 2
	e.srv.mu.Unlock()
	m := e.open()
	id, err := m.Install([]string{ovModel}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error == nil || st.Error.Code != CodeChecksum {
		t.Fatalf("got %s %+v, want failed %s", st.State, st.Error, CodeChecksum)
	}
	if st.Items[0].Phase != PhaseFailed || st.Items[0].Error == nil {
		t.Errorf("item: %+v", st.Items[0])
	}
	if n := e.srv.count(ovFile); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if f != stateFileName {
			t.Errorf("left behind after a checksum failure: %s", f)
		}
	}
	if s, _ := statusOf(m, ovModel); s.Installed {
		t.Error("installed after failing")
	}
	// a retry succeeds once the server is fine again
	mustInstall(t, m, Sources{}, ovModel)
}

func TestHTTPStatusError(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.status["/default/hf/ov-tiny/"] = 404
	e.srv.mu.Unlock()
	m := e.open()
	id, _ := m.Install([]string{ovModel}, Sources{})
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeHTTPStatus || fmt.Sprint(st.Error.Params["status"]) != "404" {
		t.Fatalf("got %s %+v", st.State, st.Error)
	}
	if n := e.srv.count("/default/hf/ov-tiny/"); n != 1 {
		t.Errorf("a 404 is not retried: %d requests", n)
	}
}

func TestServerErrorRetriedThenNetworkError(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.status["/default/hf/ov-tiny/"] = 503
	e.srv.mu.Unlock()
	m := e.open()
	id, _ := m.Install([]string{ovModel}, Sources{})
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeHTTPStatus {
		t.Fatalf("got %s %+v", st.State, st.Error)
	}
	if n := e.srv.count("/default/hf/ov-tiny/"); n != maxNoProgress {
		t.Errorf("a 503 is retried: %d requests, want %d", n, maxNoProgress)
	}
}

// The model mirror fails; the download falls back to the default source and
// the task says so.
func TestMirrorFallsBackToDefault(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.status["/mirror/"] = 502
	e.srv.mu.Unlock()
	m := e.open()
	st := mustInstall(t, m, Sources{Models: "hf-mirror"}, ovModel)
	checkModelFile(t, m, ovContent(e))
	if e.srv.count("/mirror/hf/") == 0 || e.srv.count(ovFile) != 1 {
		t.Errorf("requests: %+v", e.srv.log())
	}
	if len(st.Notices) != 1 || st.Notices[0].Code != CodeMirrorFallback ||
		st.Notices[0].Params["group"] != GroupModels || st.Notices[0].Params["mirror"] != "hf-mirror" {
		t.Errorf("notices: %+v", st.Notices)
	}
	if st.Items[0].Source != SourceDefault {
		t.Errorf("item source %q after the fallback", st.Items[0].Source)
	}
}

// The mirror works: nothing is fetched from the default source.
func TestMirrorUsed(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	st := mustInstall(t, m, Sources{Models: "hf-mirror"}, ovModel)
	if e.srv.count("/default/") != 0 || e.srv.count("/mirror/hf/ov-tiny/") != 1 || len(st.Notices) != 0 {
		t.Errorf("requests %+v notices %+v", e.srv.log(), st.Notices)
	}
	if st.Items[0].Source != SourceMirror {
		t.Errorf("item source %q", st.Items[0].Source)
	}
}

// Both sources fail; after the server recovers, Retry with another source
// finishes the same task.
func TestRetryWithOtherSource(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mu.Lock()
	e.srv.status["/mirror/"] = 502
	e.srv.status["/default/hf/ov-tiny/"] = 503
	e.srv.mu.Unlock()
	m := e.open()
	id, _ := m.Install([]string{ovModel}, Sources{Models: "hf-mirror"})
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeHTTPStatus || len(st.Notices) != 1 {
		t.Fatalf("%s %+v %+v", st.State, st.Error, st.Notices)
	}
	if err := m.Retry("nope", nil); CodeOf(err) != CodeTaskNotFound {
		t.Errorf("unknown task: %v", err)
	}
	e.srv.mu.Lock()
	delete(e.srv.status, "/default/hf/ov-tiny/")
	e.srv.mu.Unlock()
	if err := m.Retry(id, &Sources{Models: SourceDefault}); err != nil {
		t.Fatal(err)
	}
	st = wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskDone || st.Error != nil || len(st.Notices) != 0 || st.Sources.Models != SourceDefault ||
		st.Items[0].Source != SourceDefault {
		t.Fatalf("after retry: %+v", st)
	}
	checkModelFile(t, m, ovContent(e))
	if err := m.Retry(id, nil); CodeOf(err) != CodeTaskState {
		t.Errorf("retrying a finished task: %v", err)
	}
	if err := m.Retry(id, &Sources{Models: "nowhere"}); CodeOf(err) == "" {
		t.Error("an unknown source must be refused")
	}
}

// The package index mirror fails for uv; the install retries with the
// default index and notes it.
func TestPyPIMirrorFallsBackToDefault(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("LF_FAKE_UV_FAIL_INDEX", "/mirror/")
	m := e.open()
	st := mustInstall(t, m, Sources{PyPI: "mirror"}, IDASRCUDA)
	py, ok := m.Python(BackendFasterWhisper)
	if !ok {
		t.Fatal("asr-cuda not installed")
	}
	data, err := os.ReadFile(filepath.Join(sitePackages(py), "fake_installed.txt"))
	if err != nil || !strings.Contains(string(data), "index="+e.url("/default/simple")) {
		t.Fatalf("installed from: %q %v", data, err)
	}
	found := 0
	for _, n := range st.Notices {
		if n.Code == CodeMirrorFallback && n.Params["group"] == GroupPyPI && n.Params["mirror"] == "mirror" &&
			n.Params["reason"] == CodeNetwork {
			found++
		}
	}
	if found != 1 { // python's base venv and asr-cuda: one notice (deduplicated)
		t.Errorf("notices: %+v", st.Notices)
	}
	// the runtime archive came from the mirror (it works for files)
	if e.srv.count("/mirror/python/") != 1 || e.srv.count("/default/python/") != 0 {
		t.Errorf("runtime requests: %+v", e.srv.log())
	}
}

func sitePackages(python string) string {
	venv := filepath.Dir(filepath.Dir(python))
	if strings.HasSuffix(strings.ToLower(python), ".exe") {
		return filepath.Join(venv, "Lib", "site-packages")
	}
	return filepath.Join(venv, "lib", "python3.12", "site-packages")
}
