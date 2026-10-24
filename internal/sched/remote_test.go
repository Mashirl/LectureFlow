// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// SPEC-v3 V14.3–V14.5 (v14-colab-integrate): colab:remote in the
// scheduler — overflow when the local devices are busy, battery_prefer,
// suspended CUDA jobs never handed over, manual mode, CanDispatch's
// waiting_for, hand-back (IsFallback) and requeue (IsRequeue), the planned
// queue and its drain, "改用 Colab 处理".

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// fakeRemote is a RemoteBackend: CanDispatch answers can / wf; the stage is
// devStage (its hook decides what a run on colab:remote does).
type fakeRemote struct {
	mu        sync.Mutex
	cred      bool
	can       bool
	wf        string
	set       colab.Settings
	queues    [][]float64
	drained   int
	shutNow   int
	shutAfter int
	asks      int
	// nowCh is closed by ShutdownNow (the running remote lecture returns
	// NewRequeueError, see remoteHook).
	nowCh chan struct{}
}

func newFakeRemote() *fakeRemote {
	set := colab.DefaultSettings()
	return &fakeRemote{cred: true, can: true, set: set, nowCh: make(chan struct{})}
}

func (f *fakeRemote) CanDispatch(ctx context.Context, sec float64) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	return f.can, f.wf
}

func (f *fakeRemote) SetQueue(q []float64) {
	f.mu.Lock()
	f.queues = append(f.queues, slices.Clone(q))
	f.mu.Unlock()
}

func (f *fakeRemote) QueueDrained(ctx context.Context) error {
	f.mu.Lock()
	f.drained++
	f.mu.Unlock()
	return nil
}

func (f *fakeRemote) ShutdownAfterCurrent(ctx context.Context) error {
	f.mu.Lock()
	f.shutAfter++
	f.mu.Unlock()
	return nil
}

func (f *fakeRemote) ShutdownNow(ctx context.Context) error {
	f.mu.Lock()
	f.shutNow++
	ch := f.nowCh
	f.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
	return nil
}

func (f *fakeRemote) Settings() colab.Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set
}

func (f *fakeRemote) Credential(ctx context.Context) colab.CredentialInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return colab.CredentialInfo{Present: f.cred, RemainingSec: -1}
}

func (f *fakeRemote) Status(ctx context.Context) colab.Status {
	return colab.Status{Service: "colab", Accelerator: f.Settings().Accelerator, State: colab.StateNone}
}

func (f *fakeRemote) Transcribe(ctx context.Context, job colab.Job, progress func(colab.Event)) (string, error) {
	return "", errors.New("not used by the scheduler tests")
}

func (f *fakeRemote) setCan(can bool, wf string) {
	f.mu.Lock()
	f.can, f.wf = can, wf
	f.mu.Unlock()
}

func (f *fakeRemote) counts() (drained, now, after, asks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained, f.shutNow, f.shutAfter, f.asks
}

func (f *fakeRemote) lastQueue() ([]float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queues) == 0 {
		return nil, false
	}
	return f.queues[len(f.queues)-1], true
}

// remoteEnv is a devEnv with the fake remote wired like cmd does (OnRemote
// adds the colab:remote row and keeps its presence).
type remoteEnv struct {
	*devEnv
	fr *fakeRemote
	// remoteErr is what the next run on colab:remote returns once released
	// (nil = done); a run returns NewRequeueError when ShutdownNow is called.
	mu        sync.Mutex
	remoteErr error
}

func newRemoteEnv(t *testing.T, rows []model.ASRDevice, prefs RemotePrefs) *remoteEnv {
	t.Helper()
	re := &remoteEnv{fr: newFakeRemote()}
	var de *devEnv
	de = newDevEnv(t, rows, func(o *Options) {
		o.Remote = re.fr
		o.OnRemote = func(ctx context.Context, present bool, row model.ASRDevice) {
			if present {
				if _, ok := de.devs.Get(row.ID); !ok {
					de.devs.add(row)
				}
			}
			if _, ok := de.devs.Get(row.ID); ok {
				de.devs.setPresent(row.ID, present)
			}
		}
	})
	re.devEnv = de
	if err := WriteRemotePrefs(de.ctx, de.st, prefs); err != nil {
		t.Fatal(err)
	}
	de.asr.hook = re.hook
	return re
}

// hook: a run on colab:remote waits for release, then returns remoteErr
// (or NewRequeueError after ShutdownNow); a local run behaves as devStage.
func (re *remoteEnv) hook(ctx context.Context, id, dev string) error {
	d := re.asr
	d.mu.Lock()
	ch, pass := d.ch[id], d.pass[id]
	d.mu.Unlock()
	if dev != model.DeviceColab {
		if pass {
			return nil
		}
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	re.fr.mu.Lock()
	now := re.fr.nowCh
	re.fr.mu.Unlock()
	if !pass {
		select {
		case <-ch:
		case <-now:
			return colab.NewRequeueError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	re.mu.Lock()
	err := re.remoteErr
	re.remoteErr = nil
	re.mu.Unlock()
	return err
}

func (re *remoteEnv) failRemoteNext(err error) {
	re.mu.Lock()
	re.remoteErr = err
	re.mu.Unlock()
}

var colabOn = RemotePrefs{Enabled: true}

func TestRemoteTakesALectureWhenLocalDevicesAreBusy(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	l1, l2, l3 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2, l3)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitRunningOn(l2, model.DeviceColab)
	re.stillQueued(l3, model.WaitGPUSlot) // Colab runs one lecture at a time

	// The remote run is bound like any auto job: device, backend and Colab's
	// model and precision; still auto.
	set := re.fr.Settings()
	want := model.ASRSnapshot{DeviceID: model.DeviceColab, Backend: model.BackendColab, Model: set.Model,
		Precision: set.ComputeType, Language: "en", Auto: true}
	if got := re.asrSnap(l2); !model.EqualASRSnapshot(got, want) {
		t.Fatalf("snapshot of the remote job = %+v, want %+v", got, want)
	}
	// colab:remote is in the device table, outside the power policy.
	if st := re.status(model.DeviceColab); st.SlotsUsed != 1 || st.BatteryHold {
		t.Fatalf("colab:remote status = %+v", st)
	}
	// The planned queue holds l3 (the lecture that waits and could go there).
	re.waitFor("planned queue of one lecture", 5*time.Second, func() bool {
		q, ok := re.fr.lastQueue()
		return ok && len(q) == 1
	})

	re.asr.release(l2.ID)
	re.waitRunningOn(l3, model.DeviceColab)
	re.asr.release(l1.ID)
	re.asr.release(l3.ID)
	for _, l := range []model.Lecture{l1, l2, l3} {
		re.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	// Nothing more for Colab: the queue is drained (the runtime is released).
	re.waitFor("queue drained", 5*time.Second, func() bool {
		d, _, _, _ := re.fr.counts()
		return d >= 1
	})
	if q, _ := re.fr.lastQueue(); len(q) != 0 {
		t.Fatalf("last planned queue = %v, want empty", q)
	}
	if st := re.lj(l2.ID, model.StageASR).Stats; st.Remote != nil || st.RemoteRequeues != 0 {
		t.Fatalf("stats of the remote job after done = %+v", st)
	}
}

func TestRemoteOffOrWithoutCredentialIsNeverUsed(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	for _, tc := range []struct {
		name  string
		prefs RemotePrefs
		cred  bool
	}{{"disabled", RemotePrefs{}, true}, {"no credential", colabOn, false}} {
		t.Run(tc.name, func(t *testing.T) {
			re := newRemoteEnv(t, []model.ASRDevice{cuda}, tc.prefs)
			re.fr.mu.Lock()
			re.fr.cred = tc.cred
			re.fr.mu.Unlock()
			l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
			re.prepDone(l1, l2)
			re.start()
			re.waitRunningOn(l1, model.DeviceCUDA0)
			re.stillQueued(l2, model.WaitGPUSlot)
			if re.devs.Present(model.DeviceColab) {
				t.Fatal("colab:remote is in the device table")
			}
			if _, _, _, asks := re.fr.counts(); asks != 0 {
				t.Fatalf("CanDispatch asked %d times", asks)
			}
			re.asr.release(l1.ID)
			re.asr.release(l2.ID)
			re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
			if !re.asr.ran(l2.ID, model.DeviceCUDA0) {
				t.Fatalf("l2 ran on %v", re.asr.startsOf(l2.ID))
			}
		})
	}
}

func TestRemoteCanDispatchWaitingFor(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.fr.setCan(false, model.WaitRemoteWindow)
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.stillQueued(l2, model.WaitRemoteWindow)
	re.fr.setCan(false, model.WaitRemoteBalance)
	re.waitReason(l2, model.WaitRemoteBalance)
	re.fr.setCan(false, "") // busy / cannot ask: the local reason stays
	re.waitReason(l2, model.WaitGPUSlot)
	re.fr.setCan(true, "")
	re.waitRunningOn(l2, model.DeviceColab)
	re.asr.release(l1.ID)
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestRemoteBatteryPrefer(t *testing.T) {
	// A laptop on battery with only a CUDA device: no battery device, so
	// the local devices wait for the charger (power_no_saver_device).
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.power.set(onBattery)
	l1 := re.lectureOn(autoOn(cuda))
	re.prepDone(l1)
	re.start()
	re.stillQueued(l1, model.WaitPowerNoSaverDevice) // battery_prefer off
	if err := re.s.SetRemotePrefs(re.ctx, RemotePrefs{Enabled: true, BatteryPrefer: true}); err != nil {
		t.Fatal(err)
	}
	re.waitRunningOn(l1, model.DeviceColab)
	re.asr.release(l1.ID)
	re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
}

func TestRemoteIgnoresTheSingleActiveDeviceRule(t *testing.T) {
	// On battery the NPU is the one active local device; Colab runs next to
	// it (V14.4), and multi_device off does not hold either back.
	npu, cuda := devRow(model.DeviceOVNPU, 100, 1), devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda, npu}, RemotePrefs{Enabled: true, BatteryPrefer: true})
	re.setASR(func(a *model.ASRSettings) { a.MultiDevice = false })
	re.power.set(onBattery)
	l1, l2, l3 := re.lectureOn(autoOn(npu)), re.lectureOn(autoOn(npu)), re.lectureOn(autoOn(npu))
	re.prepDone(l1, l2, l3)
	re.start()
	re.waitRunningOn(l1, model.DeviceOVNPU)
	re.waitRunningOn(l2, model.DeviceColab)
	re.stillQueued(l3, model.WaitPowerQueue(model.DeviceOVNPU))
	// The NPU's next lecture is not held back by the run on Colab.
	re.asr.release(l1.ID)
	re.waitRunningOn(l3, model.DeviceOVNPU)
	for _, l := range []model.Lecture{l2, l3} {
		re.asr.release(l.ID)
		re.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
}

func TestRemoteNeverTakesASuspendedCUDAJob(t *testing.T) {
	// A laptop on AC transcribes on CUDA; unplugged, the run is suspended
	// (power_wait_ac). With battery_prefer on, a new lecture goes to Colab,
	// the suspended one never does (V14.4).
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, RemotePrefs{Enabled: true, BatteryPrefer: true})
	re.power.set(onACLaptop)
	re.fr.setCan(false, "") // nothing goes to Colab before the unplug
	l1 := re.lectureOn(autoOn(cuda))
	re.prepDone(l1)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.power.set(onBattery)
	re.waitReason(l1, model.WaitPowerWaitAC)
	re.fr.setCan(true, "")
	l2 := re.lectureOn(autoOn(cuda))
	re.prepDone(l2)
	re.waitRunningOn(l2, model.DeviceColab)
	re.stillQueued(l1, model.WaitPowerWaitAC)
	if slices.Contains(re.asr.startsOf(l1.ID), model.DeviceColab) {
		t.Fatalf("the suspended job ran on Colab: %v", re.asr.startsOf(l1.ID))
	}
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	re.stillQueued(l1, model.WaitPowerWaitAC)
	re.power.set(onACLaptop)
	re.asr.release(l1.ID)
	re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
}

func TestRemoteManualMode(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.setASR(func(a *model.ASRSettings) {
		a.DeviceMode, a.ManualDevices = model.ASRDeviceModeManual, []string{model.DeviceCUDA0}
	})
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.stillQueued(l2, model.WaitGPUSlot) // "只用某台设备": not Colab
	// The list names Colab: it takes the lecture the CUDA device cannot.
	re.setASR(func(a *model.ASRSettings) {
		a.ManualDevices = []string{model.DeviceCUDA0, model.DeviceColab}
	})
	re.waitRunningOn(l2, model.DeviceColab)
	re.asr.release(l1.ID)
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)

	// Only Colab in the list: every lecture goes there.
	re.setASR(func(a *model.ASRSettings) { a.ManualDevices = []string{model.DeviceColab} })
	l3 := re.lectureOn(autoOn(cuda))
	re.prepDone(l3)
	re.waitRunningOn(l3, model.DeviceColab)
	re.asr.release(l3.ID)
	re.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

func TestRemoteFallbackGoesBackToLocalDevices(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.failRemoteNext(colab.NewFallbackError(colab.CodeSessionReclaimed, nil))
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitRunningOn(l2, model.DeviceColab)
	re.asr.release(l2.ID) // the run returns the fallback error
	// Requeued for the local devices, not failed, no retry used up, and not
	// sent to Colab again although Colab is free.
	re.stillQueued(l2, model.WaitGPUSlot)
	j := re.lj(l2.ID, model.StageASR)
	if j.Stats.RemoteRequeues != 1 || j.Stats.RemoteFallback == nil || j.Stats.RemoteFallback.Code != colab.CodeSessionReclaimed ||
		model.CountedAttempts(j) != 0 || j.Error != nil && *j.Error != "" {
		t.Fatalf("job after the hand-back: attempt %d stats %+v error %v", j.Attempt, j.Stats, j.Error)
	}
	re.asr.block(l2.ID)
	re.asr.release(l1.ID)
	re.waitRunningOn(l2, model.DeviceCUDA0) // auto: bound to the local device now
	if got := re.asrSnap(l2); got.DeviceID != model.DeviceCUDA0 || !got.Auto {
		t.Fatalf("snapshot after the hand-back = %+v", got)
	}
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestRemoteShutdownNowRequeuesAndHolds(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	l1, l2, l3 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2, l3)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitRunningOn(l2, model.DeviceColab)
	if err := re.s.ShutdownRemote(re.ctx, true); err != nil {
		t.Fatal(err)
	}
	// "立即关机": l2 goes back to the queue (IsRequeue) and nothing borrows
	// a new runtime while lectures wait.
	re.stillQueued(l2, model.WaitGPUSlot)
	re.stillQueued(l3, model.WaitGPUSlot)
	if !re.s.RemoteHeld() {
		t.Fatal("no hold after the shutdown")
	}
	if _, now, _, _ := re.fr.counts(); now != 1 {
		t.Fatalf("ShutdownNow called %d times", now)
	}
	j := re.lj(l2.ID, model.StageASR)
	if j.Stats.RemoteFallback == nil || j.Stats.RemoteFallback.Code != colab.CodeShutdownRequeue || model.CountedAttempts(j) != 0 {
		t.Fatalf("l2 after the shutdown: %+v", j.Stats)
	}
	for _, l := range []model.Lecture{l1, l2, l3} {
		re.asr.release(l.ID)
	}
	for _, l := range []model.Lecture{l1, l2, l3} {
		re.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	if devs := re.asr.startsOf(l2.ID); len(devs) != 2 || devs[1] != model.DeviceCUDA0 {
		t.Fatalf("l2 ran on %v, want Colab then cuda:0", devs)
	}
	if !re.asr.ran(l3.ID, model.DeviceCUDA0) {
		t.Fatalf("l3 ran on %v during the hold", re.asr.startsOf(l3.ID))
	}
	// No lecture waits any more: the hold ends.
	re.waitFor("hold ended", 5*time.Second, func() bool { return !re.s.RemoteHeld() })
}

func TestRemoteShutdownAfterCurrent(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	l1, l2, l3 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2, l3)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitRunningOn(l2, model.DeviceColab)
	if err := re.s.ShutdownRemote(re.ctx, false); err != nil {
		t.Fatal(err)
	}
	re.asr.release(l2.ID) // the current lecture finishes on Colab
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	re.stillQueued(l3, model.WaitGPUSlot) // held: not the next one
	if _, _, after, _ := re.fr.counts(); after != 1 {
		t.Fatalf("ShutdownAfterCurrent called %d times", after)
	}
	re.asr.release(l1.ID)
	re.asr.release(l3.ID)
	re.waitStatus(l3.ID, model.StageASR, model.StatusDone)
	if !re.asr.ran(l3.ID, model.DeviceCUDA0) {
		t.Fatalf("l3 ran on %v", re.asr.startsOf(l3.ID))
	}
}

func TestUseRemote(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.fr.setCan(false, "") // no automatic dispatch in this test
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitReason(l2, model.WaitGPUSlot)

	// Colab says no: 409 with its waiting_for.
	re.fr.setCan(false, model.WaitRemoteBalance)
	err := re.s.UseRemote(re.ctx, l2.ID)
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != CodeRemoteUnavailable || ce.Info.Params["waiting_for"] != model.WaitRemoteBalance ||
		!errors.Is(err, state.ErrConflict) {
		t.Fatalf("UseRemote while Colab cannot = %v", err)
	}

	// A queued job: it starts over on Colab, pinned.
	re.fr.setCan(true, "")
	if err := re.s.UseRemote(re.ctx, l2.ID); err != nil {
		t.Fatal(err)
	}
	re.waitRunningOn(l2, model.DeviceColab)
	if j := re.lj(l2.ID, model.StageASR); !j.Stats.RemotePin {
		t.Fatalf("stats after UseRemote = %+v", j.Stats)
	}
	if err := re.s.UseRemote(re.ctx, l2.ID); err != nil { // already on Colab: nothing to do
		t.Fatal(err)
	}

	// A running local job is cancelled and starts over on Colab once free.
	if err := re.s.UseRemote(re.ctx, l1.ID); err != nil {
		t.Fatal(err)
	}
	re.waitReason(l1, model.WaitGPUSlot) // pinned, Colab busy with l2
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	re.waitRunningOn(l1, model.DeviceColab)

	// Colab hands the pinned lecture back: the pin is dropped, local again.
	re.failRemoteNext(colab.NewFallbackError(colab.CodeQueueTimeout, map[string]any{"accelerator": "T4", "minutes": 30}))
	re.asr.release(l1.ID)
	re.waitFor("l1 back on cuda:0", 5*time.Second, func() bool {
		devs := re.asr.startsOf(l1.ID)
		return len(devs) > 0 && devs[len(devs)-1] == model.DeviceCUDA0
	})
	re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	if j := re.lj(l1.ID, model.StageASR); j.Stats.RemotePin || j.Stats.RemoteFallback == nil {
		t.Fatalf("stats after the hand-back = %+v", j.Stats)
	}

	// Colab switched off: 409 remote.disabled.
	if err := re.s.SetRemotePrefs(re.ctx, RemotePrefs{}); err != nil {
		t.Fatal(err)
	}
	l3 := re.lectureOn(autoOn(cuda))
	err = re.s.UseRemote(re.ctx, l3.ID)
	if !errors.As(err, &ce) || ce.Info.Code != CodeRemoteDisabled || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("UseRemote with Colab off = %v", err)
	}
	if re.devs.Present(model.DeviceColab) {
		t.Fatal("colab:remote still in the device table after switching Colab off")
	}
}

func TestRemoteHasNoSelfTest(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.s.devTest = newFakeTest(1).run
	re.start()
	re.waitFor("colab:remote in the table", 5*time.Second, func() bool { return re.devs.Present(model.DeviceColab) })
	_, err := re.s.StartDeviceTest(re.ctx, model.DeviceColab)
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != CodeSelfTestRemote || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("StartDeviceTest(colab:remote) = %v", err)
	}
}

func TestCountedAttemptsLeavesOutRemoteRequeues(t *testing.T) {
	j := model.Job{Attempt: 3, Stats: model.JobStats{PowerInterrupts: 1, RemoteRequeues: 1}}
	if n := model.CountedAttempts(j); n != 1 {
		t.Fatalf("CountedAttempts = %d", n)
	}
}

func TestRemoteStepsMatchColab(t *testing.T) {
	want := []string{colab.StepWaitAccelerator, colab.StepUploadAudio, colab.StepRemoteTranscribe, colab.StepDownloadResult, colab.StepVerify}
	if !slices.Equal(model.RemoteSteps, want) {
		t.Fatalf("model.RemoteSteps = %v, colab = %v", model.RemoteSteps, want)
	}
}
