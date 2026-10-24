// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Power management, backend Gate round 1 (lead rulings, 2026-10-24): the
// scheduler side of fixes 1–7 (NOTES "决定（电源管理后端 Gate 修复：
// core-builder，2026-10-24）").

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// noStart fails when the fake self-test started an attempt.
func noStart(t *testing.T, started <-chan model.ASRDevice, what string) {
	t.Helper()
	select {
	case d := <-started:
		t.Fatalf("%s: a self-test attempt of %s started", what, d.ID)
	default:
	}
}

// Fix 1 (and 5): on a machine with a battery a pipeline job admitted on one
// device makes the running self-test of another device yield at once (the
// 3b preemption): one active device, and the test never holds up the
// transcription. The test waits power_queue:{the active device} and starts
// over once that device is idle. A desktop keeps running both.
func TestPowerSelfTestYieldsToSingleActiveDevice(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(2)
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, func(o *Options) { o.DeviceTest = ft.run })
	de.power.set(onACLaptop)
	de.start()
	de.waitFor("AC seen", 5*time.Second, func() bool { return de.s.Power() == onACLaptop })
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{} // the first candidate finishes; the second one runs
	de.waitFor("second candidate", 5*time.Second, func() bool { st, _ := de.s.DeviceTest(model.DeviceOVNPU); return st.Step == 2 })

	t0 := time.Now()
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("the transcription waited %v for the self-test", d)
	}
	st := de.waitTest(model.DeviceOVNPU, DeviceTestWaiting)
	want := model.WaitPowerQueue(model.DeviceCUDA0)
	if st.WaitingFor != want || st.Error != nil {
		t.Fatalf("preempted test: %+v", st)
	}
	if s := de.status(model.DeviceOVNPU); s.SlotsUsed != 0 || s.SelfTest == nil || s.SelfTest.WaitingFor != want {
		t.Fatalf("NPU status while CUDA transcribes: %+v (%+v)", s, s.SelfTest)
	}
	if bs := de.devs.benchmarks(model.DeviceOVNPU); len(bs) != 0 {
		t.Fatalf("the preempted attempt was stored: %+v", bs)
	}
	// It stays out while CUDA transcribes.
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting || st.WaitingFor != want {
		t.Fatalf("while CUDA transcribes: %+v", st)
	}
	noStart(t, ft.started, "while CUDA transcribes")

	// CUDA idle: the test starts over from the first candidate.
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	select {
	case <-ft.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the test did not start over")
	}
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestRunning || st.WaitingFor != "" {
		t.Fatalf("restarted test: %+v", st)
	}
	ft.step <- struct{}{}
	ft.step <- struct{}{}
	done := de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	if done.Result == nil || len(done.Result.Results) != 2 || done.WaitingFor != "" {
		t.Fatalf("done = %+v", done)
	}

	// A desktop: the test runs next to the transcription (3b).
	de.power.set(onAC)
	de.waitFor("desktop seen", 5*time.Second, func() bool { return de.s.Power() == onAC })
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceCUDA0)
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestRunning || st.WaitingFor != "" {
		t.Fatalf("desktop: the test stopped for a transcription on another device: %+v", st)
	}
	ft.step <- struct{}{}
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// Fix 5: the waiting reason of a self-test names the active device when the
// single active device holds it back (a pipeline job, or another device's
// self-test); a wait of the 3b rules (its own device busy) and every wait
// on a desktop have none. The API passes it through (SelfTestView).
func TestPowerSelfTestWaitingFor(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu}, func(o *Options) { o.DeviceTest = ft.run })
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)

	// Queued while CUDA transcribes.
	st, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != DeviceTestWaiting || st.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Fatalf("queued while CUDA transcribes: %+v", st)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting || st.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Fatalf("waiting for CUDA: %+v", st)
	}
	if s := de.status(model.DeviceOVNPU); s.SelfTest == nil || s.SelfTest.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Fatalf("device status: %+v", s.SelfTest)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.WaitingFor != "" {
		t.Fatalf("running: %+v", st)
	}

	// Another device's self-test holds its slot.
	st, err = de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != DeviceTestWaiting || st.WaitingFor != model.WaitPowerQueue(model.DeviceOVNPU) {
		t.Fatalf("queued while the NPU is tested: %+v", st)
	}
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)

	// Its own device busy (the 3b rule, on any machine): no power code.
	l2 := de.lectureOn(fixedOn(igpu))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceOVGPU)
	if st, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil || st.State != DeviceTestWaiting || st.WaitingFor != "" {
		t.Fatalf("waiting for its own device: %+v %v", st, err)
	}
	de.asr.release(l2.ID)
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)

	// A desktop: never a power code (here the test waits for its own
	// device's transcription).
	de.power.set(onAC)
	de.waitFor("desktop seen", 5*time.Second, func() bool { return de.s.Power() == onAC })
	l3 := de.lectureOn(fixedOn(npu))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceOVNPU)
	if st, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil || st.State != DeviceTestWaiting || st.WaitingFor != "" {
		t.Fatalf("desktop: %+v %v", st, err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.WaitingFor != "" {
		t.Fatalf("desktop: %+v", st)
	}
	de.asr.release(l3.ID)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
}

// memTest is a DeviceTestFunc with one candidate that reports the
// candidates sent on report (the memory budget prices the reported one)
// and finishes on finish.
type memTest struct {
	started chan model.ASRDevice
	report  chan devices.Candidate
	finish  chan struct{}
}

func newMemTest() *memTest {
	return &memTest{started: make(chan model.ASRDevice, 8), report: make(chan devices.Candidate), finish: make(chan struct{})}
}

func (m *memTest) run(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (model.DeviceBenchmark, error) {
	m.started <- dev
	b := model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone, Results: []model.BenchmarkResult{}}
	if err := env.BeforeCandidate(ctx); err != nil {
		b.Status = model.BenchmarkCancelled
		return b, nil
	}
	for {
		select {
		case c := <-m.report:
			env.Report(DeviceTestProgress{Phase: DeviceTestPhaseTranscribing, Step: 1, Steps: 1, Candidate: c, Progress: 0.5})
		case <-m.finish:
			b.Results = append(b.Results, model.BenchmarkResult{Model: "small", Precision: model.PrecisionINT8, OK: true, RTF: 0.1})
			return b, nil
		case <-ctx.Done():
			b.Status = model.BenchmarkCancelled
			return b, nil
		}
	}
}

// Fix 2: the worker memory budget counts a self-test holding a slot (the
// candidate it runs, else its device's largest candidate); a test takes or
// takes back its slot only within the budget and while no pipeline job
// waits for memory; a self-test whose memory alone keeps a pipeline job
// from starting is preempted (it never holds up the transcription). On a
// desktop, so the single active device plays no part.
func TestPowerWorkerMemoryBudgetCountsSelfTests(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	mt := newMemTest()
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu}, func(o *Options) { o.DeviceTest = mt.run })
	// cuda:0 4000 MB; the Intel GPU: small 1000, its other candidates
	// (large-v3-turbo) 5000; the NPU 500. Budget 8192.
	de.s.memEstimate = func(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int {
		switch {
		case d.ID == model.DeviceCUDA0:
			return 4000
		case d.ID == model.DeviceOVNPU:
			return 500
		case d.Model == "small":
			return 1000
		}
		return 5000
	}
	small := devices.Candidate{Model: "small", Precision: model.PrecisionINT8}
	de.start()

	// The test runs its small candidate (1000): a CUDA job fits beside it
	// (5000), the test goes on.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	<-mt.started
	mt.report <- small
	de.waitFor("candidate reported", 5*time.Second, func() bool {
		st, _ := de.s.DeviceTest(model.DeviceOVGPU)
		return st.Candidate != nil && st.Candidate.Model == "small"
	})
	l1 := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestRunning {
		t.Fatalf("a job that fits preempted the test: %+v", st)
	}
	mt.finish <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	de.asr.release(l1.ID)
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)

	// No candidate reported yet: the largest one counts (5000 + 4000 > 8192).
	// The test's memory alone keeps the job out → the test is preempted, the
	// job starts; the test takes its slot back only once the budget fits
	// (retakeSlot), i.e. when the job is done.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	<-mt.started
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	t0 := time.Now()
	l2 := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceCUDA0)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("the job waited %v for the self-test's memory", d)
	}
	if st := de.waitTest(model.DeviceOVGPU, DeviceTestWaiting); st.WaitingFor != "" {
		t.Fatalf("preempted test on a desktop: %+v", st)
	}
	time.Sleep(60 * time.Millisecond)
	noStart(t, mt.started, "over the budget (retake)")
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 {
		t.Fatalf("the preempted test kept its slot: %+v", s)
	}
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	select {
	case <-mt.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the test did not start over within the budget")
	}
	mt.finish <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)

	// A test does not start over the budget: a CUDA job runs (4000), the
	// Intel GPU's largest candidate (5000) does not fit.
	l3 := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceCUDA0)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting || !st.StartedAt.IsZero() {
		t.Fatalf("a test over the budget started: %+v", st)
	}
	noStart(t, mt.started, "over the budget (start)")
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
	<-mt.started
	mt.report <- small

	// A job kept out by the pipeline workers (and the test) preempts
	// nothing; while it waits for room in the budget no other test takes a
	// slot. Budget 7000, CUDA with two slots: A runs (4000 + the test's
	// 1000), B would need 9000 (8000 without the test).
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.MaxParallel = 2 })
	de.cfg.ASR.WorkerMemBudgetMB = 7000
	la := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.waitRunningOn(la, model.DeviceCUDA0)
	lb := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.stillQueued(lb, model.WaitMemory)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestRunning {
		t.Fatalf("a job the pipeline workers keep out preempted the test: %+v", st)
	}
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting {
		t.Fatalf("a test started while a job waits for the budget: %+v", st)
	}
	noStart(t, mt.started, "while a job waits for the budget")
	// A done: B starts (5000), then the NPU's test (5500).
	de.asr.release(la.ID)
	de.waitRunningOn(lb, model.DeviceCUDA0)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-mt.started
	for range 2 {
		mt.finish <- struct{}{}
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.asr.release(lb.ID)
	de.waitStatus(lb.ID, model.StageASR, model.StatusDone)

	// A job that can never fit the budget (4000 > 3000) keeps no test out
	// for good.
	de.cfg.ASR.WorkerMemBudgetMB = 3000
	lc := de.lectureOn(fixedOn(cuda))
	de.s.Kick()
	de.stillQueued(lc, model.WaitMemory)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-mt.started
	mt.finish <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.cfg.ASR.WorkerMemBudgetMB = 8192
	de.s.Kick()
	de.waitRunningOn(lc, model.DeviceCUDA0)
	de.asr.release(lc.ID)
	de.waitStatus(lc.ID, model.StageASR, model.StatusDone)
}

// Fix 6: power.resumed ("已接通电源") only when the suspended job starts
// on AC; started on battery (here: a job fixed to the Intel GPU, which
// becomes the battery device when the NPU is disabled) the suspended event
// is dropped and no notice is written.
func TestPowerResumedOnlyOnAC(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, nil)
	causes := withCauses(de)
	de.power.set(onACLaptop)
	l := de.lectureOn(fixedOn(igpu))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.power.set(onBattery)
	de.stillQueued(l, model.WaitPowerWaitAC)
	if cs := causes.of(l.ID); len(cs) != 1 || !errors.Is(cs[0], ErrPowerSuspend) {
		t.Fatalf("causes = %v", cs)
	}
	j := de.lj(l.ID, model.StageASR)
	if j.Stats.PowerEvent == nil || j.Stats.PowerEvent.Code != CodePowerSuspended {
		t.Fatalf("suspended: %+v", j.Stats)
	}
	// Still on battery: the Intel GPU becomes the battery device.
	de.devs.set(model.DeviceOVNPU, func(d *model.ASRDevice) { d.Enabled = false })
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.waitFor("event dropped", 5*time.Second, func() bool { return de.lj(l.ID, model.StageASR).Stats.PowerEvent == nil })
	if _, ok := de.notice(CodePowerResumed, j.ID); ok {
		t.Fatal("a power.resumed notice on battery")
	}
	if j := de.lj(l.ID, model.StageASR); j.Stats.PowerInterrupts != 1 || j.Stats.ASRResume == nil {
		t.Fatalf("running on battery: %+v", j.Stats)
	}
	de.asr.release(l.ID)
	done := de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if done.Stats.PowerEvent != nil {
		t.Fatalf("done: %+v", done.Stats.PowerEvent)
	}
	var codes []string
	for _, n := range de.s.PowerStatus().Notices {
		codes = append(codes, n.Info.Code)
	}
	if !slices.Equal(codes, []string{CodePowerSuspended}) {
		t.Fatalf("notices %v", codes)
	}
}

// Fix 7: a run on a CPU device (started while the machine counted as a
// desktop) is suspended as soon as the reading shows a machine with a
// battery, also on AC — without the "已拔掉电源" event and notice — and
// never goes back to the CPU; a CPU device cannot be tested there, in any
// power state: refused, waiting or running tests cancelled, all with
// selftest.power_cpu (lead, after Gate round 1).
func TestPowerCPURunSuspendedOnBatteryMachine(t *testing.T) {
	fwc, igpu := devRow(model.DeviceFWCPU, 50, 1), devRow(model.DeviceOVGPU, 200, 1)
	igpu.Enabled = false
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{fwc, igpu}, func(o *Options) { o.DeviceTest = ft.run })
	causes := withCauses(de)
	l := de.lectureOn(autoOn(fwc))
	de.prepDone(l)
	de.start() // power unknown: a desktop, the CPU fallback (Q1)
	de.waitRunningOn(l, model.DeviceFWCPU)
	before := de.lj(l.ID, model.StageASR)
	// A test of the CPU waits for the transcription on it (3b).
	if st, err := de.s.StartDeviceTest(de.ctx, model.DeviceFWCPU); err != nil || st.State != DeviceTestWaiting {
		t.Fatalf("desktop CPU test: %+v %v", st, err)
	}
	cpuRefused := func(what string, err error) {
		t.Helper()
		var ce *model.CodedError
		if !errors.Is(err, ErrSelfTestPowerCPU) || errors.Is(err, ErrSelfTestPowerBattery) || !errors.Is(err, state.ErrConflict) ||
			!errors.As(err, &ce) || ce.Info.Code != CodeSelfTestPowerCPU || ce.Info.Params["device"] != fwc.Label {
			t.Fatalf("%s: %v", what, err)
		}
	}
	cpuCancelled := func(what string) {
		t.Helper()
		st := de.waitTest(model.DeviceFWCPU, DeviceTestCancelled)
		if st.Error == nil || st.Error.Code != CodeSelfTestPowerCPU || st.Error.Params["device"] != fwc.Label {
			t.Fatalf("%s: %+v", what, st.Error)
		}
	}

	de.power.set(onACLaptop)
	de.stillQueued(l, model.WaitPowerNoDevice)
	if cs := causes.of(l.ID); len(cs) != 1 || !errors.Is(cs[0], ErrPowerSuspend) {
		t.Fatalf("causes = %v", cs)
	}
	j := de.lj(l.ID, model.StageASR)
	if j.ID != before.ID || j.Attempt != before.Attempt+1 || (j.Error != nil && *j.Error != "") || j.Stats.PowerInterrupts != 1 ||
		j.Stats.ASRResume == nil || j.Stats.ASRResume.DeviceID != model.DeviceFWCPU || j.Stats.PowerEvent != nil {
		t.Fatalf("suspended on AC: attempt %d error %v stats %+v", j.Attempt, j.Error, j.Stats)
	}
	if evs := de.eventsOf(j.ID); slices.Contains(evs, model.StatusFailed) {
		t.Fatalf("a failed status was emitted: %v", evs)
	}
	if ns := de.s.PowerStatus().Notices; len(ns) != 0 {
		t.Fatalf("notices on AC: %+v", ns)
	}
	// Self-test of the CPU: the waiting one is cancelled, a new one refused,
	// on AC too.
	cpuCancelled("waiting CPU test after the reading turned into a laptop on AC")
	_, err := de.s.StartDeviceTest(de.ctx, model.DeviceFWCPU)
	cpuRefused("CPU self-test on AC", err)
	// An accelerator appears: the job runs there, never on the CPU again.
	de.devs.set(model.DeviceOVGPU, func(d *model.ASRDevice) { d.Enabled = true })
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if got := de.asr.startsOf(l.ID); !slices.Equal(got, []string{model.DeviceFWCPU, model.DeviceOVGPU}) {
		t.Fatalf("starts = %v", got)
	}

	// A running CPU test (on a desktop) is cancelled when the reading shows
	// a laptop on battery; on battery a new one is refused with the same code.
	de.power.set(onAC)
	de.waitFor("desktop seen", 5*time.Second, func() bool { return de.s.Power() == onAC })
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceFWCPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceFWCPU, DeviceTestRunning)
	<-ft.started
	de.power.set(onBattery)
	cpuCancelled("running CPU test after the reading turned into a laptop on battery")
	_, err = de.s.StartDeviceTest(de.ctx, model.DeviceFWCPU)
	cpuRefused("CPU self-test on battery", err)
	// Any other device refused on battery keeps selftest.power_battery (here
	// CUDA; the Intel GPU is the battery device).
	de.devs.add(devRow(model.DeviceCUDA0, 300, 1))
	_, err = de.s.StartDeviceTest(de.ctx, model.DeviceCUDA0)
	var ce *model.CodedError
	if !errors.Is(err, ErrSelfTestPowerBattery) || errors.Is(err, ErrSelfTestPowerCPU) || !errors.As(err, &ce) || ce.Info.Code != CodeSelfTestPowerBattery {
		t.Fatalf("CUDA self-test on battery: %v", err)
	}
}

// Fixes 3 and 4 through the scheduler: "沿用原模型" (rerun and retry) never
// carries the parts of the run that happened; a retry with another model
// ("使用当前设置") drops the checkpoint file and the power state, one with
// the same configuration keeps them.
func TestPowerKeepModelDropsPartsAndModelChangeDropsCheckpoint(t *testing.T) {
	de, asr := a43Env(t)
	cudaRow := devRow(model.DeviceCUDA0, 300, 1)
	l := de.lectureOn(autoOn(cudaRow))
	asr.release(l.ID)
	de.start()
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	withParts := func(raw string) string {
		s, ok := model.ParseASRSnapshot(raw)
		if !ok {
			t.Fatalf("snapshot %s", raw)
		}
		s.Parts = []model.ASRPart{
			{FromMS: 0, ToMS: 60000, DeviceID: model.DeviceOVGPU, Model: "large-v3-turbo", Precision: model.PrecisionFP16},
			{FromMS: 60000, ToMS: 120000, DeviceID: model.DeviceCUDA0, Model: "large-v3", Precision: model.PrecisionAuto},
		}
		return model.MarshalSnapshot(s)
	}
	ran := de.lj(l.ID, model.StageASR)
	if err := de.st.SetJobSnapshot(de.ctx, ran.ID, withParts(ran.ConfigSnapshot)); err != nil {
		t.Fatal(err)
	}

	// Rerun, 沿用原模型: fixed to cuda:0, without the parts.
	asr.block(l.ID)
	if err := de.rerunASR(l, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	j := de.lj(l.ID, model.StageASR)
	if j.ID == ran.ID || strings.Contains(j.ConfigSnapshot, "parts") {
		t.Fatalf("rerun snapshot %s", j.ConfigSnapshot)
	}
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || s.Auto || len(s.Parts) != 0 {
		t.Fatalf("pinned %+v", s)
	}

	// Retry, 沿用原模型, of a fixed snapshot with parts: the parts go, the
	// configuration (and so the checkpoint) stays.
	de.waitRunningOn(l, model.DeviceCUDA0)
	j = de.lj(l.ID, model.StageASR)
	if err := de.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageASR, model.StatusCancelled)
	j = de.lj(l.ID, model.StageASR)
	if err := de.st.SetJobSnapshot(de.ctx, j.ID, withParts(j.ConfigSnapshot)); err != nil {
		t.Fatal(err)
	}
	dir := model.LectureDir(de.dataDir, l.CourseID, l.ID)
	ckpt := filepath.Join(dir, model.ASRCheckpointFile)
	interrupted := func() {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{ckpt, ckpt + ".tmp"} {
			if err := os.WriteFile(p, []byte(`{"version":1}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		st := de.lj(l.ID, model.StageASR).Stats
		st.ASRResume = &model.ASRResume{DeviceID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, At: "2026-10-24T08:00:00Z"}
		if err := de.st.SetJobStats(de.ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	interrupted()
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(l, model.DeviceCUDA0)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || s.Auto || len(s.Parts) != 0 {
		t.Fatalf("retry, 沿用原模型: %+v", s)
	}
	if _, err := os.Stat(ckpt); err != nil {
		t.Fatalf("the same configuration lost its checkpoint: %v", err)
	}
	if de.lj(l.ID, model.StageASR).Stats.ASRResume == nil {
		t.Fatal("the same configuration lost asr_resume")
	}

	// Retry, 使用当前设置, after the device's model changed: started over.
	if err := de.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageASR, model.StatusCancelled)
	interrupted()
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Model = "large-v3-turbo" })
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ckpt, ckpt + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after a model change (%v)", filepath.Base(p), err)
		}
	}
	de.waitRunningOn(l, model.DeviceCUDA0)
	if j := de.lj(l.ID, model.StageASR); j.Stats.ASRResume != nil {
		t.Fatalf("asr_resume kept after a model change: %+v", j.Stats.ASRResume)
	}
	if s := de.asrSnap(l); s.Model != "large-v3-turbo" || !s.Auto {
		t.Fatalf("current choice ran with %+v", s)
	}
	asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}
