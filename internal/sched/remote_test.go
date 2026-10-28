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
	"lectureflow/internal/devices"
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

	// colabfix: strict makes the fake keep the real backend's books — job
	// is the lecture whose Transcribe runs (remoteEnv.hook registers it),
	// stopping is set while QueueDrained releases the runtime (it takes
	// releaseFor). CanDispatch says busy and a lecture that registers
	// meanwhile gets NewBusyError, as colab.Backend does. drainSig gets a
	// value when a release starts; drainedWhile counts the QueueDrained
	// calls made while the scheduler had a run on colab:remote (the accept2
	// race: that run's Transcribe may not have registered yet).
	strict       bool
	job          string
	stopping     bool
	releaseFor   time.Duration
	released     int
	drainSig     chan struct{}
	drainedWhile int
	remoteRun    func() bool // the scheduler has a run on colab:remote (set before start)
}

func newFakeRemote() *fakeRemote {
	set := colab.DefaultSettings()
	return &fakeRemote{cred: true, can: true, set: set, nowCh: make(chan struct{}), drainSig: make(chan struct{}, 16),
		releaseFor: 30 * time.Millisecond}
}

func (f *fakeRemote) CanDispatch(ctx context.Context, sec float64) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	if f.strict && (f.job != "" || f.stopping) {
		return false, "" // busy, as colab.Backend
	}
	return f.can, f.wf
}

func (f *fakeRemote) SetQueue(q []float64) {
	f.mu.Lock()
	f.queues = append(f.queues, slices.Clone(q))
	f.mu.Unlock()
}

func (f *fakeRemote) QueueDrained(ctx context.Context) error {
	busy := f.remoteRun != nil && f.remoteRun() // outside f.mu (it takes the scheduler's lock)
	f.mu.Lock()
	f.drained++
	if busy {
		f.drainedWhile++
	}
	if !f.strict || f.job != "" {
		f.mu.Unlock()
		return nil // released after the running lecture (not modelled)
	}
	// As colab.Backend: nothing registered, so the runtime is released now.
	f.stopping = true
	d := f.releaseFor
	f.mu.Unlock()
	select {
	case f.drainSig <- struct{}{}:
	default:
	}
	time.Sleep(d)
	f.mu.Lock()
	f.stopping = false
	f.released++
	f.mu.Unlock()
	return nil
}

// register is the start of Transcribe in strict mode: busy while the
// runtime is being released or another lecture is registered.
func (f *fakeRemote) register(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopping || f.job != "" {
		return colab.NewBusyError()
	}
	f.job = id
	return nil
}

func (f *fakeRemote) unregister(id string) {
	f.mu.Lock()
	if f.job == id {
		f.job = ""
	}
	f.mu.Unlock()
}

func (f *fakeRemote) registered() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.job
}

func (f *fakeRemote) strictCounts() (drainedWhile, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainedWhile, f.released
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
		o.RemoteBusyPause = 30 * time.Millisecond
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

// strictBackend makes the fake behave like colab.Backend around a release
// (fakeRemote.strict). Call before start.
func (re *remoteEnv) strictBackend() {
	re.fr.mu.Lock()
	re.fr.strict = true
	re.fr.remoteRun = re.s.remoteRunning
	re.fr.mu.Unlock()
}

// registerWait is how long a run on colab:remote takes in strict mode
// before its Transcribe registers with the backend (the asr stage reads the
// audio, asks for the status, refreshes the credential): it registers at
// once when a release starts meanwhile, the accept2 interleaving.
const registerWait = 50 * time.Millisecond

// hook: a run on colab:remote waits for release, then returns remoteErr
// (or NewRequeueError after ShutdownNow); a local run behaves as devStage.
// In strict mode the run first registers with the fake backend (busy:
// NewBusyError, as Transcribe).
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
	now, strict, sig := re.fr.nowCh, re.fr.strict, re.fr.drainSig
	re.fr.mu.Unlock()
	if strict {
		select {
		case <-sig: // a release started before this lecture registered
		case <-time.After(registerWait):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := re.fr.register(id); err != nil {
			return err
		}
		defer re.fr.unregister(id)
	}
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

	// A queued job: it starts over on Colab, pinned. Once Colab can take a
	// lecture, any scan would also send l2 there on its own (overflow, not
	// pinned), and UseRemote on a lecture already running on Colab does
	// nothing (by design); a scan between setCan and UseRemote made this
	// test flaky under CPU load. Dispatch is paused (with its barrier) before
	// Colab says yes and resumed after UseRemote, so the pin is what runs.
	resume := re.s.PauseDispatch()
	re.fr.setCan(true, "")
	err = re.s.UseRemote(re.ctx, l2.ID)
	resume()
	if err != nil {
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
	re.waitReason(l1, model.WaitRemoteBusy) // pinned, Colab busy with l2 (colabfix: says Colab)
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

// ---- colabfix (accept2) ----

// onlyColab is a laptop on AC whose one local device is switched off: only
// Colab transcribes (the accept2 setup; the local reason is power_no_device).
func onlyColab(t *testing.T) (*remoteEnv, model.ASRDevice) {
	t.Helper()
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	re.power.set(onACLaptop)
	return re, cuda
}

// One lecture runs on Colab, the next waits for it; the first one ends and
// the scan that sends the second to Colab must not tell the backend the
// queue is drained: the backend would release the runtime before the second
// lecture's Transcribe registered, and that lecture would find the runtime
// stopping (accept2: colab.busy, handed back for good, then stuck with
// power_no_device). The fake backend keeps the real one's books (strict);
// QueueDrained comes once the last lecture has ended.
func TestRemoteHandoverToTheNextLectureKeepsTheRuntime(t *testing.T) {
	re, cuda := onlyColab(t)
	re.strictBackend()
	l1 := re.lectureOn(autoOn(cuda))
	re.prepDone(l1)
	re.start()
	re.waitRunningOn(l1, model.DeviceColab)
	re.waitFor("l1 registered with the backend", 5*time.Second, func() bool { return re.fr.registered() == l1.ID })
	l2 := re.lectureOn(autoOn(cuda))
	re.prepDone(l2)
	// colabfix item 2: it waits for Colab, not for a local device.
	re.stillQueued(l2, model.WaitRemoteBusy)
	re.waitFor("planned queue of one lecture", 5*time.Second, func() bool {
		q, ok := re.fr.lastQueue()
		return ok && len(q) == 1
	})

	re.asr.release(l1.ID)
	re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	re.waitFor("l2 registered with the backend", 5*time.Second, func() bool { return re.fr.registered() == l2.ID })
	if d, _, _, _ := re.fr.counts(); d != 0 {
		t.Fatalf("QueueDrained called %d times while l2 was on its way to Colab", d)
	}
	re.asr.release(l2.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	// Nothing left: the runtime is released once, after the last lecture.
	re.waitFor("runtime released after the last lecture", 5*time.Second, func() bool {
		_, released := re.fr.strictCounts()
		return released == 1
	})
	if while, _ := re.fr.strictCounts(); while != 0 {
		t.Fatalf("QueueDrained called %d times while a run on colab:remote existed", while)
	}
	for _, l := range []model.Lecture{l1, l2} {
		if devs := re.asr.startsOf(l.ID); len(devs) != 1 || devs[0] != model.DeviceColab {
			t.Fatalf("%s ran on %v, want Colab once", l.ID, devs)
		}
		if st := re.lj(l.ID, model.StageASR).Stats; st.RemoteFallback != nil || st.RemoteRequeues != 0 {
			t.Fatalf("stats of %s = %+v", l.ID, st)
		}
	}
}

// Colab answers busy (the runtime was being released or started, e.g. an
// idle release racing a dispatch): the lecture goes back to the queue
// without a hand-back — no remote_fallback, the "改用 Colab 处理" pin
// stays, no retry used up — and Colab takes it again after a short pause.
func TestRemoteBusyIsNoHandBack(t *testing.T) {
	re, cuda := onlyColab(t)
	l1 := re.lectureOn(autoOn(cuda))
	re.prepDone(l1)
	re.asr.release(l1.ID)
	re.failRemoteNext(colab.NewBusyError())
	re.start()
	j := re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	if devs := re.asr.startsOf(l1.ID); len(devs) != 2 || devs[0] != model.DeviceColab || devs[1] != model.DeviceColab {
		t.Fatalf("l1 ran on %v, want Colab twice", devs)
	}
	if j.Stats.RemoteFallback != nil || j.Stats.RemoteRequeues != 1 || model.CountedAttempts(j) != 0 {
		t.Fatalf("l1 after the busy answer: attempt %d stats %+v", j.Attempt, j.Stats)
	}

	// Pinned with "改用 Colab 处理": the pin survives the busy answer.
	re.fr.setCan(false, "") // nothing goes to Colab on its own meanwhile
	l2 := re.lectureOn(autoOn(cuda))
	re.prepDone(l2)
	re.waitReason(l2, model.WaitRemoteBusy)
	if err := re.s.UseRemote(re.ctx, l2.ID); err != nil {
		t.Fatal(err)
	}
	re.asr.release(l2.ID)
	re.failRemoteNext(colab.NewBusyError())
	re.fr.setCan(true, "")
	j = re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if devs := re.asr.startsOf(l2.ID); len(devs) != 2 || devs[1] != model.DeviceColab {
		t.Fatalf("l2 ran on %v, want Colab twice", devs)
	}
	if !j.Stats.RemotePin || j.Stats.RemoteFallback != nil || j.Stats.RemoteRequeues != 1 {
		t.Fatalf("l2 after the busy answer: %+v", j.Stats)
	}
}

// Only Colab transcribes (no local device can take a lecture): a lecture
// waiting for the busy Colab says remote_busy, on a laptop on AC
// (power_no_device locally) and on a desktop (no_asr_device). One Colab
// hands back waits with remote_fallback — it is never sent to Colab on its
// own again — until "改用 Colab 处理".
func TestRemoteOnlyColabWaitingFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		power devices.Power
	}{{"laptop on AC", onACLaptop}, {"desktop", onAC}} {
		t.Run(tc.name, func(t *testing.T) {
			re, cuda := onlyColab(t)
			re.power.set(tc.power)
			l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
			re.prepDone(l1, l2)
			re.start()
			re.waitRunningOn(l1, model.DeviceColab)
			re.stillQueued(l2, model.WaitRemoteBusy)

			re.failRemoteNext(colab.NewFallbackError(colab.CodeSessionReclaimed, nil))
			re.asr.release(l1.ID)
			re.waitRunningOn(l2, model.DeviceColab)
			re.stillQueued(l1, model.WaitRemoteFallback)
			if st := re.lj(l1.ID, model.StageASR).Stats; st.RemoteFallback == nil || st.RemoteFallback.Code != colab.CodeSessionReclaimed {
				t.Fatalf("l1 after the hand-back: %+v", st)
			}
			if err := re.s.UseRemote(re.ctx, l1.ID); err != nil {
				t.Fatal(err)
			}
			re.waitReason(l1, model.WaitRemoteBusy) // pinned now; Colab busy with l2
			re.asr.release(l2.ID)
			re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
			if devs := re.asr.startsOf(l1.ID); len(devs) != 2 || devs[1] != model.DeviceColab {
				t.Fatalf("l1 ran on %v", devs)
			}
		})
	}
}

// With a local device that is only busy the reason stays the local one
// (whichever frees first takes the lecture); a handed-back lecture waits
// for that device, not with remote_fallback.
func TestRemoteBusyReasonKeepsLocalWait(t *testing.T) {
	for _, local := range []string{model.WaitGPUSlot, model.WaitMemory, model.WaitPowerQueue(model.DeviceCUDA0), model.WaitGPUPaused} {
		if got := remoteBusyReason(local); got != local {
			t.Errorf("remoteBusyReason(%q) = %q", local, got)
		}
	}
	for _, local := range []string{model.WaitNoASRDevice, model.WaitPowerNoDevice, model.WaitPowerNoSaverDevice, model.WaitDevicePaused(model.DeviceCUDA0)} {
		if got := remoteBusyReason(local); got != model.WaitRemoteBusy {
			t.Errorf("remoteBusyReason(%q) = %q", local, got)
		}
		if got := remoteHeldReason(local); got != model.WaitRemoteHeld { // colabfix2
			t.Errorf("remoteHeldReason(%q) = %q", local, got)
		}
	}
	for _, local := range []string{model.WaitGPUSlot, model.WaitMemory, model.WaitPowerQueue(model.DeviceCUDA0)} {
		if got := remoteHeldReason(local); got != local {
			t.Errorf("remoteHeldReason(%q) = %q", local, got)
		}
	}
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	re := newRemoteEnv(t, []model.ASRDevice{cuda}, colabOn)
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.failRemoteNext(colab.NewFallbackError(colab.CodeSessionReclaimed, nil))
	re.start()
	re.waitRunningOn(l1, model.DeviceCUDA0)
	re.waitRunningOn(l2, model.DeviceColab)
	re.asr.release(l2.ID)
	re.stillQueued(l2, model.WaitGPUSlot)
	re.asr.release(l1.ID)
	re.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// colabfix2: only Colab transcribes and "处理完当前这节后关机" holds the
// automatic dispatch: a waiting lecture no local device can take says
// remote_held (also one fixed to colab:remote, which runs nowhere else),
// and the hold stays while it waits. "改用 Colab 处理" ends the hold (the
// user asked for Colab explicitly): the lecture runs on Colab and finishes.
func TestRemoteHeldWithoutLocalDevice(t *testing.T) {
	re, cuda := onlyColab(t)
	l1, l2 := re.lectureOn(autoOn(cuda)), re.lectureOn(autoOn(cuda))
	re.prepDone(l1, l2)
	re.start()
	re.waitRunningOn(l1, model.DeviceColab)
	re.stillQueued(l2, model.WaitRemoteBusy)
	if err := re.s.ShutdownRemote(re.ctx, false); err != nil {
		t.Fatal(err)
	}
	re.stillQueued(l2, model.WaitRemoteHeld)
	re.asr.release(l1.ID) // the current lecture finishes; l2 stays held
	re.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	re.stillQueued(l2, model.WaitRemoteHeld)
	if !re.s.RemoteHeld() {
		t.Fatal("the hold ended while a lecture waits for Colab")
	}
	l3 := re.lectureOn(fixedOn(RemoteRow(re.fr.Settings())))
	re.prepDone(l3)
	re.stillQueued(l3, model.WaitRemoteHeld)

	re.asr.release(l2.ID)
	re.asr.release(l3.ID)
	if err := re.s.UseRemote(re.ctx, l2.ID); err != nil {
		t.Fatal(err)
	}
	for _, l := range []model.Lecture{l2, l3} {
		re.waitStatus(l.ID, model.StageASR, model.StatusDone)
		if !re.asr.ran(l.ID, model.DeviceColab) {
			t.Fatalf("%s ran on %v", l.ID, re.asr.startsOf(l.ID))
		}
	}
	if re.s.RemoteHeld() {
		t.Fatal("still held after 改用 Colab 处理")
	}
}
