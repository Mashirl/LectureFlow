// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// A slot that frees while a scan is under way (flake-hunt, product race A):
// a job further down the scan's FIFO order must not take it from a job the
// same scan refused for want of that slot. The tests end a run at a fixed
// point in the middle of a scan (resourceHook), so they do not depend on
// timing.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// resourceHook is a stage whose Resource runs a hook first, once armed: the
// scan calls Resource when it evaluates a queued job of the stage
// (tryStart; for an auxiliary CPU stage also once at the start of the scan,
// cpuAuxLast), so the hook runs in the middle of a scan, between two jobs.
type resourceHook struct {
	Stage
	mu    sync.Mutex
	calls int    // Resource calls since armed
	at    int    // the call that fires (0: disarmed)
	fire  func() // runs on the scan's goroutine
}

func (h *resourceHook) Resource() ResourceKind {
	h.mu.Lock()
	var f func()
	if h.at > 0 {
		h.calls++
		if h.calls == h.at {
			f, h.at = h.fire, 0
		}
	}
	h.mu.Unlock()
	if f != nil {
		f()
	}
	return h.Stage.Resource()
}

// arm makes the n-th Resource call from the next scan on run fire first.
// No scan runs while it is armed (PauseDispatch returns once a scan in
// progress has ended), so the count starts with a whole scan.
func (h *resourceHook) arm(s *Scheduler, n int, fire func()) {
	resume := s.PauseDispatch()
	h.mu.Lock()
	h.calls, h.at, h.fire = 0, n, fire
	h.mu.Unlock()
	resume()
}

func (h *resourceHook) fired() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.at == 0
}

// endRunMidScan is a hook that lets a run end (release) and waits until its
// slot is free, still inside the scan. It runs on the scan's goroutine, so
// it reports a timeout with Error (not Fatal).
func endRunMidScan(t *testing.T, s *Scheduler, res ResourceKind, release func()) func() {
	return func() {
		running := s.countRunning(res)
		release()
		deadline := time.Now().Add(10 * time.Second)
		for s.countRunning(res) >= running {
			if time.Now().After(deadline) {
				t.Error("the released run did not end")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// CPU pool: H's prep holds the only slot; B's prep (a pipeline stage) and
// A's transcode (an auxiliary task, last in the scan) wait. In a scan, B's
// prep is refused, then H's prep ends before the scan gets to A's
// transcode: the slot goes to B's prep in the next scan, not to A's
// transcode (before the fix it did; by chance, this made
// TestCPUPoolPipelineBeforeAux fail 1 run in 7 at GOMAXPROCS=1).
func TestCPUSlotFreedMidScanKeepsFIFO(t *testing.T) {
	order := &cpuOrder{}
	hold := make(chan struct{})
	var holder string
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, _ func(float64)) error {
		order.add("prep:" + id)
		if id != holder {
			return nil
		}
		select {
		case <-hold:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	stream := &resourceHook{Stage: &fakeStage{name: model.StageStream, res: ResourceCPU, run: func(ctx context.Context, id string, _ func(float64)) error {
		order.add("stream:" + id)
		return nil
	}}}
	e := newEnvOpts(t, nil, func(o *Options) { o.CPUSlots = 1 }, prep, stream)
	a := e.lecture()
	e.prepDone(a)
	if _, err := state.EnqueueAux(e.ctx, e.st, model.ScopeLecture, a.ID, model.StageStream); err != nil {
		t.Fatal(err)
	}
	h, b := e.lecture(), e.lecture()
	holder = h.ID
	e.start()
	e.waitStatus(h.ID, model.StagePrep, model.StatusRunning)
	e.waitFor("B's prep and A's transcode waiting for the slot", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(b.ID, model.StagePrep)) == model.WaitCPUSlot &&
			e.s.WaitingFor(e.lj(a.ID, model.StageStream)) == model.WaitCPUSlot
	})
	// The scan calls the transcode stage's Resource once at its start
	// (cpuAuxLast) and once for A's transcode, after B's prep.
	stream.arm(e.s, 2, endRunMidScan(t, e.s, ResourceCPU, func() { close(hold) }))
	e.waitFor("the hook", 5*time.Second, stream.fired)
	e.waitStatus(b.ID, model.StagePrep, model.StatusDone)
	e.waitStatus(a.ID, model.StageStream, model.StatusDone)
	want := []string{"prep:" + h.ID, "prep:" + b.ID, "stream:" + a.ID}
	if got := order.list(); !slices.Equal(got, want) {
		t.Fatalf("CPU starts %v, want %v (the slot freed in the scan went past the prep it refused)", got, want)
	}
}

// The single GPU pool (no device registry): l0 runs, l1 and l2 wait; l0
// ends in a scan after l1 was refused: l1 starts next, then l2.
func TestGPUSlotFreedMidScanKeepsFIFO(t *testing.T) {
	asr := newDevStage()
	hook := &resourceHook{Stage: asr.stage()}
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), hook)
	ls := []model.Lecture{e.lecture(), e.lecture(), e.lecture()}
	e.prepDone(ls...)
	e.start()
	e.waitStatus(ls[0].ID, model.StageASR, model.StatusRunning)
	e.waitFor("l1 and l2 waiting for the slot", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(ls[1].ID, model.StageASR)) == model.WaitGPUSlot &&
			e.s.WaitingFor(e.lj(ls[2].ID, model.StageASR)) == model.WaitGPUSlot
	})
	// One Resource call per queued asr job: l1's, then l2's.
	hook.arm(e.s, 2, endRunMidScan(t, e.s, ResourceGPU, func() { asr.release(ls[0].ID) }))
	e.waitFor("the hook", 5*time.Second, hook.fired)
	nextStartsInOrder(e, asr, ls)
}

// Per-device pools: the same on a machine with one device.
func TestDeviceSlotFreedMidScanKeepsFIFO(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	asr := newDevStage()
	hook := &resourceHook{Stage: asr.stage()}
	de := newDevEnv(t, []model.ASRDevice{cuda}, nil, instant(model.StagePrep, ResourceCPU), hook)
	de.asr = asr
	ls := []model.Lecture{de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))}
	de.prepDone(ls...)
	de.start()
	de.waitRunningOn(ls[0], model.DeviceCUDA0)
	de.waitReason(ls[1], model.WaitGPUSlot)
	de.waitReason(ls[2], model.WaitGPUSlot)
	hook.arm(de.s, 2, endRunMidScan(t, de.s, ResourceGPU, func() { asr.release(ls[0].ID) }))
	de.waitFor("the hook", 5*time.Second, hook.fired)
	nextStartsInOrder(de.env, asr, ls)
}

// multi_device off: one asr job at a time whatever the devices. l0 runs on
// the CUDA card, l1 and l2 wait for it; it ends in a scan after l1 was
// refused: l2 does not start on the free card (or the other device) ahead
// of l1.
func TestMultiDeviceOffTurnFreedMidScanKeepsFIFO(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	asr := newDevStage()
	hook := &resourceHook{Stage: asr.stage()}
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil, instant(model.StagePrep, ResourceCPU), hook)
	de.asr = asr
	de.setASR(func(a *model.ASRSettings) { a.MultiDevice = false })
	ls := []model.Lecture{de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))}
	de.prepDone(ls...)
	de.start()
	de.waitRunningOn(ls[0], model.DeviceCUDA0)
	de.waitReason(ls[1], model.WaitGPUSlot)
	de.waitReason(ls[2], model.WaitGPUSlot)
	hook.arm(de.s, 2, endRunMidScan(t, de.s, ResourceGPU, func() { asr.release(ls[0].ID) }))
	de.waitFor("the hook", 5*time.Second, hook.fired)
	nextStartsInOrder(de.env, asr, ls)
}

// nextStartsInOrder checks that the run after l0's is l1's (not l2's,
// which the scan reached after the slot freed), then lets the rest run: the
// lectures' asr jobs start once each, in FIFO order.
func nextStartsInOrder(e *env, asr *devStage, ls []model.Lecture) {
	e.t.Helper()
	e.waitFor("the next asr start", 5*time.Second, func() bool { return asr.startCount() >= 2 })
	if got := startOrder(asr, ls); !slices.Equal(got, []string{"l0", "l1"}) {
		e.t.Fatalf("asr starts %v, want l0 then l1 (the slot freed in the scan went past the job it refused)", got)
	}
	asr.release(ls[1].ID)
	asr.release(ls[2].ID)
	e.waitStatus(ls[2].ID, model.StageASR, model.StatusDone)
	if got := startOrder(asr, ls); !slices.Equal(got, []string{"l0", "l1", "l2"}) {
		e.t.Fatalf("asr starts %v, want l0 l1 l2", got)
	}
}

// startOrder lists the lectures (l0, l1, ... by their index in ls) of the
// asr runs in start order.
func startOrder(asr *devStage, ls []model.Lecture) []string {
	asr.mu.Lock()
	starts := slices.Clone(asr.starts)
	asr.mu.Unlock()
	var out []string
	for _, s := range starts {
		id, _, _ := strings.Cut(s, "@")
		for i, l := range ls {
			if l.ID == id {
				out = append(out, fmt.Sprintf("l%d", i))
			}
		}
	}
	return out
}

// SSH servers (ssh-mvp): the same for a server's slots. l0 runs on the
// server (the prefer group), l1 on the CUDA card; l2 and l3 wait. The
// server's run ends in a scan after l2 was refused: l3 does not take it.
func TestSSHSlotFreedMidScanKeepsFIFO(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	mac := sshRow("mac", 400)
	asr := newDevStage()
	hook := &resourceHook{Stage: asr.stage()}
	de := newDevEnv(t, []model.ASRDevice{cuda, mac}, nil, instant(model.StagePrep, ResourceCPU), hook)
	de.asr = asr
	de.s.SetSSH(&fakeFleet{devs: map[string]*fakeSSHDevice{mac.ID: {id: mac.ID, ready: true}}})
	ls := []model.Lecture{de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))}
	de.prepDone(ls...)
	de.start()
	de.waitRunningOn(ls[0], mac.ID)
	de.waitRunningOn(ls[1], model.DeviceCUDA0)
	de.waitReason(ls[2], model.WaitGPUSlot)
	de.waitReason(ls[3], model.WaitGPUSlot)
	hook.arm(de.s, 2, endRunMidScan(t, de.s, ResourceGPU, func() { asr.release(ls[0].ID) }))
	de.waitFor("the hook", 5*time.Second, hook.fired)
	de.waitFor("the next asr start", 5*time.Second, func() bool { return asr.startCount() >= 3 })
	// l0 and l1 started in the same scan: their runs record themselves in
	// either order. The third run is the point.
	if got := startOrder(asr, ls); len(got) != 3 || !slices.Contains(got[:2], "l0") || !slices.Contains(got[:2], "l1") || got[2] != "l2" {
		t.Fatalf("asr starts %v, want l0 and l1, then l2 (the server's slot freed in the scan went past the job it refused)", got)
	}
	de.waitRunningOn(ls[2], mac.ID)
	for _, l := range ls {
		asr.release(l.ID)
	}
	for _, l := range ls {
		de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
}

// The power policy's queue (on battery one device is active: power_queue
// for it): l0 runs on the NPU, l1 and l2 queue for it; the NPU's run ends
// in a scan after l1 was refused: l2 does not take the NPU ahead of l1.
func TestPowerQueueFreedMidScanKeepsFIFO(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	asr := newDevStage()
	hook := &resourceHook{Stage: asr.stage()}
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, nil, instant(model.StagePrep, ResourceCPU), hook)
	de.asr = asr
	de.power.set(onBattery)
	ls := []model.Lecture{de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))}
	de.prepDone(ls...)
	de.start()
	de.waitRunningOn(ls[0], model.DeviceOVNPU)
	de.waitReason(ls[1], model.WaitPowerQueue(model.DeviceOVNPU))
	de.waitReason(ls[2], model.WaitPowerQueue(model.DeviceOVNPU))
	hook.arm(de.s, 2, endRunMidScan(t, de.s, ResourceGPU, func() { asr.release(ls[0].ID) }))
	de.waitFor("the hook", 5*time.Second, hook.fired)
	nextStartsInOrder(de.env, asr, ls)
	for _, l := range ls {
		if devs := asr.startsOf(l.ID); len(devs) != 1 || devs[0] != model.DeviceOVNPU {
			t.Fatalf("lecture %s ran on %v, want the NPU only (on battery)", l.ID, devs)
		}
	}
}
