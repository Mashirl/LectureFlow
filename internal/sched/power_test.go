// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Power management (power-req.md, power-brief §3.4–3.7, §7 P1): the
// scheduler side of the power-aware policy with a fake power reading and
// blocking fake asr stages.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// causeRecorder is a devStage hook that blocks until the run is cancelled
// (recording context.Cause) or the lecture is released.
type causeRecorder struct {
	mu     sync.Mutex
	causes map[string][]error
	de     *devEnv
}

func (c *causeRecorder) hook(ctx context.Context, id, dev string) error {
	c.de.asr.mu.Lock()
	ch := c.de.asr.ch[id]
	pass := c.de.asr.pass[id]
	c.de.asr.mu.Unlock()
	if pass {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		cause := context.Cause(ctx)
		c.mu.Lock()
		c.causes[id] = append(c.causes[id], cause)
		c.mu.Unlock()
		// As the asr stage does (task P3): the error wraps the cause.
		if IsPowerInterrupt(cause) {
			return errors.Join(ctx.Err(), cause)
		}
		return ctx.Err()
	}
}

func (c *causeRecorder) of(id string) []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.causes[id]...)
}

func withCauses(de *devEnv) *causeRecorder {
	c := &causeRecorder{causes: map[string][]error{}, de: de}
	de.asr.mu.Lock()
	de.asr.hook = c.hook
	de.asr.mu.Unlock()
	return c
}

// eventsOf lists the statuses of the Job events of one job, in order.
func (de *devEnv) eventsOf(jobID string) []string {
	de.ev.mu.Lock()
	defer de.ev.mu.Unlock()
	var out []string
	for _, j := range de.ev.jobs {
		if j.ID == jobID {
			out = append(out, j.Status)
		}
	}
	return out
}

func (de *devEnv) notice(code, jobID string) (PowerNotice, bool) {
	for _, n := range de.s.PowerStatus().Notices {
		if n.Info.Code == code && n.JobID == jobID {
			return n, true
		}
	}
	return PowerNotice{}, false
}

// Unplugged while the Intel GPU transcribes: the run is killed with
// ErrPowerMigrate as the cause, the job goes back to queued in one
// transaction (no failed event, no error, attempt+1, power_interrupts and
// asr_resume set) and continues on the NPU; notice power.switched.
func TestPowerUnplugMigratesToNPU(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, nil)
	causes := withCauses(de)
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(igpu))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceOVGPU)
	before := de.lj(l.ID, model.StageASR)

	de.power.set(onBattery)
	de.waitRunningOn(l, model.DeviceOVNPU)
	if cs := causes.of(l.ID); len(cs) != 1 || !errors.Is(cs[0], ErrPowerMigrate) || !IsPowerInterrupt(cs[0]) {
		t.Fatalf("causes = %v", cs)
	}
	j := de.lj(l.ID, model.StageASR)
	if j.ID != before.ID || j.Attempt != before.Attempt+1 || (j.Error != nil && *j.Error != "") {
		t.Fatalf("after the migration: id %s attempt %d error %v", j.ID, j.Attempt, j.Error)
	}
	st := j.Stats
	if st.PowerInterrupts != 1 || st.ASRResume == nil || st.ASRResume.DeviceID != model.DeviceOVGPU || st.ASRResume.Backend != model.BackendOpenVINO ||
		st.ASRResume.At == "" || st.PowerEvent == nil || st.PowerEvent.Code != CodePowerSwitched ||
		st.PowerEvent.FromDevice != model.DeviceOVGPU || st.PowerEvent.ToDevice != model.DeviceOVNPU {
		t.Fatalf("stats = %+v (resume %+v, event %+v)", st, st.ASRResume, st.PowerEvent)
	}
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVNPU || !s.Auto {
		t.Fatalf("bound to %+v", s)
	}
	if evs := de.eventsOf(j.ID); slices.Contains(evs, model.StatusFailed) {
		t.Fatalf("a failed status was emitted: %v", evs)
	}
	if got := de.asr.startsOf(l.ID); !slices.Equal(got, []string{model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("starts = %v", got)
	}
	n, ok := de.notice(CodePowerSwitched, j.ID)
	if !ok || n.LectureID != l.ID || n.CourseID != l.CourseID || n.Info.Params["lecture"] != l.Title ||
		n.Info.Params["device"] != npu.Label || n.ID == "" || n.At == "" {
		t.Fatalf("notice %+v (%v)", n, ok)
	}
	de.asr.release(l.ID)
	done := de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if done.Stats.ASRResume != nil || done.Stats.PowerEvent == nil || done.Stats.PowerInterrupts != 1 {
		t.Fatalf("done: resume %+v event %+v", done.Stats.ASRResume, done.Stats.PowerEvent)
	}
}

// Unplugged while CUDA transcribes: the run is killed with ErrPowerSuspend,
// the job waits power_wait_ac and is not handed to the NPU or the Intel
// GPU (a new job takes the NPU meanwhile); plugged in, it waits for the
// NPU's lecture (plugging in never interrupts), then continues on CUDA:
// power.suspended, power.resumed. The policy's status follows.
func TestPowerUnplugSuspendsCUDAAndResumes(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu}, nil)
	causes := withCauses(de)
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if ps := de.s.PowerStatus(); !ps.View.BatteryMachine || ps.View.Reason != devices.PolicyReasonACAuto ||
		!slices.Equal(ps.View.Active, []string{model.DeviceCUDA0}) || ps.WaitAC != 0 {
		t.Fatalf("on AC: %+v", ps)
	}

	de.power.set(onBattery)
	de.stillQueued(l, model.WaitPowerWaitAC)
	if cs := causes.of(l.ID); len(cs) != 1 || !errors.Is(cs[0], ErrPowerSuspend) {
		t.Fatalf("causes = %v", cs)
	}
	j := de.lj(l.ID, model.StageASR)
	if j.Stats.PowerInterrupts != 1 || j.Stats.ASRResume == nil || j.Stats.ASRResume.DeviceID != model.DeviceCUDA0 ||
		j.Stats.ASRResume.Backend != model.BackendFasterWhisper || j.Stats.PowerEvent == nil || j.Stats.PowerEvent.Code != CodePowerSuspended {
		t.Fatalf("suspended: %+v", j.Stats)
	}
	if n, ok := de.notice(CodePowerSuspended, j.ID); !ok || n.Info.Params["lecture"] != l.Title || n.Info.Params["device"] != nil {
		t.Fatalf("suspended notice %+v (%v)", n, ok)
	}
	// A new lecture uses the NPU; the suspended one stays where it is.
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceOVNPU)
	de.stillQueued(l, model.WaitPowerWaitAC)
	ps := de.s.PowerStatus()
	if ps.WaitAC != 1 || !slices.Equal(ps.HoldsSuspended, []string{model.DeviceCUDA0}) || !ps.Power.OnBatteryPower() ||
		ps.View.Reason != devices.PolicyReasonBattery || !slices.Equal(ps.View.Active, []string{model.DeviceOVNPU}) {
		t.Fatalf("on battery: %+v", ps)
	}
	for id, want := range map[string]bool{model.DeviceCUDA0: true, model.DeviceOVGPU: true, model.DeviceOVNPU: false} {
		if de.status(id).BatteryHold != want {
			t.Fatalf("battery hold %s", id)
		}
	}

	// Plugged in: the NPU finishes its lecture first.
	de.power.set(onACLaptop)
	de.stillQueued(l, model.WaitPowerSwitchPending(model.DeviceOVNPU, model.DeviceCUDA0))
	if j := de.lj(l2.ID, model.StageASR); j.Status != model.StatusRunning || j.Stats.PowerInterrupts != 0 {
		t.Fatalf("plugging in interrupted the NPU: %s", j.Status)
	}
	if ps := de.s.PowerStatus(); ps.View.Next != model.DeviceCUDA0 || !slices.Equal(ps.View.Active, []string{model.DeviceOVNPU}) {
		t.Fatalf("switch pending view: %+v", ps.View)
	}
	de.asr.release(l2.ID)
	de.waitRunningOn(l, model.DeviceCUDA0)
	j = de.lj(l.ID, model.StageASR)
	de.waitFor("power.resumed", 5*time.Second, func() bool {
		j = de.lj(l.ID, model.StageASR)
		return j.Stats.PowerEvent != nil && j.Stats.PowerEvent.Code == CodePowerResumed
	})
	if e := j.Stats.PowerEvent; e.FromDevice != model.DeviceCUDA0 || e.ToDevice != model.DeviceCUDA0 {
		t.Fatalf("resumed event %+v", e)
	}
	if n, ok := de.notice(CodePowerResumed, j.ID); !ok || n.Info.Params["device"] != cuda.Label || n.Info.Params["lecture"] != l.Title {
		t.Fatalf("resumed notice %+v (%v)", n, ok)
	}
	if got := de.asr.startsOf(l.ID); !slices.Equal(got, []string{model.DeviceCUDA0, model.DeviceCUDA0}) {
		t.Fatalf("starts = %v", got)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if ps := de.s.PowerStatus(); ps.WaitAC != 0 || len(ps.HoldsSuspended) != 0 {
		t.Fatalf("after the resume: %+v", ps)
	}
	// Notices oldest first.
	var codes []string
	for _, n := range de.s.PowerStatus().Notices {
		codes = append(codes, n.Info.Code)
	}
	if !slices.Equal(codes, []string{CodePowerSuspended, CodePowerResumed}) {
		t.Fatalf("notices %v", codes)
	}
}

// Plugging in never interrupts: the NPU's run continues, the next job waits
// power_switch_pending, then goes to CUDA.
func TestPowerPlugInDoesNotInterrupt(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, nil)
	de.power.set(onBattery)
	l1 := de.lectureOn(autoOn(cuda))
	de.prepDone(l1)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVNPU)
	de.power.set(onACLaptop)
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l2, model.WaitPowerSwitchPending(model.DeviceOVNPU, model.DeviceCUDA0))
	if j := de.lj(l1.ID, model.StageASR); j.Status != model.StatusRunning || j.Stats.PowerInterrupts != 0 || len(de.asr.startsOf(l1.ID)) != 1 {
		t.Fatalf("NPU run after plugging in: %s %v", j.Status, de.asr.startsOf(l1.ID))
	}
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// The preferred device busy: the job queues for it (power_queue), never
// goes to another device; and lead ruling 9.3: a job fixed to a device
// outside the active set is not starved by a stream of auto jobs on a
// two-slot device (the switch barrier).
func TestPowerQueueAndSwitchBarrier(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	de.power.set(onACLaptop)
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.stillQueued(l2, model.WaitPowerQueue(model.DeviceCUDA0))
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 {
		t.Fatalf("the Intel GPU took a job: %+v", s)
	}
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)

	// CUDA with two slots, one auto job running; then (FIFO) a job pinned
	// to the Intel GPU and another auto job.
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.MaxParallel = 2 })
	a0 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(a0, model.DeviceCUDA0)
	// Both get past their prep while the global switch holds every asr
	// job, so they meet the policy in the same scan, in FIFO order.
	if err := de.s.SetGPUPaused(true, false); err != nil {
		t.Fatal(err)
	}
	pinned := de.lectureOn(fixedOn(igpu))
	a1 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitReason(pinned, model.WaitGPUPaused)
	de.waitReason(a1, model.WaitGPUPaused)
	if err := de.s.SetGPUPaused(false, false); err != nil {
		t.Fatal(err)
	}
	want := model.WaitPowerSwitchPending(model.DeviceCUDA0, model.DeviceOVGPU)
	de.stillQueued(pinned, want)
	// a1 would fit in CUDA's free slot; the barrier lets CUDA drain.
	de.stillQueued(a1, want)
	de.asr.release(a0.ID)
	de.waitRunningOn(pinned, model.DeviceOVGPU)
	de.stillQueued(a1, model.WaitPowerSwitchPending(model.DeviceOVGPU, model.DeviceCUDA0))
	de.asr.release(pinned.ID)
	de.waitRunningOn(a1, model.DeviceCUDA0)
	de.asr.release(a1.ID)
	de.waitStatus(a1.ID, model.StageASR, model.StatusDone)
}

// Several unplug / plug-in cycles do not use up the automatic retries: a
// real failure afterwards still gets its two retries.
func TestPowerInterruptsKeepAutoRetries(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, nil)
	var failing atomic.Bool
	var failedRuns atomic.Int32
	failCh := make(chan struct{})
	fail := func() error {
		failedRuns.Add(1)
		return errors.New("worker exited with code 1")
	}
	de.asr.hook = func(ctx context.Context, id, dev string) error {
		if failing.Load() {
			return fail()
		}
		select {
		case <-failCh:
			return fail()
		case <-ctx.Done():
			if cause := context.Cause(ctx); IsPowerInterrupt(cause) {
				return errors.Join(ctx.Err(), cause)
			}
			return ctx.Err()
		}
	}
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	for i := 0; i < 3; i++ {
		de.waitRunningOn(l, model.DeviceCUDA0)
		de.power.set(onBattery)
		de.waitReason(l, model.WaitPowerWaitAC)
		de.power.set(onACLaptop)
	}
	de.waitRunningOn(l, model.DeviceCUDA0)
	if j := de.lj(l.ID, model.StageASR); j.Attempt != 3 || j.Stats.PowerInterrupts != 3 || model.CountedAttempts(j) != 0 {
		t.Fatalf("after three cycles: attempt %d interrupts %d", j.Attempt, j.Stats.PowerInterrupts)
	}
	failing.Store(true)
	close(failCh)
	j := de.waitStatus(l.ID, model.StageASR, model.StatusFailed)
	if n := failedRuns.Load(); n != 1+model.MaxAutoRetries {
		t.Fatalf("%d failing runs, want %d (the first and two automatic retries)", n, 1+model.MaxAutoRetries)
	}
	if j.Attempt != 3+model.MaxAutoRetries || model.CountedAttempts(j) != model.MaxAutoRetries || j.Error == nil || *j.Error == "" {
		t.Fatalf("failed: attempt %d counted %d error %v", j.Attempt, model.CountedAttempts(j), j.Error)
	}
}

// §4.7 recovery counts model.CountedAttempts: a job requeued twice by the
// power policy (attempt 2 = MaxAutoRetries by the raw count) is requeued by
// a restart and runs.
func TestPowerInterruptsAndRecovery(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, nil)
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	for i := 0; i < 2; i++ {
		de.waitRunningOn(l, model.DeviceCUDA0)
		de.power.set(onBattery)
		de.waitReason(l, model.WaitPowerWaitAC)
		de.power.set(onACLaptop)
	}
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.s.Stop()
	if err := state.Recover(de.ctx, de.st, de.dataDir); err != nil {
		t.Fatal(err)
	}
	if j := de.lj(l.ID, model.StageASR); j.Status != model.StatusQueued || j.Attempt != 3 || j.Stats.PowerInterrupts != 2 {
		t.Fatalf("recovered: %s attempt %d interrupts %d", j.Status, j.Attempt, j.Stats.PowerInterrupts)
	}
	de.s = de.newScheduler(instant(model.StagePrep, ResourceCPU), de.asr.stage())
	de.wire(de.s)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// Self-tests on a machine with a battery (power-brief §3.5): on battery only
// the battery device may be tested (StartDeviceTest: 409
// selftest.power_battery); pulling the charger cancels the tests of other
// devices, waiting or running, with that code; on AC a test starts only
// while no other device is active.
func TestPowerSelfTestRules(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu}, func(o *Options) { o.DeviceTest = ft.run })
	de.power.set(onBattery)
	de.start()
	de.waitFor("battery seen", 5*time.Second, func() bool { return de.s.Power() == onBattery })
	for _, id := range []string{model.DeviceCUDA0, model.DeviceOVGPU} {
		_, err := de.s.StartDeviceTest(de.ctx, id)
		var ce *model.CodedError
		row, _ := de.devs.Get(id)
		if !errors.Is(err, ErrSelfTestPowerBattery) || !errors.Is(err, state.ErrConflict) || !errors.As(err, &ce) ||
			ce.Info.Code != CodeSelfTestPowerBattery || ce.Info.Params["device"] != row.Label {
			t.Fatalf("test of %s on battery: %v", id, err)
		}
		if _, ok := de.s.DeviceTest(id); ok {
			t.Fatalf("a refused test of %s left a state", id)
		}
	}
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)

	// On AC: the Intel GPU's test runs; a test of CUDA waits while the
	// Intel GPU's test holds its slot (only one active device).
	de.power.set(onACLaptop)
	de.waitFor("AC seen", 5*time.Second, func() bool { return de.s.Power() == onACLaptop })
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	<-ft.started
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceCUDA0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceCUDA0); st.State != DeviceTestWaiting {
		t.Fatalf("CUDA test while the Intel GPU is tested: %s", st.State)
	}
	// Unplugged: both are cancelled with selftest.power_battery.
	de.power.set(onBattery)
	for _, id := range []string{model.DeviceOVGPU, model.DeviceCUDA0} {
		st := de.waitTest(id, DeviceTestCancelled)
		row, _ := de.devs.Get(id)
		if st.Error == nil || st.Error.Code != CodeSelfTestPowerBattery || st.Error.Params["device"] != row.Label {
			t.Fatalf("cancelled test of %s: %+v", id, st.Error)
		}
	}
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 {
		t.Fatalf("slot kept: %+v", s)
	}

	// On AC, a test waits while a pipeline job runs on another device, and
	// starts once it ended.
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting {
		t.Fatalf("NPU test while CUDA transcribes: %s", st.State)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
}

type awakeRec struct {
	mu    sync.Mutex
	calls []bool
}

func (a *awakeRec) set(on bool) {
	a.mu.Lock()
	a.calls = append(a.calls, on)
	a.mu.Unlock()
}

func (a *awakeRec) get() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.calls...)
}

// The keep-awake hook (power-brief §3.6): held while any pipeline asr job
// or self-test runs, released when the last one ended; overlapping runs
// switch it once.
func TestPowerKeepAwakeSequence(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	ft := newFakeTest(1)
	rec := &awakeRec{}
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, func(o *Options) {
		o.DeviceTest = ft.run
		o.KeepAwake = rec.set
	})
	de.setASR(func(a *model.ASRSettings) {
		a.DeviceMode, a.ManualDevices = model.ASRDeviceModeManual, []string{model.DeviceCUDA0}
	})
	de.start()
	time.Sleep(30 * time.Millisecond)
	if got := rec.get(); len(got) != 0 {
		t.Fatalf("calls while idle: %v", got)
	}
	// A transcription and a self-test that overlap (a desktop: both run).
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	time.Sleep(30 * time.Millisecond)
	if got := rec.get(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("while the test still runs: %v", got)
	}
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	de.waitFor("released", 5*time.Second, func() bool { return slices.Equal(rec.get(), []bool{true, false}) })
	// Another transcription: once more.
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	de.waitFor("second cycle", 5*time.Second, func() bool { return slices.Equal(rec.get(), []bool{true, false, true, false}) })
	// A run killed by the power policy releases it as well.
	de.power.set(onACLaptop)
	l3 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.power.set(onBattery)
	de.waitReason(l3, model.WaitPowerWaitAC)
	de.waitFor("third cycle", 5*time.Second, func() bool {
		return slices.Equal(rec.get(), []bool{true, false, true, false, true, false})
	})
}

// holdsOnly checks over several scans that the request was held once and
// never released.
func (a *awakeRec) holdsOnly(t *testing.T, when string) {
	t.Helper()
	time.Sleep(40 * time.Millisecond) // several scans (ScanInterval 5 ms)
	if got := a.get(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("%s: KeepAwake calls %v, want [true]", when, got)
	}
}

// heldThenReleased waits for the release after the last run and checks
// that nothing follows it.
func (a *awakeRec) heldThenReleased(de *devEnv, when string) {
	de.t.Helper()
	want := []bool{true, false}
	de.waitFor(when, 5*time.Second, func() bool { return slices.Equal(a.get(), want) })
	time.Sleep(40 * time.Millisecond)
	if got := a.get(); !slices.Equal(got, want) {
		de.t.Fatalf("%s: KeepAwake calls %v, want %v", when, got, want)
	}
}

// Power-checkpoint §6.8 item 4: a run the power policy migrates keeps the
// power request held — no KeepAwake(false) between the killed attempt on
// the Intel GPU and the next one on the NPU (the release waits for the end
// of the next scan, which starts it again); released once it is done.
func TestPowerKeepAwakeHeldAcrossMigration(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	rec := &awakeRec{}
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, func(o *Options) { o.KeepAwake = rec.set })
	causes := withCauses(de)
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(igpu))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceOVGPU)
	rec.holdsOnly(t, "on the Intel GPU")

	de.power.set(onBattery)
	de.waitRunningOn(l, model.DeviceOVNPU)
	if cs := causes.of(l.ID); len(cs) != 1 || !errors.Is(cs[0], ErrPowerMigrate) {
		t.Fatalf("causes = %v", cs)
	}
	if got := de.asr.startsOf(l.ID); !slices.Equal(got, []string{model.DeviceOVGPU, model.DeviceOVNPU}) {
		t.Fatalf("starts = %v", got)
	}
	rec.holdsOnly(t, "after the migration")

	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	rec.heldThenReleased(de, "released after the migrated run")
}

// The same for the other hand-offs: the next queued lecture taking over the
// device, then a self-test that waited for the pipeline work (on AC a
// machine with a battery tests only while no other device is active), keep
// the request held; released after the self-test, the last one.
func TestPowerKeepAwakeHeldAcrossHandOff(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(1)
	rec := &awakeRec{}
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, func(o *Options) {
		o.DeviceTest = ft.run
		o.KeepAwake = rec.set
	})
	de.power.set(onACLaptop)
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.stillQueued(l2, model.WaitPowerQueue(model.DeviceCUDA0))
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting {
		t.Fatalf("NPU test while CUDA transcribes: %s", st.State)
	}

	de.asr.release(l1.ID)
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	rec.holdsOnly(t, "after the next lecture took over")

	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	rec.holdsOnly(t, "after the self-test took over")

	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	rec.heldThenReleased(de, "released after the self-test")
}

// The deferred release never holds the request for long: with a scan
// interval of an hour the end of the last run (a self-test, which kicks its
// scan before it is uncounted; then a pipeline job) still releases it
// within moments; a shutdown releases it before Stop returns.
func TestPowerKeepAwakeReleasedPromptly(t *testing.T) {
	cuda, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(1)
	rec := &awakeRec{}
	de := newDevEnv(t, []model.ASRDevice{cuda, npu}, func(o *Options) {
		o.ScanInterval = time.Hour
		o.DeviceTest = ft.run
		o.KeepAwake = rec.set
	})
	de.power.set(onAC) // a desktop
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.waitFor("released after the self-test", 2*time.Second, func() bool { return slices.Equal(rec.get(), []bool{true, false}) })

	// Driven by kicks only (the instant prep stage's end kicks the scan
	// that starts the asr job).
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitFor("released after the transcription", 2*time.Second, func() bool {
		return slices.Equal(rec.get(), []bool{true, false, true, false})
	})

	// Shut down while a transcription runs: released before Stop returns.
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.s.Stop()
	if got := rec.get(); !slices.Equal(got, []bool{true, false, true, false, true, false}) {
		t.Fatalf("after Stop: KeepAwake calls %v", got)
	}
	if j := de.lj(l2.ID, model.StageASR); j.Status != model.StatusRunning {
		t.Fatalf("shutdown left the job %s, want running (§4.7)", j.Status)
	}
}

// The worker memory budget (power-brief §3.7): the running workers'
// estimates plus the new one stay within asr.worker_mem_budget_mb, CUDA
// included; otherwise the job waits "memory".
func TestPowerWorkerMemoryBudget(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	de.memEst.Store(5000) // two workers: 10000 > 8192
	if de.cfg.ASR.WorkerMemBudgetMB != 8192 {
		t.Fatalf("budget %d", de.cfg.ASR.WorkerMemBudgetMB)
	}
	// l2 is fixed to the Intel GPU, so its reason is that device's (an auto
	// job reports its highest-priority candidate's: CUDA, full).
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(fixedOn(igpu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	// The Intel GPU is free and the system has memory (64 GB), but the
	// budget does not fit a second worker.
	de.stillQueued(l2, model.WaitMemory)
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceOVGPU)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)

	// A larger budget lets both run at once.
	de.cfg.ASR.WorkerMemBudgetMB = 10000
	l3, l4 := de.lectureOn(autoOn(cuda)), de.lectureOn(fixedOn(igpu))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.waitRunningOn(l4, model.DeviceOVGPU)
	de.asr.release(l3.ID)
	de.asr.release(l4.ID)
	de.waitStatus(l4.ID, model.StageASR, model.StatusDone)
	// A single worker over the budget never starts.
	de.cfg.ASR.WorkerMemBudgetMB = 4000
	l5 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l5, model.WaitMemory)
}

// PowerStatus keeps the last PowerNoticeMax notices of the last
// PowerNoticeAge, oldest first, and never returns nil slices.
func TestPowerNoticesKeepTheLatest(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC).Unix())
	de := newDevEnv(t, []model.ASRDevice{devRow(model.DeviceOVNPU, 100, 1)}, func(o *Options) {
		o.Now = func() time.Time { return time.Unix(now.Load(), 0) }
	})
	ps := de.s.PowerStatus()
	if ps.Notices == nil || ps.HoldsSuspended == nil || ps.View.Active == nil || len(ps.Notices) != 0 {
		t.Fatalf("empty status: %+v", ps)
	}
	l := de.lecture()
	for i := 0; i < PowerNoticeMax+5; i++ {
		de.s.addPowerNotice(CodePowerSwitched, "job", l.ID, model.DeviceOVNPU)
		now.Add(1)
	}
	ns := de.s.PowerStatus().Notices
	if len(ns) != PowerNoticeMax || ns[0].At >= ns[len(ns)-1].At {
		t.Fatalf("%d notices, first %s last %s", len(ns), ns[0].At, ns[len(ns)-1].At)
	}
	if ns[0].Info.Params["lecture"] != l.Title || ns[0].Info.Params["device"] != "label "+model.DeviceOVNPU || ns[0].CourseID != l.CourseID {
		t.Fatalf("notice %+v", ns[0])
	}
	seen := map[string]bool{}
	for _, n := range ns {
		if seen[n.ID] {
			t.Fatalf("duplicate id %s", n.ID)
		}
		seen[n.ID] = true
	}
	now.Add(int64(PowerNoticeAge/time.Second) + 60)
	if ns := de.s.PowerStatus().Notices; len(ns) != 0 {
		t.Fatalf("old notices kept: %d", len(ns))
	}
}
