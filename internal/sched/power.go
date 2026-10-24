// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Power management (user requirement 2026-10-24; .claude/power-plan.md,
// .claude/power-brief.md). Types and signatures FROZEN by the lead for
// v2-step20; bodies: task P1. The policy itself is devices.Place /
// OnPowerChange / SelfTest / View (pure); the scheduler builds a
// devices.PolicyInput once per scan from its 3b scan state and acts:
//
//   - every scan: a queued pipeline asr job goes to devices.Place's devices
//     (then the 3b admission: self-test, multi_device, slots, VRAM, memory)
//     or waits with its Wait code; battery_aware is no longer read;
//   - a change of the power reading (readPower): devices.OnPowerChange for
//     the running pipeline asr jobs; PowerMigrate / PowerSuspend cancel the
//     run's context with the cause ErrPowerMigrate / ErrPowerSuspend
//     (context.WithCancelCause) — the worker is killed at once (3b lead
//     ruling 9) and keeps its checkpoint; self-tests that devices.SelfTest
//     no longer allows are cancelled;
//   - the end of such a run (finish): not a failure and no failure
//     notification — stats.power_interrupts+1, stats.asr_resume = the
//     device, stats.power_event, then state.PowerRequeue (running → failed
//     → queued in one transaction, job.error cleared); a PowerNotice is
//     recorded (power.switched or power.suspended); when a job that had
//     waited power_wait_ac starts again on AC: power.resumed (started on
//     battery: the suspended event is dropped, no notice; backend Gate
//     round 1, fix 6); a CPU run suspended on AC (a machine with a battery
//     never uses the CPU, fix 7) gets no event and no notice ("已拔掉电源"
//     would be false);
//   - automatic retries count model.CountedAttempts (attempt minus
//     power_interrupts), here and in state.FailJob / AutoRetry, so §4.7
//     recovery and the retry limit ignore power requeues;
//   - a run that finished successfully although a power action was
//     pending is done as usual (the kill came too late).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

var (
	// ErrPowerInterrupt is the class of both power interrupts: a stage
	// whose context was cancelled with one of them as the cause
	// (context.Cause) returns an error wrapping that cause, so
	// IsPowerInterrupt(err) holds, and keeps its checkpoint.
	ErrPowerInterrupt = errors.New("interrupted by the power-aware policy")
	// ErrPowerMigrate: the run continues on the battery device of the same
	// backend from its checkpoint (devices.PowerMigrate).
	ErrPowerMigrate = fmt.Errorf("%w: moving to the battery device", ErrPowerInterrupt)
	// ErrPowerSuspend: the run waits for AC power (devices.PowerSuspend,
	// waiting_for model.WaitPowerWaitAC).
	ErrPowerSuspend = fmt.Errorf("%w: waiting for the power adapter", ErrPowerInterrupt)

	// ErrSelfTestPowerBattery: StartDeviceTest of a device the power policy
	// does not allow on battery (devices.SelfTest Allowed false). P1 returns
	// it inside a *model.CodedError with CodeSelfTestPowerBattery {device};
	// it wraps state.ErrConflict, so the API answers 409 (task P4 maps it
	// like ErrSelfTestRunning).
	ErrSelfTestPowerBattery = fmt.Errorf("%w: on battery only the battery device can be tested", state.ErrConflict)
	// ErrSelfTestPowerCPU: StartDeviceTest of a CPU device on a machine with
	// a battery (any power state; backend Gate round 1). Returned inside a
	// *model.CodedError with CodeSelfTestPowerCPU {device}; it wraps
	// state.ErrConflict, so the API answers 409 with that code.
	ErrSelfTestPowerCPU = fmt.Errorf("%w: a machine with a battery never uses the CPU", state.ErrConflict)
)

// IsPowerInterrupt reports whether err (a stage's error, or a context
// cause) is a power interrupt.
func IsPowerInterrupt(err error) bool { return errors.Is(err, ErrPowerInterrupt) }

// PowerNotice is one notification of the power policy (requirement 7), kept
// in memory by the scheduler (the last PowerNoticeMax, at most
// PowerNoticeAge old; lost on restart) and delivered through the resource
// snapshot (resources.Policy.Notices, SSE "resources"): the front end shows
// a toast / browser notification once per ID (task P5).
type PowerNotice struct {
	ID string `json:"id"` // unique: "{job id}:{code}:{at}"
	At string `json:"at"` // UTC RFC3339
	// Info is the text: notes.{code} — CodePowerSwitched {lecture, device},
	// CodePowerSuspended {lecture}, CodePowerResumed {lecture, device};
	// lecture = the lecture's title, device = the device's label.
	Info      model.ErrorInfo `json:"info"`
	JobID     string          `json:"job_id"`
	LectureID string          `json:"lecture_id"`
	CourseID  string          `json:"course_id"`
}

// How many notices PowerStatus keeps.
const (
	PowerNoticeMax = 20
	PowerNoticeAge = 30 * time.Minute
)

// PowerStatus is the power policy's state for the resource bar
// (resources.Deps.PowerStatus, task P4).
type PowerStatus struct {
	Power devices.Power
	View  devices.PolicyView
	// WaitAC counts the queued asr jobs waiting model.WaitPowerWaitAC
	// ("2 个任务等待电源适配器"); WaitNoDevice those waiting
	// WaitPowerNoSaverDevice or WaitPowerNoDevice (last scan).
	WaitAC       int
	WaitNoDevice int
	// HoldsSuspended are the devices a power_wait_ac job will continue on
	// (its stats.asr_resume device): "有任务挂起" on the standby rows.
	HoldsSuspended []string
	Notices        []PowerNotice // oldest first; never nil
}

// PowerStatus returns the power policy's state now (the last scan's reasons
// and notices; View computed from the current state).
func (s *Scheduler) PowerStatus() PowerStatus {
	out := PowerStatus{Power: s.Power(), View: devices.PolicyView{Mode: model.ASRDeviceModeAuto, Active: []string{}},
		HoldsSuspended: []string{}, Notices: []PowerNotice{}}
	if s.devs == nil {
		return out
	}
	b := s.readPolicyBase()
	s.mu.Lock()
	in := s.policyInputLocked(&b)
	out.WaitAC, out.WaitNoDevice = s.powerWaitAC, s.powerNoDev
	out.HoldsSuspended = append(out.HoldsSuspended, s.powerHolds...)
	s.notices = s.prunedNoticesLocked()
	out.Notices = append(out.Notices, s.notices...)
	s.mu.Unlock()
	out.Power = b.power
	out.View = devices.View(in)
	return out
}

// suspendedOn is the device a queued asr job waiting power_wait_ac will
// continue on: its checkpoint's device (stats.asr_resume), else the device
// of its fixed snapshot ("有任务挂起" on the standby rows).
func suspendedOn(j model.Job) string {
	if j.Stats.ASRResume != nil && j.Stats.ASRResume.DeviceID != "" {
		return j.Stats.ASRResume.DeviceID
	}
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		if snap = model.NormalizeASRSnapshot(snap); !snap.Auto {
			return snap.DeviceID
		}
	}
	return ""
}

// addPowerNotice records a notice of the power policy (requirement 7):
// code CodePowerSwitched / CodePowerResumed {lecture, device} or
// CodePowerSuspended {lecture}; lecture is the lecture's title, device the
// device's label.
func (s *Scheduler) addPowerNotice(code, jobID, lectureID, deviceID string) {
	ctx := context.Background()
	n := PowerNotice{At: s.now().UTC().Format(time.RFC3339), JobID: jobID, LectureID: lectureID}
	n.ID = jobID + ":" + code + ":" + n.At
	params := map[string]any{"lecture": lectureID}
	if l, err := s.st.GetLecture(ctx, lectureID); err == nil {
		params["lecture"], n.CourseID = l.Title, l.CourseID
	}
	if code != CodePowerSuspended {
		label := deviceID
		if s.devs != nil {
			if d, ok := s.devs.Get(deviceID); ok {
				label = deviceLabel(d)
			}
		}
		params["device"] = label
	}
	n.Info = model.ErrorInfo{Code: code, Params: params}
	s.mu.Lock()
	s.notices = append(s.prunedNoticesLocked(), n)
	if len(s.notices) > PowerNoticeMax {
		s.notices = append([]PowerNotice(nil), s.notices[len(s.notices)-PowerNoticeMax:]...)
	}
	s.mu.Unlock()
	if s.devs != nil {
		s.ev.Changed("devices", "")
	}
}

// prunedNoticesLocked drops the notices older than PowerNoticeAge (caller
// holds s.mu).
func (s *Scheduler) prunedNoticesLocked() []PowerNotice {
	cut := s.now().Add(-PowerNoticeAge)
	out := s.notices[:0:0]
	for _, n := range s.notices {
		if at, err := time.Parse(time.RFC3339, n.At); err == nil && !at.Before(cut) {
			out = append(out, n)
		}
	}
	return out
}

// awake counts the running pipeline asr jobs and self-tests (power-brief
// §3.6) and calls Options.KeepAwake(true) when the count leaves 0 while the
// request is not held.
//
// The release is deferred (power-checkpoint §6.8 item 4): when the count
// returns to 0 while the scan loop runs, the request stays held until the
// end of the first scan that began after that moment (awakeScanEnd), and is
// released there only if the count is still 0. That scan lists the jobs
// after the ended run's final transition, so a run the power policy
// requeued (a migration), the next queued job or a self-test waiting for
// the device, if that scan starts it, keeps the request held without a
// release in between. Nothing is held for long: the drop kicks a scan at
// once (and the loop scans every ScanInterval anyway), a drop once the loop
// is shutting down releases at once, and the loop's exit settles a pending
// release (settleAwake). Calls are serialised (awakeMu).
func (s *Scheduler) awake(delta int) {
	if s.keepAwake == nil {
		return
	}
	s.awakeMu.Lock()
	defer s.awakeMu.Unlock()
	s.awakeN = max(0, s.awakeN+delta)
	switch {
	case s.awakeN > 0:
		s.awakePending = false
		if !s.awakeHeld {
			s.keepAwake(true)
			s.awakeHeld = true
		}
	case !s.awakeHeld:
		// nothing held, nothing to release
	case s.loopRunning():
		if !s.awakePending {
			s.awakePending, s.awakeDropScan = true, s.awakeScans
		}
		s.Kick()
	default:
		s.releaseAwakeLocked()
	}
}

// loopRunning reports whether the scan loop runs (Start was called and the
// scheduler is not shutting down): only then does a scan follow that can
// decide a deferred release. baseCtx is set once, before any run starts.
func (s *Scheduler) loopRunning() bool {
	return s.baseCtx != nil && s.baseCtx.Err() == nil
}

// awakeScanBegin numbers a scan for the deferred release (awake); called
// under scanMu before the scan reads anything.
func (s *Scheduler) awakeScanBegin() uint64 {
	if s.keepAwake == nil {
		return 0
	}
	s.awakeMu.Lock()
	defer s.awakeMu.Unlock()
	s.awakeScans++
	return s.awakeScans
}

// awakeScanEnd decides a deferred release at the end of scan number scan
// (still under scanMu, after the pipeline jobs and the self-tests had their
// turn): released if the scan began after the count returned to 0 and the
// count is still 0. A scan that was already under way when the count
// dropped may have listed the jobs before the ended run was requeued, so it
// leaves the decision to the next one (which the drop kicked).
func (s *Scheduler) awakeScanEnd(scan uint64) {
	if s.keepAwake == nil {
		return
	}
	s.awakeMu.Lock()
	defer s.awakeMu.Unlock()
	if s.awakePending && scan > s.awakeDropScan {
		s.releaseAwakeLocked()
	}
}

// settleAwake releases a deferred request without waiting for a scan (the
// loop ends: no scan will start anything any more).
func (s *Scheduler) settleAwake() {
	if s.keepAwake == nil {
		return
	}
	s.awakeMu.Lock()
	defer s.awakeMu.Unlock()
	if s.awakePending {
		s.releaseAwakeLocked()
	}
}

// releaseAwakeLocked ends a pending release: KeepAwake(false) when the
// request is held and nothing runs (caller holds awakeMu).
func (s *Scheduler) releaseAwakeLocked() {
	s.awakePending = false
	if s.awakeHeld && s.awakeN == 0 {
		s.keepAwake(false)
		s.awakeHeld = false
	}
}
