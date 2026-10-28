// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// colab-batch (user decisions 2026-10-27 04:30 / 04:40, lead ruling 05:40):
// the plan the scheduler gives a RemoteBatcher backend, "1 + 1" with local
// devices (the second lecture kept for Colab; a local device gets the
// next one first), every waiting lecture planned without a local device,
// lectures whose audio is on Colab already going first and only there,
// two at once on the larger GPUs, and the runtime kept for lectures still
// being prepared or downloaded (with a limit).

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// fakeBatch is a fakeRemote that is a RemoteBatcher.
type fakeBatch struct {
	*fakeRemote
	bmu   sync.Mutex
	plans [][]colab.PlanEntry
	held  map[string]bool
	slots int
}

func (f *fakeBatch) SetPlan(p []colab.PlanEntry) {
	f.bmu.Lock()
	f.plans = append(f.plans, slices.Clone(p))
	f.bmu.Unlock()
}

func (f *fakeBatch) Held() map[string]bool {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	out := map[string]bool{}
	for k, v := range f.held {
		out[k] = v
	}
	return out
}

func (f *fakeBatch) Slots() int {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	return max(1, f.slots)
}

func (f *fakeBatch) setHeld(ids ...string) {
	f.bmu.Lock()
	f.held = map[string]bool{}
	for _, id := range ids {
		f.held[id] = true
	}
	f.bmu.Unlock()
}

func (f *fakeBatch) lastPlan() []colab.PlanEntry {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	if len(f.plans) == 0 {
		return nil
	}
	return f.plans[len(f.plans)-1]
}

type batchEnv struct {
	*remoteEnv
	fb *fakeBatch
}

func newBatchEnv(t *testing.T, rows []model.ASRDevice, prefs RemotePrefs, opt func(*Options), stages ...Stage) *batchEnv {
	t.Helper()
	be := &batchEnv{}
	re := &remoteEnv{fr: newFakeRemote()}
	be.fb = &fakeBatch{fakeRemote: re.fr, slots: 1}
	var de *devEnv
	de = newDevEnv(t, rows, func(o *Options) {
		o.Remote = be.fb
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
		if opt != nil {
			opt(o)
		}
	}, stages...)
	re.devEnv = de
	if err := WriteRemotePrefs(de.ctx, de.st, prefs); err != nil {
		t.Fatal(err)
	}
	de.asr.hook = re.hook
	be.remoteEnv = re
	return be
}

func (be *batchEnv) entry(l model.Lecture) (colab.PlanEntry, bool) {
	id := be.lj(l.ID, model.StageASR).ID
	for _, e := range be.fb.lastPlan() {
		if e.Key == id {
			return e, true
		}
	}
	return colab.PlanEntry{}, false
}

func (be *batchEnv) waitEntry(l model.Lecture, running, upload bool) {
	be.t.Helper()
	be.waitFor("plan entry of "+l.ID, 5*time.Second, func() bool {
		e, ok := be.entry(l)
		return ok && e.Running == running && e.Upload == upload
	})
}

// With a local device Colab holds one lecture running and one kept (the
// "+1", uploaded ahead); the third waits for whichever frees first, and a
// local device that frees takes it, never the kept one.
func TestBatchOffloadOnePlusOne(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, nil)
	l1, l2, l3, l4 := be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))
	be.prepDone(l1, l2, l3, l4)
	be.start()
	be.waitRunningOn(l1, model.DeviceCUDA0)
	be.waitRunningOn(l2, model.DeviceColab)
	be.waitEntry(l2, true, false)
	be.waitEntry(l3, false, true) // kept for Colab: may go up ahead
	be.waitEntry(l4, false, false)
	if p := be.fb.lastPlan(); p[0].Key != be.lj(l2.ID, model.StageASR).ID || p[1].Key != be.lj(l3.ID, model.StageASR).ID {
		t.Fatalf("plan order %+v", p)
	}
	if e, _ := be.entry(l3); e.AudioPath == "" || e.AudioSec != 0 && e.AudioSec < 0 {
		t.Fatalf("entry %+v", e)
	}
	// The local device frees: l4 goes there ("续杯时本机优先"), l3 stays for Colab.
	be.asr.release(l1.ID)
	be.waitRunningOn(l4, model.DeviceCUDA0)
	be.stillQueued(l3, model.WaitRemoteBusy)
	// Colab frees: l3 runs there.
	be.asr.release(l2.ID)
	be.waitRunningOn(l3, model.DeviceColab)
	be.asr.release(l3.ID)
	be.asr.release(l4.ID)
	for _, l := range []model.Lecture{l1, l2, l3, l4} {
		be.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	be.waitFor("drained", 5*time.Second, func() bool { d, _, _, _ := be.fr.counts(); return d >= 1 })
	be.waitFor("empty plan", 5*time.Second, func() bool { return len(be.fb.lastPlan()) == 0 })
}

// Without a local device every waiting lecture may go up ahead.
func TestBatchColabOnlyPlansEveryWaitingLecture(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, nil)
	be.power.set(onACLaptop)
	ls := []model.Lecture{be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))}
	be.prepDone(ls...)
	be.start()
	be.waitRunningOn(ls[0], model.DeviceColab)
	for _, l := range ls[1:] {
		be.waitEntry(l, false, true)
		be.stillQueued(l, model.WaitRemoteBusy)
	}
	for _, l := range ls {
		be.asr.release(l.ID)
	}
	for _, l := range ls {
		be.waitStatus(l.ID, model.StageASR, model.StatusDone)
		if !be.asr.ran(l.ID, model.DeviceColab) {
			t.Fatalf("%s ran on %v", l.ID, be.asr.startsOf(l.ID))
		}
	}
}

// A lecture whose audio is on Colab already goes first, and only there;
// two at once when Colab says two slots.
func TestBatchHeldLecturesGoFirstTwoAtOnce(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, nil)
	be.power.set(onACLaptop)
	be.fb.slots = 2
	l1, l2, l3 := be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))
	be.prepDone(l1, l2, l3)
	be.fb.setHeld(be.lj(l3.ID, model.StageASR).ID)
	be.start()
	be.waitRunningOn(l3, model.DeviceColab)
	be.waitRunningOn(l1, model.DeviceColab)
	be.stillQueued(l2, model.WaitRemoteBusy)
	if st := be.status(model.DeviceColab); st.SlotsUsed != 2 {
		t.Errorf("colab:remote %+v", st)
	}
	for _, l := range []model.Lecture{l1, l2, l3} {
		be.asr.release(l.ID)
	}
	for _, l := range []model.Lecture{l1, l2, l3} {
		be.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
}

// A held lecture does not go to a free local device.
func TestBatchHeldLectureStaysForColab(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, nil)
	l1, l2, l3 := be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))
	be.prepDone(l1, l2, l3)
	be.fb.setHeld(be.lj(l2.ID, model.StageASR).ID)
	be.fr.setCan(false, "") // Colab busy
	be.start()
	be.waitRunningOn(l1, model.DeviceCUDA0)
	be.stillQueued(l2, model.WaitRemoteBusy)
	be.asr.release(l1.ID)
	be.waitRunningOn(l3, model.DeviceCUDA0) // the local device takes l3, not the held l2
	be.stillQueued(l2, model.WaitRemoteBusy)
	be.fr.setCan(true, "")
	be.waitRunningOn(l2, model.DeviceColab)
	be.asr.release(l2.ID)
	be.asr.release(l3.ID)
	be.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	be.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

// A shutdown or a hand-back ends the reservation: the local device may
// take the lecture.
func TestBatchReservationEndsWithAHandBack(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, nil)
	l1, l2, l3 := be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))
	be.prepDone(l1, l2, l3)
	be.start()
	be.waitRunningOn(l1, model.DeviceCUDA0)
	be.waitRunningOn(l2, model.DeviceColab)
	be.waitEntry(l3, false, true)
	be.failRemoteNext(colab.NewFallbackError(colab.CodeRemoteFailed, map[string]any{"reason": "no_gpu"}))
	be.asr.release(l2.ID)
	be.asr.release(l1.ID)
	// l2 goes back to the local devices; l3 is no longer kept for Colab.
	be.asr.release(l3.ID)
	for _, l := range []model.Lecture{l1, l2, l3} {
		be.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	if st := be.asr.startsOf(l2.ID); len(st) != 2 || st[1] != model.DeviceCUDA0 {
		t.Errorf("l2 ran on %v (Colab, then the local device)", st)
	}
	be.s.mu.Lock()
	n := len(be.s.rem.reserved)
	be.s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d reservations left", n)
	}
}

// The runtime stays while a lecture that would go to Colab is still being
// prepared, at most RemoteImminentMax after the queue emptied.
func TestBatchImminentPrepKeepsTheRuntime(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	asr := newDevStage()
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, func(o *Options) { o.RemoteImminentMax = 400 * time.Millisecond },
		asr.stage()) // no prep stage: a prep job stays queued
	be.asr = asr
	be.asr.hook = be.remoteEnv.hook
	be.power.set(onACLaptop)
	l1 := be.lectureOn(autoOn(cuda))
	be.prepDone(l1)
	l2 := be.lectureOn(autoOn(cuda)) // its prep waits
	be.start()
	be.waitRunningOn(l1, model.DeviceColab)
	be.asr.release(l1.ID)
	be.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	time.Sleep(150 * time.Millisecond)
	if d, _, _, _ := be.fr.counts(); d != 0 {
		t.Fatalf("drained with l2 in prep (%d)", d)
	}
	// The limit: released after RemoteImminentMax all the same.
	be.waitFor("drained after the limit", 5*time.Second, func() bool { d, _, _, _ := be.fr.counts(); return d >= 1 })
	_ = l2
}

// A download about to become a lecture keeps the runtime too.
func TestBatchImminentDownloadKeepsTheRuntime(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	var mu sync.Mutex
	eta := 30 * time.Second
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, func(o *Options) {
		o.RemoteSoon = func(context.Context) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return eta
		}
	})
	be.power.set(onACLaptop)
	l1 := be.lectureOn(autoOn(cuda))
	be.prepDone(l1)
	be.start()
	be.waitRunningOn(l1, model.DeviceColab)
	be.asr.release(l1.ID)
	be.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	time.Sleep(150 * time.Millisecond)
	if d, _, _, _ := be.fr.counts(); d != 0 {
		t.Fatalf("drained with a download 30 s away (%d)", d)
	}
	mu.Lock()
	eta = 20 * time.Minute // beyond the limit
	mu.Unlock()
	be.waitFor("drained", 5*time.Second, func() bool { d, _, _, _ := be.fr.counts(); return d >= 1 })
}

// With local devices free nothing keeps the runtime for work to come.
func TestBatchImminentNotForLocalDevices(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, func(o *Options) {
		o.RemoteSoon = func(context.Context) time.Duration { return 10 * time.Second }
	})
	l1, l2 := be.lectureOn(autoOn(cuda)), be.lectureOn(autoOn(cuda))
	be.prepDone(l1, l2)
	be.start()
	be.waitRunningOn(l1, model.DeviceCUDA0)
	be.waitRunningOn(l2, model.DeviceColab)
	be.asr.release(l1.ID)
	be.asr.release(l2.ID)
	be.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	be.waitFor("drained", 5*time.Second, func() bool { d, _, _, _ := be.fr.counts(); return d >= 1 })
}

// "改用 Colab 处理" on a "修复断句" job (textrepair-int's asr_mode): the
// lecture is transcribed on Colab from the start, the mode goes (lead
// ruling 2026-10-27 05:50 item 3).
func TestUseRemoteClearsTheTextRepairMode(t *testing.T) {
	re, cuda := onlyColab(t)
	l := re.lectureOn(autoOn(cuda))
	re.prepDone(l)
	j := re.lj(l.ID, model.StageASR)
	if err := state.TransitionJob(re.ctx, re.st, j.ID, model.StatusPending, model.StatusQueued, db.JobFinish{}); err != nil {
		t.Fatal(err)
	}
	st := j.Stats
	st.ASRMode = model.ASRModeTextRepair
	if err := re.st.SetJobStats(re.ctx, j.ID, st); err != nil {
		t.Fatal(err)
	}
	if err := re.s.UseRemote(re.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	nj := re.lj(l.ID, model.StageASR)
	if nj.Attempt <= j.Attempt || !nj.Stats.RemotePin || nj.Stats.ASRMode != "" {
		t.Fatalf("after 改用 Colab 处理: %+v", nj.Stats)
	}
}
