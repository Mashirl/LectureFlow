// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// fresh-cuda (component_admit.go): a local device whose components are not
// installed, or are being installed, takes no lecture; the lecture waits
// for it with a reason naming it, or Colab takes it by the overflow rules.
// Nothing changes once the components are installed, without a component
// manager, in test mode, or with a configured interpreter.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// fakeComponents is a component manager as config.Config.Components sees
// it (Python, ModelDir) with its unfinished tasks (devices.ComponentTasks).
// Models match by backend and name (any precision, enough here).
type fakeComponents struct {
	mu         sync.Mutex
	pythons    map[string]string // backend → interpreter of the installed environment
	models     map[string]string // backend|name → directory of the installed model
	pyBusy     map[string]bool   // backend → a task installs its environment
	modelsBusy map[string]bool   // backend|name → a task installs the model
}

var _ devices.ComponentTasks = (*fakeComponents)(nil)

func newFakeComponents() *fakeComponents {
	return &fakeComponents{pythons: map[string]string{}, models: map[string]string{}, pyBusy: map[string]bool{}, modelsBusy: map[string]bool{}}
}

func (f *fakeComponents) Python(backend string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pythons[backend]
	return p, ok
}

func (f *fakeComponents) ModelDir(backend, name, precision string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.models[backend+"|"+name]
	return d, ok
}

func (f *fakeComponents) PythonInstalling(backend string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pyBusy[backend]
}

func (f *fakeComponents) ModelInstalling(backend, name, precision string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelsBusy[backend+"|"+name]
}

// installing marks the environment of backend (and its model, when mdl is
// set) as being installed.
func (f *fakeComponents) installing(backend, mdl string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pyBusy[backend] = true
	if mdl != "" {
		f.modelsBusy[backend+"|"+mdl] = true
	}
}

// installEnv installs the environment of backend (its task ends).
func (f *fakeComponents) installEnv(backend string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pythons[backend] = filepath.Join("C:/components", backend, "venv", "python.exe")
	delete(f.pyBusy, backend)
}

// installModel installs a model at dir (its task ends).
func (f *fakeComponents) installModel(backend, mdl, dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models[backend+"|"+mdl] = dir
	delete(f.modelsBusy, backend+"|"+mdl)
}

// installFW installs asr-cuda and large-v3 (the model of devRow's cuda:0).
func (f *fakeComponents) installFW() {
	f.installEnv(model.BackendFasterWhisper)
	f.installModel(model.BackendFasterWhisper, "large-v3", "C:/components/model__faster-whisper__large-v3__float16/1.0.0/model")
}

// installOV installs asr-openvino and large-v3-turbo (devRow's OpenVINO
// model), with the model's files in a temporary directory.
func (f *fakeComponents) installOV(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range devices.ModelFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.installEnv(model.BackendOpenVINO)
	f.installModel(model.BackendOpenVINO, "large-v3-turbo", dir)
}

// componentEnv is a computer with the component manager and no configured
// interpreters (the desktop application): rows, with Colab enabled and
// authorised when colab is set.
func componentEnv(t *testing.T, colab bool, rows ...model.ASRDevice) (*devEnv, *fakeComponents) {
	t.Helper()
	var de *devEnv
	if colab {
		de = newRemoteEnv(t, rows, colabOn).devEnv
	} else {
		de = newDevEnv(t, rows, nil)
	}
	fc := newFakeComponents()
	de.cfg.Python, de.cfg.PythonOpenVINO, de.cfg.PythonMLX = "", "", ""
	de.cfg.Components = fc
	return de, fc
}

var cudaRow = devRow(model.DeviceCUDA0, 300, 1)

// Without Colab: not installed and being installed wait for cuda:0 (each
// with its reason) and start there by themselves once installed; installed
// runs at once. The model is a component too.
func TestComponentAdmissionWithoutColab(t *testing.T) {
	cuda := cudaRow
	fw := model.BackendFasterWhisper
	t.Run("not installed", func(t *testing.T) {
		de, fc := componentEnv(t, false, cuda)
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.stillQueued(l, devices.WaitComponentMissing(model.DeviceCUDA0))
		if n := len(de.asr.startsOf(l.ID)); n != 0 {
			t.Fatalf("started %d times on a device without its component", n)
		}
		fc.installing(fw, "large-v3")
		de.stillQueued(l, devices.WaitComponentInstalling(model.DeviceCUDA0))
		fc.installFW()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
	t.Run("being installed", func(t *testing.T) {
		de, fc := componentEnv(t, false, cuda)
		fc.installing(fw, "large-v3")
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.stillQueued(l, devices.WaitComponentInstalling(model.DeviceCUDA0))
		fc.installEnv(fw) // asr-cuda done; the model still downloads
		de.stillQueued(l, devices.WaitComponentInstalling(model.DeviceCUDA0))
		fc.installFW()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
	t.Run("installed", func(t *testing.T) {
		de, fc := componentEnv(t, false, cuda)
		fc.installFW()
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.waitRunningOn(l, model.DeviceCUDA0)
		if got := de.asrSnap(l); got.DeviceID != model.DeviceCUDA0 || !got.Auto {
			t.Fatalf("snapshot %+v", got)
		}
	})
	t.Run("model not installed", func(t *testing.T) {
		de, fc := componentEnv(t, false, cuda)
		fc.installEnv(fw)
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.stillQueued(l, devices.WaitModelMissing(model.DeviceCUDA0))
		fc.installing(fw, "large-v3")
		de.stillQueued(l, devices.WaitComponentInstalling(model.DeviceCUDA0))
		fc.installFW()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
}

// With Colab: not installed (environment or model) is a computer without a
// local device — Colab takes the lectures one after the other (remote_busy
// meanwhile); being installed is a busy device — Colab takes one, the next
// waits for the install and runs locally; installed is unchanged (local
// first, Colab the overflow).
func TestComponentAdmissionWithColab(t *testing.T) {
	cuda := cudaRow
	fw := model.BackendFasterWhisper
	for _, tc := range []struct {
		name  string
		setup func(fc *fakeComponents)
	}{
		{"not installed", func(fc *fakeComponents) {}},
		{"model not installed", func(fc *fakeComponents) { fc.installEnv(fw) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			de, fc := componentEnv(t, true, cuda)
			tc.setup(fc)
			l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
			de.prepDone(l1, l2)
			de.start()
			de.waitRunningOn(l1, model.DeviceColab)
			de.stillQueued(l2, model.WaitRemoteBusy)
			// installed meanwhile: the next lecture runs locally
			fc.installFW()
			de.waitRunningOn(l2, model.DeviceCUDA0)
			if devs := de.asr.startsOf(l1.ID); len(devs) != 1 || devs[0] != model.DeviceColab {
				t.Fatalf("l1 ran on %v", devs)
			}
		})
	}
	t.Run("being installed", func(t *testing.T) {
		de, fc := componentEnv(t, true, cuda)
		fc.installing(fw, "large-v3")
		l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
		de.prepDone(l1, l2)
		de.start()
		de.waitRunningOn(l1, model.DeviceColab)
		de.stillQueued(l2, devices.WaitComponentInstalling(model.DeviceCUDA0))
		fc.installFW()
		de.waitRunningOn(l2, model.DeviceCUDA0)
	})
	t.Run("installed", func(t *testing.T) {
		de, fc := componentEnv(t, true, cuda)
		fc.installFW()
		l1, l2, l3 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
		de.prepDone(l1, l2, l3)
		de.start()
		de.waitRunningOn(l1, model.DeviceCUDA0)
		de.waitRunningOn(l2, model.DeviceColab)
		de.stillQueued(l3, model.WaitGPUSlot)
	})
}

// Another local device with its components takes the lecture instead (a
// desktop: the next device in the list; a laptop on AC: the Intel GPU as
// P_ac). A fixed snapshot runs on its own device only: it waits for the
// component, Colab or not.
func TestComponentAdmissionOtherDevices(t *testing.T) {
	cuda := cudaRow
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	for _, tc := range []struct {
		name  string
		power devices.Power
	}{{"desktop", onAC}, {"laptop on AC", onACLaptop}} {
		t.Run(tc.name, func(t *testing.T) {
			de, fc := componentEnv(t, false, cuda, igpu)
			de.power.set(tc.power)
			fc.installOV(t)
			l := de.lectureOn(autoOn(cuda))
			de.prepDone(l)
			de.start()
			de.waitRunningOn(l, model.DeviceOVGPU)
		})
	}
	for _, tc := range []struct {
		name  string
		colab bool
	}{{"fixed snapshot", false}, {"fixed snapshot with Colab", true}} {
		t.Run(tc.name, func(t *testing.T) {
			de, fc := componentEnv(t, tc.colab, cuda, igpu)
			fc.installOV(t)
			l := de.lectureOn(fixedOn(cuda))
			de.prepDone(l)
			de.start()
			de.stillQueued(l, devices.WaitComponentMissing(model.DeviceCUDA0))
			fc.installFW()
			de.waitRunningOn(l, model.DeviceCUDA0)
		})
	}
}

// On a laptop the policy's own waits stay when the device without its
// component would not have been used anyway (CUDA on battery), and become
// the component's when it would (on AC).
func TestComponentAdmissionLaptop(t *testing.T) {
	cuda := cudaRow
	de, fc := componentEnv(t, false, cuda)
	de.power.set(onBattery)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.stillQueued(l, model.WaitPowerNoSaverDevice)
	de.power.set(onACLaptop)
	de.s.Kick()
	de.stillQueued(l, devices.WaitComponentMissing(model.DeviceCUDA0))
	fc.installFW()
	de.waitRunningOn(l, model.DeviceCUDA0)
}

// The earlier admission where this does not decide: no component manager
// (the stage reports the missing interpreter), test mode, and a configured
// interpreter that exists (development .venv: any model).
func TestComponentAdmissionUnchanged(t *testing.T) {
	cuda := cudaRow
	t.Run("no component manager", func(t *testing.T) {
		de := newDevEnv(t, []model.ASRDevice{cuda}, nil)
		de.cfg.Python = ""
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
	t.Run("test mode", func(t *testing.T) {
		de, _ := componentEnv(t, false, cuda)
		de.cfg.ASR.FakeResult = "testdata/fake_asr.json"
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
	t.Run("configured interpreter", func(t *testing.T) {
		de, _ := componentEnv(t, false, cuda)
		py := filepath.Join(t.TempDir(), "python.exe")
		if err := os.WriteFile(py, []byte("interpreter"), 0o755); err != nil {
			t.Fatal(err)
		}
		de.cfg.Python = py
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.waitRunningOn(l, model.DeviceCUDA0)
	})
	t.Run("configured interpreter missing", func(t *testing.T) {
		de, _ := componentEnv(t, false, cuda)
		de.cfg.Python = filepath.Join(t.TempDir(), ".venv", "Scripts", "python.exe")
		l := de.lectureOn(autoOn(cuda))
		de.prepDone(l)
		de.start()
		de.stillQueued(l, devices.WaitComponentMissing(model.DeviceCUDA0))
	})
}

// The component waits in Colab's terms: not installed counts as no local
// device (Colab only), being installed as a busy one (whichever frees
// first); the policy's no-device waits are the ones componentWait refines.
func TestComponentWaitReasons(t *testing.T) {
	missing, noModel, installing := devices.WaitComponentMissing(model.DeviceCUDA0), devices.WaitModelMissing(model.DeviceCUDA0),
		devices.WaitComponentInstalling(model.DeviceCUDA0)
	for _, w := range []string{missing, noModel} {
		if !localUnavailable(w) || remoteBusyReason(w) != model.WaitRemoteBusy || remoteHeldReason(w) != model.WaitRemoteHeld {
			t.Errorf("%s: unavailable %v, busy %q, held %q", w, localUnavailable(w), remoteBusyReason(w), remoteHeldReason(w))
		}
	}
	if localUnavailable(installing) || remoteBusyReason(installing) != installing || remoteHeldReason(installing) != installing {
		t.Errorf("%s: unavailable %v, busy %q, held %q", installing, localUnavailable(installing), remoteBusyReason(installing), remoteHeldReason(installing))
	}
	for w, want := range map[string]bool{
		model.WaitNoASRDevice: true, model.WaitPowerNoDevice: true, model.WaitPowerNoSaverDevice: true,
		model.WaitDevicePaused(model.DeviceCUDA0): false, model.WaitGPUSlot: false, model.WaitPowerQueue(model.DeviceCUDA0): false,
	} {
		if got := noLocalDevice(w); got != want {
			t.Errorf("noLocalDevice(%q) = %v", w, got)
		}
	}
}
