// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// ssh-mvp: the SSH servers in the admission (remote_devices.go) — the
// prefer / local / supplement order, the servers' waiting_for, pins,
// hand-backs, and the guard that without servers nothing changes.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// fakeSSHDevice is a remote.Device whose Ready answers ready / wf.
type fakeSSHDevice struct {
	mu    sync.Mutex
	id    string
	ready bool
	wf    string
	slots int
	asks  int
}

func (d *fakeSSHDevice) ID() string { return d.id }
func (d *fakeSSHDevice) Ready(context.Context, float64) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asks++
	return d.ready, d.wf
}
func (d *fakeSSHDevice) Slots() int { return max(d.slots, 1) }
func (d *fakeSSHDevice) Transcribe(context.Context, remote.Job, func(remote.Event)) (string, error) {
	return "", errors.New("not used: the scheduler tests run devStage")
}
func (d *fakeSSHDevice) Card(context.Context) remote.Card {
	return remote.Card{ID: d.id, Kind: remote.KindSSH}
}

func (d *fakeSSHDevice) set(ready bool, wf string) {
	d.mu.Lock()
	d.ready, d.wf = ready, wf
	d.mu.Unlock()
}

type fakeFleet struct {
	mu   sync.Mutex
	devs map[string]*fakeSSHDevice
}

func (f *fakeFleet) Device(id string) (remote.Device, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devs[id]
	if !ok {
		return nil, false
	}
	return d, true
}

func sshRow(slug string, priority int) model.ASRDevice {
	d := devRow(model.SSHDeviceID(slug), priority, 1)
	d.Backend, d.Model, d.Precision = model.BackendMLX, "large-v3-turbo", model.PrecisionFP16
	return d
}

// newSSHEnv is a devEnv with the given rows and one fake server per ssh row.
func newSSHEnv(t *testing.T, rows []model.ASRDevice) (*devEnv, *fakeFleet) {
	t.Helper()
	de := newDevEnv(t, rows, nil)
	fl := &fakeFleet{devs: map[string]*fakeSSHDevice{}}
	for _, r := range rows {
		if model.IsSSHDevice(r.ID) {
			fl.devs[r.ID] = &fakeSSHDevice{id: r.ID, ready: true}
		}
	}
	de.s.SetSSH(fl)
	return de, fl
}

func TestSSHPreferGroupGoesFirst(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	mac := sshRow("mac", 400)
	de, _ := newSSHEnv(t, []model.ASRDevice{cuda, mac})
	l1, l2, l3 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2, l3)
	de.start()
	de.waitRunningOn(l1, mac.ID)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.stillQueued(l3, model.WaitGPUSlot)
	// Bound like any auto job: the server's row, still auto.
	want := model.ASRSnapshot{DeviceID: mac.ID, Backend: model.BackendMLX, Model: mac.Model, Precision: mac.Precision,
		Language: "en", Auto: true}
	if got := de.asrSnap(l1); !model.EqualASRSnapshot(got, want) {
		t.Fatalf("snapshot %+v, want %+v", got, want)
	}
	// Outside the power policy: not in the policy's device view.
	if st := de.status(mac.ID); st.BatteryHold {
		t.Fatalf("status %+v", st)
	}
	de.asr.release(l1.ID)
	de.waitRunningOn(l3, mac.ID)
}

func TestSSHPreferOfflineFallsToLocal(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	mac := sshRow("mac", 400)
	de, fl := newSSHEnv(t, []model.ASRDevice{cuda, mac})
	fl.devs[mac.ID].set(false, model.WaitRemoteOffline)
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	// The local device is busy and the server offline: the local reason.
	de.stillQueued(l2, model.WaitGPUSlot)
	fl.devs[mac.ID].set(true, "")
	de.s.Kick()
	de.waitRunningOn(l2, mac.ID)
}

func TestSSHSupplementWhenLocalIsBusy(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	srv := sshRow("box", 6)
	de, _ := newSSHEnv(t, []model.ASRDevice{cuda, srv})
	l1, l2, l3 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2, l3)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.waitRunningOn(l2, srv.ID)
	de.stillQueued(l3, model.WaitGPUSlot)
}

func TestSSHOnlyServerWaitingFor(t *testing.T) {
	srv := sshRow("box", 6)
	de, fl := newSSHEnv(t, []model.ASRDevice{srv})
	fl.devs[srv.ID].set(false, model.WaitRemoteSetup)
	l := de.lectureOn(autoOn(srv))
	de.prepDone(l)
	de.start()
	de.stillQueued(l, model.WaitRemoteSetup)
	fl.devs[srv.ID].set(false, model.WaitRemoteOffline)
	de.s.Kick()
	de.waitReason(l, model.WaitRemoteOffline)
	fl.devs[srv.ID].set(true, "")
	de.s.Kick()
	de.waitRunningOn(l, srv.ID)
}

func TestSSHBatteryPrefersRemote(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	srv := sshRow("box", 6)
	de, _ := newSSHEnv(t, []model.ASRDevice{cuda, srv})
	de.power.set(onBattery)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, srv.ID)
}

func TestSSHManualModeAndExclusions(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	mac := sshRow("mac", 400)
	de, fl := newSSHEnv(t, []model.ASRDevice{cuda, mac})
	de.setASR(func(a *model.ASRSettings) {
		a.DeviceMode = model.ASRDeviceModeManual
		a.ManualDevices = []string{model.DeviceCUDA0}
	})
	l1 := de.lectureOn(autoOn(cuda))
	// A lecture a remote device handed back is never sent to one again.
	l2 := de.lectureOn(autoOn(cuda))
	if err := de.st.SetJobStats(de.ctx, de.lj(l2.ID, model.StageASR).ID,
		model.JobStats{RemoteFallback: &model.ErrorInfo{Code: remote.CodeOffline}}); err != nil {
		t.Fatal(err)
	}
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.stillQueued(l2, model.WaitGPUSlot)
	de.setASR(func(a *model.ASRSettings) { a.DeviceMode = model.ASRDeviceModeAuto; a.ManualDevices = nil })
	de.s.Kick()
	de.stillQueued(l2, model.WaitGPUSlot) // still never to the server
	if fl.devs[mac.ID].asks != 0 {
		t.Fatalf("the server was asked %d times", fl.devs[mac.ID].asks)
	}
	// A paused server is skipped.
	l3 := de.lectureOn(autoOn(cuda))
	if err := de.s.SetDevicePaused(mac.ID, true, false); err != nil {
		t.Fatal(err)
	}
	de.prepDone(l3)
	de.stillQueued(l3, model.WaitGPUSlot)
}

func TestUseSSHPinsTheServer(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	srv := sshRow("box", 6)
	de, fl := newSSHEnv(t, []model.ASRDevice{cuda, srv})
	fl.devs[srv.ID].set(false, model.WaitRemoteOffline)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if err := de.s.UseSSH(de.ctx, l.ID, srv.ID); err != nil {
		t.Fatal(err)
	}
	// Pinned: the local device is free, but the lecture waits for its server.
	de.stillQueued(l, model.WaitRemoteOffline)
	if snap := de.asrSnap(l); snap.Auto || snap.DeviceID != srv.ID {
		t.Fatalf("snapshot %+v", snap)
	}
	fl.devs[srv.ID].set(true, "")
	de.s.Kick()
	de.waitRunningOn(l, srv.ID)
	if err := de.s.UseSSH(de.ctx, l.ID, "ssh:unknown"); err == nil {
		t.Fatal("an unknown server was accepted")
	}
}

func TestSSHFallbackGoesBackToLocal(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	srv := sshRow("box", 6)
	de, _ := newSSHEnv(t, []model.ASRDevice{cuda, srv})
	failed := false
	var fmu sync.Mutex
	de.asr.hook = func(ctx context.Context, id, dev string) error {
		if dev == srv.ID {
			fmu.Lock()
			first := !failed
			failed = true
			fmu.Unlock()
			if first {
				return &remote.Error{Kind: remote.ErrFallback, Code: remote.CodeResultInvalid,
					Err: &model.CodedError{Info: model.ErrorInfo{Code: remote.CodeResultInvalid, Params: map[string]any{"device": "box"}},
						Err: errors.New("refused")}}
			}
		}
		de.asr.mu.Lock()
		ch := de.asr.ch[id]
		de.asr.mu.Unlock()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l := de.lectureOn(fixedOn(srv)) // pinned to the server
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	j := de.lj(l.ID, model.StageASR)
	if j.Stats.RemoteFallback == nil || j.Stats.RemoteFallback.Code != remote.CodeResultInvalid || j.Stats.RemoteRequeues != 1 {
		t.Fatalf("stats after the hand-back: %+v", j.Stats)
	}
	if snap := de.asrSnap(l); snap.DeviceID != model.DeviceCUDA0 {
		t.Fatalf("snapshot after the hand-back: %+v", snap)
	}
	if got := de.asr.startsOf(l.ID); !slices.Equal(got, []string{srv.ID, model.DeviceCUDA0}) {
		t.Fatalf("starts %v", got)
	}
}

func TestSSHBusyRequeuesAsItWas(t *testing.T) {
	srv := sshRow("box", 6)
	de, _ := newSSHEnv(t, []model.ASRDevice{srv})
	var n int
	var mu sync.Mutex
	de.asr.hook = func(ctx context.Context, id, dev string) error {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			return &remote.Error{Kind: remote.ErrBusy, Code: remote.CodeOffline, WaitingFor: model.WaitRemoteOffline,
				Err: &model.CodedError{Info: model.ErrorInfo{Code: remote.CodeOffline}, Err: errors.New("offline")}}
		}
		return nil
	}
	l := de.lectureOn(autoOn(srv))
	de.prepDone(l)
	de.start()
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	j := de.lj(l.ID, model.StageASR)
	if j.Stats.RemoteFallback != nil || j.Stats.RemoteRequeues != 1 || j.Attempt > 1 {
		t.Fatalf("busy hand-back changed the job: %+v attempt %d", j.Stats, j.Attempt)
	}
}

// The guard: without SSH servers (no fleet, or a fleet without rows) the
// admission is the earlier one, start for start. Compared are the starts
// that do not depend on timing: the first two lectures on the two devices
// in priority order, the third waiting for a slot, and then one start each
// for the last two on one of the local devices. Which of the two devices
// each of those gets depends on which run ends first (flake-hunt #2), so
// it is not compared.
func TestSSHAbsentKeepsAdmissionUnchanged(t *testing.T) {
	local := []string{model.DeviceCUDA0, model.DeviceOVGPU}
	scenario := func(withFleet bool) []string {
		cuda := devRow(model.DeviceCUDA0, 300, 1)
		gpu := devRow(model.DeviceOVGPU, 200, 1)
		de := newDevEnv(t, []model.ASRDevice{cuda, gpu}, nil)
		if withFleet {
			de.s.SetSSH(&fakeFleet{devs: map[string]*fakeSSHDevice{}})
		}
		var ls []model.Lecture
		for i := 0; i < 4; i++ {
			ls = append(ls, de.lectureOn(autoOn(cuda)))
		}
		de.prepDone(ls...)
		de.start()
		de.waitRunningOn(ls[0], model.DeviceCUDA0)
		de.waitRunningOn(ls[1], model.DeviceOVGPU)
		de.stillQueued(ls[2], model.WaitGPUSlot)
		de.stillQueued(ls[3], model.WaitGPUSlot)
		for _, l := range ls {
			de.asr.release(l.ID)
		}
		for _, l := range ls {
			de.waitStatus(l.ID, model.StageASR, model.StatusDone)
		}
		if n := de.asr.startCount(); n != 4 {
			t.Fatalf("with a fleet %v: %d starts, want one per lecture", withFleet, n)
		}
		var out []string
		for i, l := range ls {
			starts := de.asr.startsOf(l.ID)
			if len(starts) != 1 || !slices.Contains(local, starts[0]) {
				t.Fatalf("with a fleet %v: lecture %d started on %v, want once on a local device", withFleet, i, starts)
			}
			if i < 2 {
				out = append(out, fmt.Sprintf("%d@%s", i, starts[0]))
			}
		}
		return out
	}
	a, b := scenario(false), scenario(true)
	if want := []string{"0@" + model.DeviceCUDA0, "1@" + model.DeviceOVGPU}; !slices.Equal(a, want) || !slices.Equal(b, want) {
		t.Fatalf("without a fleet %v, with an empty fleet %v; want %v in both", a, b, want)
	}
}

// An SSH server handed the lecture back and no local device can take it:
// its own waiting_for (Colab's remote_fallback offers "改用 Colab 处理").
func TestSSHFallbackWithoutLocalDevice(t *testing.T) {
	srv := sshRow("box", 6)
	de, fl := newSSHEnv(t, []model.ASRDevice{srv})
	l := de.lectureOn(autoOn(srv))
	if err := de.st.SetJobStats(de.ctx, de.lj(l.ID, model.StageASR).ID,
		model.JobStats{RemoteFallback: &model.ErrorInfo{Code: remote.CodeResultInvalid, Params: map[string]any{"device": "box"}}}); err != nil {
		t.Fatal(err)
	}
	de.prepDone(l)
	de.start()
	de.stillQueued(l, model.WaitRemoteFallbackSSH)
	if fl.devs[srv.ID].asks != 0 {
		t.Fatal("a handed-back lecture was offered to the server again")
	}
}

func TestSSHSelfTestRefusal(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	srv := sshRow("box", 6)
	de, _ := newSSHEnv(t, []model.ASRDevice{cuda, srv})
	de.s.devTest = newFakeTest(1).run
	de.start()
	_, err := de.s.StartDeviceTest(de.ctx, srv.ID)
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != remote.CodeSelfTestSSH {
		t.Fatalf("StartDeviceTest(%s) = %v", srv.ID, err)
	}
}
