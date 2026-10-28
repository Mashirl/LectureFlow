// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"testing"

	"lectureflow/internal/devices"
)

// The scheduler reads the manager's unfinished tasks through this interface.
var _ devices.ComponentTasks = (*Manager)(nil)

// fresh-cuda: PythonInstalling / ModelInstalling follow one install of a
// faster-whisper model (python → asr-cuda → the model, in one task):
// being installed while its item is not done (also while the task is
// paused), not once it is installed or for components the task does not
// have.
func TestInstallingFollowsTheTask(t *testing.T) {
	e := newTestEnv(t)
	pyHit, pyRelease := e.srv.gate("/default/python/cpython-1.tar.gz")
	defer pyRelease()
	modelHit, modelRelease := e.srv.gate("/default/hf/tiny/1/model.bin")
	defer modelRelease()
	m := e.open()
	fw, ov := BackendFasterWhisper, BackendOpenVINO

	if m.PythonInstalling(fw) || m.PythonInstalling(BackendBase) || m.ModelInstalling(fw, "tiny", "float16") {
		t.Fatal("being installed before any task")
	}
	id, err := m.Install([]string{fwFloat16}, Sources{})
	if err != nil {
		t.Fatal(err)
	}

	// The python runtime downloads; asr-cuda and the model wait in the task.
	<-pyHit
	if !m.PythonInstalling(BackendBase) || !m.PythonInstalling(fw) {
		t.Error("python and asr-cuda are items of the running task")
	}
	if _, ok := m.Python(fw); ok {
		t.Error("asr-cuda installed already")
	}
	// faster-whisper: any precision of the model (ModelDir serves any).
	for _, p := range []string{"float16", "int8_float16", "auto"} {
		if !m.ModelInstalling(fw, "tiny", p) {
			t.Errorf("tiny / %s should be being installed", p)
		}
	}
	if m.ModelInstalling(fw, "small", "float16") || m.ModelInstalling(ov, "tiny", "int8") || m.PythonInstalling(ov) || m.PythonInstalling(BackendMLX) {
		t.Error("components the task does not have are being installed")
	}

	// asr-cuda is installed; the model downloads.
	pyRelease()
	<-modelHit
	if _, ok := m.Python(fw); !ok || m.PythonInstalling(fw) || m.PythonInstalling(BackendBase) {
		t.Errorf("after asr-cuda: installed %v, being installed %v / %v", ok, m.PythonInstalling(fw), m.PythonInstalling(BackendBase))
	}
	if !m.ModelInstalling(fw, "tiny", "float16") {
		t.Error("the model should be being installed")
	}

	// Paused: still unfinished.
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	if !m.ModelInstalling(fw, "tiny", "float16") {
		t.Error("a paused task still installs the model")
	}
	modelRelease()
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("install: %s %+v", st.State, st.Error)
	}
	if m.ModelInstalling(fw, "tiny", "float16") || m.PythonInstalling(fw) {
		t.Error("being installed after the task finished")
	}
	if _, ok := m.ModelDir(fw, "tiny", "auto"); !ok {
		t.Error("the model is not installed")
	}

	// OpenVINO models: the exact precision only; a cancelled task installs nothing.
	hit, release := e.srv.gate(ovFile)
	defer release()
	id, err = m.Install([]string{ovModel}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit
	if !m.ModelInstalling(ov, "tiny", "int8") || m.ModelInstalling(ov, "tiny", "fp16") {
		t.Errorf("openvino tiny: int8 %v, fp16 %v", m.ModelInstalling(ov, "tiny", "int8"), m.ModelInstalling(ov, "tiny", "fp16"))
	}
	if err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskCancelled)
	if m.ModelInstalling(ov, "tiny", "int8") {
		t.Error("a cancelled task still installs the model")
	}
}
