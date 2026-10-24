// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Power management in the mock (power-req parts 1 and 2, .claude/power-brief
// task P5): the power policy of a store — the real back end's pure
// functions of internal/devices (View for the resource bar, Place for the
// device a running job is put on and the single-active waits, SelfTest for
// the self-test start, BatteryDevice for battery_hold) on the store's state
// —, the upper area's readings (resources.System) and the policy state of
// the resource bar (resources.Policy), plus six scenarios with their own
// store:
//
//	power_ac_dgpu            a laptop on AC power, charging: the NVIDIA card is
//	                         the active device, the Intel devices stand by
//	power_ac_intel           a laptop on AC power without an NVIDIA card: the
//	                         Intel GPU is active, the NPU stands by; no CPU
//	                         temperature ("--")
//	power_battery_npu        on battery: the NPU is active; the lecture that
//	                         ran on the Intel GPU continued on it (power.switched)
//	power_battery_suspended  on battery: the NPU is active and a lecture of the
//	                         NVIDIA card waits for the power adapter
//	                         (power_wait_ac, power.suspended)
//	power_ac_switch          plugged in again while the NPU runs a lecture: it
//	                         finishes there, the queued lecture waits for the
//	                         switch to the NVIDIA card (power_switch_pending,
//	                         policy.next)
//	power_all_suspended      on battery with the Intel GPU and the NPU turned
//	                         off: no device qualifies, every transcription is
//	                         suspended (the NVIDIA lecture power_wait_ac, the
//	                         queued one power_no_saver_device); the middle
//	                         area shows why there is no active device
//
// The default data is a laptop on AC power as well (power_ac_dgpu without
// its own store); scenario cpuonly is a desktop (no battery: the 3b rules).

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
)

// sysReadings are a store's simulated upper-area readings.
type sysReadings struct {
	cpuBase int  // CPU utilisation around which the reading moves
	tempC   *int // CPU temperature; nil = not readable ("--")
}

func intPtr(v int) *int { return &v }

// powerOf returns a power state of a laptop (has_battery) or a desktop.
func powerOf(onBattery, hasBattery, charging bool, pct *int) resources.Power {
	p := resources.Power{Known: true, OnBattery: onBattery, HasBattery: hasBattery, Charging: charging, BatteryPct: pct}
	if !hasBattery {
		p.BatteryPct, p.Charging = nil, false
	}
	return p
}

// ---------- scenarios ----------

func setupPowerACDGPU(s *store) {
	s.dev.power = powerOf(false, true, true, intPtr(87))
	s.dev.sys = sysReadings{cpuBase: 24, tempC: intPtr(71)}
}

func setupPowerACIntel(s *store) {
	s.dev.noCUDA = true
	s.dev.power = powerOf(false, true, false, intPtr(100))
	s.dev.sys = sysReadings{cpuBase: 31, tempC: nil}
	s.queueASRWait(lecID2120(7), model.WaitPowerQueue(model.DeviceOVGPU))
}

func setupPowerBatteryNPU(s *store) {
	now := time.Now()
	s.dev.power = powerOf(true, true, false, intPtr(63))
	s.dev.npuUtil = true
	s.dev.sys = sysReadings{cpuBase: 15, tempC: intPtr(58)}
	run := lecID3631(7)
	s.dev.assigned[run] = model.DeviceOVNPU
	s.queueASRWait(lecID2120(7), model.WaitPowerQueue(model.DeviceOVNPU))
	s.powerNote(run, sched.CodePowerSwitched, labelNPU)
	s.addNotice(run, sched.CodePowerSwitched, labelNPU, now.Add(-3*time.Minute))
	s.resumedParts(lecID3631(3))
}

func setupPowerBatterySuspended(s *store) {
	now := time.Now()
	s.dev.power = powerOf(true, true, false, intPtr(41))
	s.dev.npuUtil = true
	s.dev.sys = sysReadings{cpuBase: 19, tempC: nil}
	// the lecture on the NPU started there after the charger was pulled
	// (no switch notice at the same moment as the suspension)
	run, held := lecID3631(7), lecID2120(7)
	s.dev.assigned[run] = model.DeviceOVNPU
	s.queueASRWait(held, model.WaitPowerWaitAC)
	s.dev.suspended[held] = model.DeviceCUDA0
	s.powerNote(held, sched.CodePowerSuspended, "")
	s.addNotice(held, sched.CodePowerSuspended, "", now.Add(-2*time.Minute))
	// the real check (power-checkpoint.md §6.5): 54:47, checkpoint at 21:16
	s.keepCheckpoint(held, 3287340, 1276460)
	s.resumedParts(lecID3631(3))
}

// setupPowerACSwitch: the charger was plugged in while the NPU runs a
// lecture (plugging in never interrupts a run): the NPU stays active until
// the lecture is done, then the NVIDIA card takes over (policy.next); the
// queued lecture waits for the switch.
func setupPowerACSwitch(s *store) {
	s.dev.power = powerOf(false, true, true, intPtr(48))
	s.dev.npuUtil = true
	s.dev.sys = sysReadings{cpuBase: 17, tempC: intPtr(61)}
	run := lecID3631(7)
	s.dev.assigned[run] = model.DeviceOVNPU
	s.queueASRWait(lecID2120(7), model.WaitPowerSwitchPending(model.DeviceOVNPU, model.DeviceCUDA0))
}

// setupPowerAllSuspended: on battery, the Intel GPU and the NPU turned off
// by the user. The NVIDIA card is never a battery device and the CPU is
// never used on a laptop, so nothing qualifies: the lecture that ran on
// the NVIDIA card was suspended when the charger was pulled (it continues
// there on AC power) and the queued one waits for a power-saving device.
func setupPowerAllSuspended(s *store) {
	now := time.Now()
	s.dev.power = powerOf(true, true, false, intPtr(34))
	s.dev.sys = sysReadings{cpuBase: 9, tempC: intPtr(52)}
	for _, d := range s.dev.devs {
		if d.row.ID == model.DeviceOVGPU || d.row.ID == model.DeviceOVNPU {
			d.row.Enabled = false
		}
	}
	held, queued := lecID3631(7), lecID2120(7)
	s.suspendASR(held)
	s.dev.suspended[held] = model.DeviceCUDA0
	s.queueASRWait(queued, model.WaitPowerNoSaverDevice)
	s.powerNote(held, sched.CodePowerSuspended, "")
	s.addNotice(held, sched.CodePowerSuspended, "", now.Add(-1*time.Minute))
	// it ran at 42 %; the last complete window ends a little earlier
	s.keepCheckpoint(held, 2873000, 1172000)
}

// suspendASR is state.PowerRequeue on a running asr stage: running →
// queued, waiting power_wait_ac (no failure shown; progress 0, the
// checkpoint's position is StageView.checkpoint, keepCheckpoint).
func (s *store) suspendASR(id string) {
	l, err := s.findLecture(id)
	if err != nil {
		return
	}
	j := stageIndex(l, model.StageASR)
	if j < 0 || l.Stages[j].Status != model.StatusRunning {
		return
	}
	w := model.WaitPowerWaitAC
	l.Stages[j].Status, l.Stages[j].WaitingFor, l.Stages[j].Progress = model.StatusQueued, &w, 0
	delete(s.progress0, l.ID+"/"+model.StageASR)
	s.changed(l)
}

// keepCheckpoint is views.go's StageView.checkpoint of a lecture whose asr
// the power policy put back in line (asr_checkpoint.go): the worker's
// checkpoint ends at resumeMS of a lecture durMS long. The fixture has no
// duration for lectures before asr; the real view knows it from audio.wav,
// so the scenario sets it. finalizeLecture drops the field once the stage
// is no longer queued.
func (s *store) keepCheckpoint(id string, durMS, resumeMS int64) {
	l, err := s.findLecture(id)
	if err != nil {
		return
	}
	j := stageIndex(l, model.StageASR)
	if j < 0 || l.Stages[j].Status != model.StatusQueued {
		return
	}
	d := durMS
	l.DurationMS = &d
	l.Stages[j].Checkpoint = &api.ASRCheckpointView{ResumeMS: resumeMS, DurationMS: durMS, Progress: float64(resumeMS) / float64(durMS)}
	s.changed(l)
}

func lecID3631(seq int) string { return lecIDOf("3631", seq) }
func lecID2120(seq int) string { return lecIDOf("2120", seq) }

// lecIDOf is fixtures_test.go's lecID (the fixture ids).
func lecIDOf(course string, seq int) string { return fmt.Sprintf("01JD%s%018d", course, seq) }

// queueASRWait sets the waiting_for of a lecture's queued asr stage.
func (s *store) queueASRWait(id, wait string) {
	l, err := s.findLecture(id)
	if err != nil {
		return
	}
	if j := stageIndex(l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusQueued {
		w := wait
		l.Stages[j].WaitingFor = &w
	}
}

// powerNote is the asr stage note of job.stats.power_event (views.go of
// task P4: queued / running asr stages show notes.power.*).
func (s *store) powerNote(id, code, device string) {
	l, err := s.findLecture(id)
	if err != nil {
		return
	}
	j := stageIndex(l, model.StageASR)
	if j < 0 {
		return
	}
	params := map[string]any{"lecture": l.Title}
	if device != "" {
		params["device"] = device
	}
	l.Stages[j].NoteInfo = &model.ErrorInfo{Code: code, Params: params}
}

func (s *store) addNotice(id, code, device string, at time.Time) {
	l, err := s.findLecture(id)
	if err != nil {
		return
	}
	params := map[string]any{"lecture": l.Title}
	if device != "" {
		params["device"] = device
	}
	ts := at.UTC().Format(time.RFC3339)
	job := "job-" + l.ID + "-asr"
	s.dev.notices = append(s.dev.notices, resources.PowerNotice{ID: job + ":" + code + ":" + ts, At: ts,
		Info: model.ErrorInfo{Code: code, Params: params}, JobID: job, LectureID: l.ID, CourseID: l.CourseID})
}

// resumedParts makes a finished lecture's asr snapshot one that was resumed
// from a checkpoint on the NPU after the charger was pulled (the hover
// card's parts, power-brief §3.8): the top-level device is the last part's.
func (s *store) resumedParts(id string) {
	l, err := s.findLecture(id)
	if err != nil || l.DurationMS == nil {
		return
	}
	j := stageIndex(l, model.StageASR)
	if j < 0 || l.Stages[j].Status != model.StatusDone {
		return
	}
	end := *l.DurationMS
	cut := end * 47 / 100 / 1000 * 1000
	const mdl, prec = "large-v3-turbo", model.PrecisionFP16
	snap := model.ASRSnapshot{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: mdl, Precision: prec, Language: "en",
		Parts: []model.ASRPart{
			{FromMS: 0, ToMS: cut, DeviceID: model.DeviceOVGPU, Model: mdl, Precision: prec},
			{FromMS: cut, ToMS: end, DeviceID: model.DeviceOVNPU, Model: mdl, Precision: prec},
		}}
	l.Stages[j].Model = &api.StageModel{Label: asrLabelOf(labelNPU, mdl, prec, "en"), Model: mdl, ASR: &snap}
}

// ---------- the policy ----------
//
// The mock asks the real back end's pure functions (internal/devices:
// Place, View, SelfTest, BatteryDevice) on its own state instead of
// mirroring their rules: policyInput is the scheduler's per-scan input
// (devices.PolicyInput) built from the store.

// asrJobID is the mock's id of a lecture's asr job.
func asrJobID(lectureID string) string { return "job-" + lectureID + "-asr" }

// policyInput is the store's state as the power policy sees it: the power
// state, the device mode (model.NormalizeASRDeviceMode), every visible row
// with its pause, slots and self-test, and the running asr jobs on the
// devices they were put on (runs: lecture id → device id). s.mu held.
func (s *store) policyInput(now time.Time, runs map[string]string) devices.PolicyInput {
	p := s.dev.power
	a := model.NormalizeASRDeviceMode(s.settings.ASR)
	in := devices.PolicyInput{
		Power: devices.Power{Known: p.Known, OnBattery: p.OnBattery, Saver: p.Saver, BatteryPct: p.BatteryPct, HasBattery: p.HasBattery, Charging: p.Charging},
		Mode:  a.DeviceMode, Manual: a.ManualDevices, MultiDevice: s.settings.ASR.MultiDevice, PausedAll: s.paused,
	}
	used := map[string]int{}
	for _, id := range runs {
		used[id]++
	}
	for _, d := range s.visibleDevices() {
		test := s.selfTestView(d, now).State == sched.DeviceTestRunning // a running test holds a slot
		u := used[d.row.ID]
		if test {
			u++
		}
		in.Devices = append(in.Devices, devices.PolicyDevice{ID: d.row.ID, Kind: model.DeviceKind(d.row.ID), Backend: d.row.Backend,
			Enabled: d.row.Enabled, Present: d.present, Paused: d.paused, Slots: d.row.MaxParallel, Used: u, SelfTest: test})
	}
	lecs := make([]string, 0, len(runs))
	for lec := range runs {
		lecs = append(lecs, lec)
	}
	slices.Sort(lecs)
	for _, lec := range lecs {
		in.Running = append(in.Running, devices.PolicyRun{JobID: asrJobID(lec), DeviceID: runs[lec], Backend: model.DeviceBackend(runs[lec])})
	}
	return in
}

// policyDevice is in's row of id.
func policyDevice(in devices.PolicyInput, id string) (devices.PolicyDevice, bool) {
	for _, d := range in.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return devices.PolicyDevice{}, false
}

// otherActive is the device that keeps a self-test of id waiting on a
// machine with a battery (devices.SelfTest Start false): the first device
// in priority order other than id with a pipeline asr run, else one whose
// self-test holds a slot; "" when there is none.
func otherActive(in devices.PolicyInput, id string) string {
	busy := map[string]bool{}
	for _, r := range in.Running {
		busy[r.DeviceID] = true
	}
	for _, d := range in.Devices {
		if d.ID != id && busy[d.ID] {
			return d.ID
		}
	}
	for _, d := range in.Devices {
		if d.ID != id && d.SelfTest {
			return d.ID
		}
	}
	return ""
}

// assignRuns puts the running asr jobs on devices the way the scheduler
// admits them: a job keeps its device while that stays enabled and
// present; otherwise devices.Place names the devices it may use (the
// power policy; the 3b pool on a desktop) and the first one that is not
// paused and has a free slot takes it. The global switch is ignored here
// (it stops new starts only, and these jobs already run); a job the policy
// keeps waiting is on no card. Returns lecture id → device id. s.mu held.
func (s *store) assignRuns(now time.Time) map[string]string {
	runs := map[string]string{}
	var todo []string
	for _, l := range s.lectures {
		j := stageIndex(&l, model.StageASR)
		if j < 0 || l.Stages[j].Status != model.StatusRunning {
			continue
		}
		id := s.dev.assigned[l.ID]
		if d, err := s.device(id); id != "" && err == nil && s.usable(d) {
			runs[l.ID] = id
			continue
		}
		todo = append(todo, l.ID)
	}
	for _, lec := range todo {
		in := s.policyInput(now, runs)
		in.PausedAll = false
		for _, id := range devices.Place(in, devices.PolicyJob{JobID: asrJobID(lec)}).Devices {
			if d, ok := policyDevice(in, id); ok && !d.Paused && d.Used < max(d.Slots, 1) {
				runs[lec] = id
				break
			}
		}
	}
	for lec := range s.dev.assigned {
		if _, ok := runs[lec]; !ok {
			delete(s.dev.assigned, lec)
		}
	}
	for lec, id := range runs {
		s.dev.assigned[lec] = id
	}
	return runs
}

// policyView is devices.View of the store with its input. s.mu held.
func (s *store) policyView(now time.Time) (devices.PolicyView, devices.PolicyInput) {
	in := s.policyInput(now, s.assignRuns(now))
	return devices.View(in), in
}

func (s *store) usable(d *mockDevice) bool {
	return d.row.Enabled && d.present
}

// resourcesPolicy is resources.Snapshot.policy (sched.PowerStatus of the
// real back end: the view, the counts of the suspended jobs, the notices
// of the last PowerNoticeAge). s.mu held.
func (s *store) resourcesPolicy(now time.Time) *resources.Policy {
	v, _ := s.policyView(now)
	out := &resources.Policy{BatteryMachine: v.BatteryMachine, Mode: v.Mode, ManualIgnored: v.ManualIgnored, Reason: v.Reason,
		Active: append([]string{}, v.Active...), Next: v.Next, NoneReason: v.NoneReason, Notices: []resources.PowerNotice{}}
	for _, l := range s.lectures {
		j := stageIndex(&l, model.StageASR)
		if j < 0 || l.Stages[j].Status != model.StatusQueued || l.Stages[j].WaitingFor == nil {
			continue
		}
		switch *l.Stages[j].WaitingFor {
		case model.WaitPowerWaitAC:
			out.WaitAC++
		case model.WaitPowerNoSaverDevice, model.WaitPowerNoDevice:
			out.WaitNoDevice++
		}
	}
	cutoff := now.Add(-sched.PowerNoticeAge)
	for _, n := range s.dev.notices {
		if at, err := time.Parse(time.RFC3339, n.At); err == nil && at.After(cutoff) {
			out.Notices = append(out.Notices, n)
		}
	}
	return out
}

// syncPowerWaits keeps the queued asr stages that wait under the single
// active rules of a machine with a battery (power_queue:{id},
// device_selftest:{id}) in step with devices.Place: while a self-test
// holds the active device they wait for the test, afterwards for the
// device again. The mock never starts a queued job, so a placement that
// would start one, or any other answer, leaves the stage as it is.
// s.mu held.
func (s *store) syncPowerWaits(now time.Time) {
	in := s.policyInput(now, s.assignRuns(now))
	if !in.Power.BatteryMachine() {
		return
	}
	single := func(w string) bool {
		return strings.HasPrefix(w, model.WaitPowerQueuePrefix) || strings.HasPrefix(w, model.WaitDeviceSelfTestPrefix)
	}
	for i := range s.lectures {
		l := &s.lectures[i]
		j := stageIndex(l, model.StageASR)
		if j < 0 || l.Stages[j].Status != model.StatusQueued || l.Stages[j].WaitingFor == nil || !single(*l.Stages[j].WaitingFor) {
			continue
		}
		p := devices.Place(in, devices.PolicyJob{JobID: asrJobID(l.ID)})
		if p.Wait == "" || p.Wait == *l.Stages[j].WaitingFor || !single(p.Wait) {
			continue
		}
		w := p.Wait
		l.Stages[j].WaitingFor = &w
		s.changed(l)
	}
}

// holdsSuspended: a power_wait_ac job continues on this device.
func (s *store) holdsSuspended(id string) bool {
	for lec, dev := range s.dev.suspended {
		if dev != id {
			continue
		}
		if l, err := s.findLecture(lec); err == nil && stageStatus(l, model.StageASR) == model.StatusQueued {
			return true
		}
	}
	return false
}

// systemReadings is resources.Snapshot.system (moving slowly with the clock).
func (s *store) systemReadings(now time.Time, busy bool) *resources.System {
	t := float64(now.Unix())
	r := s.dev.sys
	cpu := r.cpuBase + int(math.Round(4*math.Sin(t/7)+2*math.Sin(t/2.9)))
	if busy {
		cpu += 9
	}
	cpu = max(1, min(100, cpu))
	used := 14210 + int(math.Round(180*math.Sin(t/23)))
	if busy {
		used += 2400
	}
	out := &resources.System{CPUPct: &cpu, MemUsedMB: &used, MemTotalMB: intPtr(32456)}
	if r.tempC != nil {
		tc := *r.tempC + int(math.Round(2*math.Sin(t/17)))
		out.CPUTempC = &tc
	}
	return out
}
