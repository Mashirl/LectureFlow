// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"sort"

	"lectureflow/internal/model"
)

// ---------------------------------------------------------------------------
// The power-aware device policy (user requirement 2026-10-24,
// .claude/power-req.md; lead plan .claude/power-plan.md §2). FROZEN by the
// lead for v2-step20 (.claude/power-brief.md §3.3): the types and signatures
// below. Bodies: task P1, golden-tested (power-brief §7, the list of
// requirement part 3). The functions are pure: the scheduler collects a
// PolicyInput once per scan (the 3b scan state) and acts on the answers;
// load-dependent admission (VRAM, system memory) stays in the scheduler.
//
// TERMS
//
//   - Machine with a battery = Power.BatteryMachine() (AC state known and a
//     system battery present). Everything else — a desktop, an unknown power
//     state, a failed reading — is a "desktop" and keeps the phase-3b rules
//     exactly (several devices at once, multi_device, the device list's
//     priority order), with two additions: the manual list (ASRSettings
//     ManualDevices) and the retirement of battery_aware (a desktop never
//     runs on battery). CPU on a desktop: DesktopCPUFallback (Q1).
//   - On battery = Power.OnBatteryPower().
//   - Usable device = Enabled ∧ Present (PolicyDevice); a paused device is
//     usable but takes no job (see "pause" below).
//   - Accelerator = kind cuda, igpu or npu. On a machine with a battery a
//     CPU device (kind cpu: fw:cpu, ov:CPU) is NEVER chosen, on any path,
//     in any mode (requirement 3: "任何情况下都不使用 CPU 转写"); when no
//     accelerator qualifies the jobs wait (power_* codes) instead.
//   - Battery device P_batt = the first usable npu, else the first usable
//     igpu (devices in priority order); none → WaitPowerNoSaverDevice. CUDA
//     is never a battery device. (Requirement 2/3: 电池 → NPU → 核显 → 挂起.)
//   - AC device P_ac = the first usable cuda, else the first usable igpu;
//     none → WaitPowerNoDevice. The NPU is not in the AC chain (only the
//     manual list can choose it on AC). (Requirement 2/3: CUDA → 核显 → 挂起.)
//   - Active set S of a machine with a battery: on battery {P_batt} (the
//     manual list does not apply on battery, requirement 6); on AC with
//     Mode manual: the usable accelerators of Manual in priority order
//     (several may run at once — the user saw the warning); on AC in auto
//     mode: {P_ac}. "Single active" = S has one device because the mode is
//     not manual (or the machine is on battery).
//
// PLACE (one queued pipeline asr job; Placement)
//
// Checked in this order; the first rule that decides wins.
//
//  1. Global pause (in.PausedAll) → WaitGPUPaused, on every machine.
//
//  2. Desktop — the 3b rules (sched planDevices of 3b), unchanged:
//     a fixed snapshot (j.SnapOK ∧ ¬Auto) runs only on its device: missing
//     / disabled / not present → WaitNoASRDevice, paused →
//     WaitDevicePaused(id), else Devices = [id]; an auto (or unparsable)
//     snapshot: the pool is every usable device (Mode auto; CPU devices
//     included as in 3b because DesktopCPUFallback is true) or the usable
//     devices of Manual (Mode manual; never CPU); empty pool →
//     WaitNoASRDevice; every pool device paused → WaitDevicePaused(first);
//     else Devices = the unpaused pool in priority order (the scheduler
//     admits the first that fits; multi_device, self-test, slots, VRAM and
//     memory reasons stay the scheduler's, as in 3b). No power_* code is
//     ever produced on a desktop.
//
//  3. Machine with a battery, fixed snapshot on device D (pinned by "沿用
//     原模型", or a v1.1 snapshot; never rebound, never migrated):
//     D missing / disabled / not present → WaitNoASRDevice; D a CPU device →
//     WaitPowerNoDevice; on battery and D ≠ P_batt (including no P_batt) →
//     WaitPowerWaitAC; otherwise the job may use D only, under the single
//     active rules 5a–5e with D in place of P ({D} the allowed devices).
//     P1 addition: on AC in manual mode with D in S, D is one of the
//     user's concurrent devices: rule 5a with S, a paused D →
//     WaitDevicePaused(D), else Devices = [D] (the 3b checks of the
//     scheduler decide slots), like the other members of S.
//
//  4. Machine with a battery, auto (or unparsable) snapshot: S empty →
//     WaitPowerNoSaverDevice on battery, WaitPowerNoDevice on AC. Then the
//     checkpoint rule (j.Resume = job.stats.asr_resume, set by a power
//     interrupt; P1: checked before "S empty" on battery when no battery
//     device could ever have Resume's backend, so a suspended CUDA job
//     says "等待电源适配器" also on a machine without Intel devices): on
//     battery, Resume.Backend ≠ P_batt's backend →
//     WaitPowerWaitAC (requirement 4b: a CUDA job is never handed to the
//     NPU or the Intel GPU; it continues on its own device once the charger
//     is back); on AC, Resume.DeviceID ∈ S → that device is tried first
//     ("接回外部电源后，在原设备上从检查点继续"); Resume.DeviceID ∉ S → S as
//     usual (the worker continues from the checkpoint only when the backend
//     matches, else it starts the lecture over — power-plan §2).
//
//  5. The single-active rules (machine with a battery; S is {P}, or {D} for
//     a fixed snapshot), in order:
//     a. (also in manual mode) a pipeline asr run on a device X outside the
//     job's allowed devices (S, or {D}) → WaitPowerSwitchPending(X, P)
//     (X = the highest-priority such device): X finishes its current
//     lecture first; plugging in never interrupts a run (requirement 4).
//     b. P paused (its own switch) → WaitDevicePaused(P). A pause is not a
//     fallback trigger: the preferred device stays the active one and the
//     job waits for it (as 3b C decision 3); the fallback chain only skips
//     devices that are missing (not present), disabled, or of the wrong
//     kind ("不存在、被用户停用或不可用").
//     c. a self-test holds P (PolicyDevice.SelfTest) → WaitDeviceSelfTest(P).
//     d. P busy — Used ≥ Slots, or ¬MultiDevice and any pipeline asr run
//     exists → WaitPowerQueue(P). The job is never given to another
//     device (requirement 5).
//     e. Devices = [P].
//     Manual mode on AC (S may hold several devices): rule 5a with S, then
//     the paused members are skipped (all paused → WaitDevicePaused(first
//     of S)) and Devices = the unpaused members of S in priority order
//     (Resume device first); slots, multi_device and the rest are the
//     scheduler's 3b checks.
//
//  Placement.Resume: j.Resume ≠ nil and Devices[0] has Resume.Backend (the
//  worker will continue from the checkpoint; informational — the worker
//  decides by the file). Placement.Bind: the snapshot is auto (j.SnapOK ∧
//  Auto) — the scheduler binds Devices[k] at start (3b lead ruling 1);
//  never for a fixed or unparsable snapshot.
//
//  Starvation barrier (lead ruling 9.3, SwitchHold): a fixed snapshot on a
//  device outside S could wait behind a continuous stream of auto jobs
//  when P has two slots or more (rule 5a keeps it waiting while P runs).
//  On a machine with a battery, once an earlier job of the scan's FIFO
//  order waits power_switch_pending, every later job that Place would
//  start waits with the same code, so the running device drains and the
//  switch happens. The scheduler applies SwitchHold in its scan order.
//
// POWER CHANGE (OnPowerChange; every running pipeline asr job)
//
//   - Not a change from AC (or unknown) to battery on a machine with a
//     battery — battery → AC, AC → AC, any desktop reading: every run
//     continues (requirement 4: plugging in never interrupts; the next
//     lecture follows the new state through Place; jobs waiting
//     power_wait_ac become placeable).
//   - AC → battery, per run on device d (P_batt computed with in.Power, the
//     new reading):
//     d = P_batt → PowerContinue;
//     the run is not fixed, P_batt exists and backend(d) = backend(P_batt)
//     (e.g. Intel GPU → NPU) → PowerMigrate, To = P_batt: the run is
//     killed now, requeued, and continues on P_batt from its checkpoint
//     (notice power.switched);
//     otherwise (CUDA; a fixed run off P_batt; no P_batt) → PowerSuspend:
//     killed now, requeued, waits WaitPowerWaitAC, continues on its own
//     device once on AC again (notice power.suspended; power.resumed when
//     it starts again).
//   - A run on a CPU device when the new reading is a machine with a
//     battery (backend Gate round 1, fix 7; it can only have started while
//     the machine counted as a desktop — unknown reading, no battery): the
//     CPU is never used there, so the run is suspended whatever the
//     transition (also unknown → AC, or AC → AC). The one exception is the
//     unplug rule above: an auto ov:CPU run migrates to an OpenVINO P_batt
//     (requirement 4a, same backend) instead of being suspended. The
//     scheduler writes no "已拔掉电源" event or notice for a suspension on
//     AC (NOTES "决定（电源管理后端 Gate 修复）").
//
// SELF-TEST (SelfTest)
//
//   - Desktop: Allowed and Start (the 3b conditions of the scheduler apply
//     on top: pause, pipeline work on the device or waiting for it).
//   - Machine with a battery: a CPU device is never Allowed, in any power
//     state (it is never used for transcription there; backend Gate round
//     1, fix 7; sched's selftest.power_cpu). On battery only P_batt may be tested — another device is
//     not Allowed (StartDeviceTest refuses it with sched's
//     selftest.power_battery; a waiting or running test of it is
//     cancelled, also when the charger is pulled during the test). Start
//     (any power state) only when no pipeline asr run is on another device
//     and no other device's self-test holds a slot ("自测只在没有其他设备活跃
//     时开始"); a running test of a device yields at once (the scheduler's
//     3b preemption) when a pipeline job starts on another device.
//
// VIEW (View; the resource bar's middle area, power-req part 2)
//
//   - Desktop: Active = the auto pool (every usable device, CPU as in 3b) or
//     the usable devices of Manual, priority order, paused ones included;
//     Reason PolicyReasonDesktop (auto) or PolicyReasonManual; empty →
//     NoneReason WaitNoASRDevice.
//   - Machine with a battery: with pipeline asr runs, Active = their devices
//     (priority order) and Next = S's first device when a running device is
//     outside S (switch pending); without runs, Active = S (a paused P
//     stays active; the card shows the pause). S empty and no run →
//     NoneReason WaitPowerNoSaverDevice (on battery) / WaitPowerNoDevice
//     (AC). Reason: PolicyReasonBattery on battery, PolicyReasonManual on
//     AC with a manual list, else PolicyReasonACAuto. Mode is the
//     effective mode ("manual" only on AC); ManualIgnored = a manual list
//     is set but the machine runs on battery ("使用电池时不生效").
// ---------------------------------------------------------------------------

// DesktopCPUFallback is Q1 (user, 2026-10-24): on a machine WITHOUT a
// battery, CPU devices keep their phase-3b role — fw:cpu is the fallback
// (enabled only when the database has no accelerator row, devices.Detect)
// and a CPU device the user enabled is a candidate in priority order. On a
// machine with a battery the CPU is never used, whatever this says. The
// manual list never holds a CPU device on any machine. false would remove
// CPU devices from the desktop pool too (then: WaitNoASRDevice).
const DesktopCPUFallback = true

// What OnPowerChange does with a running job (RunAction.Action).
const (
	PowerContinue = "continue" // keep running
	PowerMigrate  = "migrate"  // kill, requeue, continue on RunAction.To from the checkpoint
	PowerSuspend  = "suspend"  // kill, requeue, wait power_wait_ac (resume on its own device on AC)
)

// Why the active devices are active (PolicyView.Reason; the card's "为什么
// 是它", front end resources.power.reason.{code}, task P5).
const (
	PolicyReasonACAuto  = "ac_auto" // 外接电源 · 自动选择
	PolicyReasonBattery = "battery" // 电池 · 电源感知
	PolicyReasonManual  = "manual"  // 手动指定
	PolicyReasonDesktop = "desktop" // 台式机 · 自动
)

// PolicyDevice is one asr_device row as the policy sees it.
type PolicyDevice struct {
	ID      string // model.Device*
	Kind    string // model.DeviceKind(ID)
	Backend string // model.Backend*
	Enabled bool
	Present bool // Registry.Present (before the first detection: a row with detected_at)
	Paused  bool // the device's own pause switch (the global one is PolicyInput.PausedAll)
	Slots   int  // the scheduler's slot count (max_parallel; 1 in fake ASR mode)
	Used    int  // pipeline asr runs on it + a self-test holding a slot
	// SelfTest: a self-test holds a slot of the device now
	// (device_selftest:{id} for pipeline jobs).
	SelfTest bool
}

// PolicyRun is one running pipeline asr job.
type PolicyRun struct {
	JobID    string
	DeviceID string // the device it runs on
	Backend  string
	Fixed    bool // its snapshot is fixed (auto = false): it never migrates
}

// PolicyInput is the state one decision is made on (one per scan).
type PolicyInput struct {
	Power Power // the scan's reading (the new one for OnPowerChange)
	// Mode and Manual are the ASR settings after
	// model.NormalizeASRDeviceMode: auto, or manual with a non-empty list.
	Mode        string
	Manual      []string
	MultiDevice bool // settings multi_device
	PausedAll   bool // the global switch (gpu_paused)
	// Devices are every row, priority DESC then id (Registry.List order).
	Devices []PolicyDevice
	// Running are the pipeline asr runs now (self-tests are in
	// PolicyDevice.SelfTest).
	Running []PolicyRun
}

// PolicyJob is one queued pipeline asr job.
type PolicyJob struct {
	JobID string
	// Snap is the job's snapshot after model.NormalizeASRSnapshot; SnapOK
	// false = missing or unparsable (placed like auto, never bound; the
	// stage fails it, v1.1 decision 4).
	Snap   model.ASRSnapshot
	SnapOK bool
	// Resume is job.stats.asr_resume (nil = no power interrupt).
	Resume *model.ASRResume
}

// Placement is Place's answer: the devices to try (in order) or why the
// job waits.
type Placement struct {
	// Devices are the device ids the scheduler may start the job on, in the
	// order to try (it admits the first that fits by its own checks: self-
	// test, multi_device, slots, VRAM, memory — 3b). Empty when Wait is set.
	// On a machine with a battery outside manual mode it has one entry.
	Devices []string
	// Wait is the waiting_for when Devices is empty: model.Wait* (3b codes
	// and the power_* codes).
	Wait string
	// Resume: the job has a power checkpoint (PolicyJob.Resume) and
	// Devices[0] has its backend, so the worker continues from it.
	Resume bool
	// Bind: an auto snapshot — the chosen device is bound at start
	// (state.BindASRDevice in the queued → running transaction).
	Bind bool
}

// RunAction is what happens to one running job on a power change.
type RunAction struct {
	JobID  string
	Action string // PowerContinue | PowerMigrate | PowerSuspend
	From   string // the device it runs on
	To     string // PowerMigrate: the device it continues on (P_batt); "" otherwise
}

// SelfTestDecision is SelfTest's answer for one device.
type SelfTestDecision struct {
	// Allowed false: the power policy does not allow testing this device
	// now (on battery, not the battery device; a CPU device on a machine
	// with a battery in any power state): StartDeviceTest refuses
	// (selftest.power_battery; selftest.power_cpu for the CPU device) and a
	// waiting or running test is cancelled.
	Allowed bool
	// Start: Allowed, and no other device is active, so a waiting test may
	// take its slot now (the scheduler's 3b conditions still apply).
	Start bool
}

// PolicyView is the policy's state for the resource bar (resources.Policy,
// task P4) and the settings page.
type PolicyView struct {
	BatteryMachine bool
	OnBattery      bool
	// Mode is the effective mode: model.ASRDeviceModeManual only when a
	// manual list is set and it applies (desktop, or AC power), else auto.
	Mode string
	// ManualIgnored: a manual list is set but the machine with a battery
	// runs on battery (requirement 6: "使用电池时不生效").
	ManualIgnored bool
	Reason        string   // PolicyReason*
	Active        []string // device ids of the middle area, priority order; never nil
	// Next is the device that becomes active once the running one finished
	// its lecture (a switch is pending); "" otherwise.
	Next string
	// NoneReason is why Active is empty (a model.Wait* code); "" when not.
	NoneReason string
}

// Place decides where a queued pipeline asr job may start (package
// comment: PLACE). Pure; never reads the database.
func Place(in PolicyInput, j PolicyJob) Placement {
	in = withoutRemote(in)
	if in.PausedAll {
		return Placement{Wait: model.WaitGPUPaused}
	}
	var p Placement
	if in.Power.BatteryMachine() {
		p = placeBattery(in, j)
	} else {
		p = placeDesktop(in, j)
	}
	if len(p.Devices) == 0 {
		return Placement{Wait: p.Wait}
	}
	p.Wait = ""
	p.Bind = j.SnapOK && j.Snap.Auto
	if j.Resume != nil {
		if d, ok := in.device(p.Devices[0]); ok && d.backend() == resumeBackend(j.Resume) {
			p.Resume = true
		}
	}
	return p
}

// SwitchHold is lead ruling 9.3 (power-brief §9.3, the barrier against the
// starvation of rule 5a): on a machine with a battery, once an earlier
// queued job of the same scan waits power_switch_pending (earlier is its
// waiting_for), a later job does not start on the device that is being
// drained either: it waits with the same code, so the active device runs
// empty and the switch happens. Only the scheduler knows the FIFO order of
// a scan, so it applies this to Place's answer for every later job. Any
// other machine, an earlier value that is not a switch, or a placement
// that already waits: p unchanged. Pure.
func SwitchHold(in PolicyInput, earlier string, p Placement) Placement {
	if !in.Power.BatteryMachine() || len(p.Devices) == 0 {
		return p
	}
	if _, _, ok := model.ParseWaitPowerSwitchPending(earlier); !ok {
		return p
	}
	return Placement{Wait: earlier}
}

// BatteryDevice returns P_batt (package comment: TERMS): the first usable
// NPU, else the first usable Intel GPU, in priority order; ok false when
// there is none. It does not look at the power state. Pure.
func BatteryDevice(in PolicyInput) (id string, ok bool) {
	d, ok := batteryDevice(in)
	return d.ID, ok
}

// OnPowerChange returns one RunAction per in.Running entry (same order) for
// a change of the power reading from prev to in.Power (package comment:
// POWER CHANGE). Pure.
func OnPowerChange(prev Power, in PolicyInput) []RunAction {
	out := make([]RunAction, len(in.Running))
	unplugged := in.Power.OnBatteryPower() && !prev.OnBatteryPower()
	battery := in.Power.BatteryMachine()
	pb, hasPB := batteryDevice(in)
	for i, r := range in.Running {
		a := RunAction{JobID: r.JobID, Action: PowerContinue, From: r.DeviceID}
		if runBackend(r) == model.BackendColab {
			out[i] = a // SPEC-v3 V14.4: a remote run uses no local power
			continue
		}
		if unplugged {
			switch {
			case hasPB && r.DeviceID == pb.ID:
				// already on the battery device
			case !r.Fixed && hasPB && runBackend(r) == pb.backend():
				a.Action, a.To = PowerMigrate, pb.ID
			default:
				a.Action = PowerSuspend
			}
		}
		if battery && a.Action == PowerContinue && runKind(in, r) == model.DeviceKindCPU {
			// Never the CPU on a machine with a battery (fix 7): also when the
			// reading turned from unknown / no battery into one on AC.
			a.Action = PowerSuspend
		}
		out[i] = a
	}
	return out
}

// SelfTest decides whether a self-test of device id may run / start now
// (package comment: SELF-TEST). Pure.
func SelfTest(in PolicyInput, id string) SelfTestDecision {
	in = withoutRemote(in)
	if !in.Power.BatteryMachine() {
		return SelfTestDecision{Allowed: true, Start: true}
	}
	kind := model.DeviceKind(id)
	if d, ok := in.device(id); ok {
		kind = d.kind()
	}
	if kind == model.DeviceKindCPU {
		// The CPU is never used on a machine with a battery (fix 7).
		return SelfTestDecision{}
	}
	if in.Power.OnBatteryPower() {
		if pb, ok := batteryDevice(in); !ok || pb.ID != id {
			return SelfTestDecision{}
		}
	}
	start := true
	for _, r := range in.Running {
		if r.DeviceID != id {
			start = false
		}
	}
	for _, d := range in.Devices {
		if d.ID != id && d.SelfTest {
			start = false
		}
	}
	return SelfTestDecision{Allowed: true, Start: start}
}

// View is the policy's state for the resource bar (package comment: VIEW).
// Pure.
func View(in PolicyInput) PolicyView {
	in = withoutRemote(in)
	v := PolicyView{BatteryMachine: in.Power.BatteryMachine(), OnBattery: in.Power.OnBatteryPower(),
		Mode: model.ASRDeviceModeAuto, Active: []string{}}
	list := manualList(in)
	if !v.BatteryMachine {
		v.Reason = PolicyReasonDesktop
		if list != nil {
			v.Mode, v.Reason = model.ASRDeviceModeManual, PolicyReasonManual
		}
		for _, d := range desktopPool(in, list) {
			v.Active = append(v.Active, d.ID)
		}
		if len(v.Active) == 0 {
			v.NoneReason = model.WaitNoASRDevice
		}
		return v
	}
	s, manual := activeSet(in)
	switch {
	case v.OnBattery:
		v.Reason, v.ManualIgnored = PolicyReasonBattery, list != nil
	case manual:
		v.Mode, v.Reason = model.ASRDeviceModeManual, PolicyReasonManual
	default:
		v.Reason = PolicyReasonACAuto
	}
	if len(in.Running) == 0 {
		v.Active = ids(s)
		if len(v.Active) == 0 {
			v.NoneReason = model.WaitPowerNoDevice
			if v.OnBattery {
				v.NoneReason = model.WaitPowerNoSaverDevice
			}
		}
		return v
	}
	inS := map[string]bool{}
	for _, d := range s {
		inS[d.ID] = true
	}
	outside := false
	for _, id := range runDevices(in) {
		v.Active = append(v.Active, id)
		outside = outside || !inS[id]
	}
	if outside && len(s) > 0 {
		v.Next = s[0].ID
	}
	return v
}

// ---------- helpers ----------

// withoutRemote drops the remote Colab device and its runs (SPEC-v3 V14.4:
// colab:remote uses neither the machine's power nor its cooling, so it is
// never part of the power-aware policy; the scheduler dispatches to it on
// its own, sched/remote.go).
func withoutRemote(in PolicyInput) PolicyInput {
	remote := func(id, backend string) bool {
		return backend == model.BackendColab || model.DeviceBackend(id) == model.BackendColab
	}
	has := false
	for _, d := range in.Devices {
		has = has || remote(d.ID, d.Backend)
	}
	for _, r := range in.Running {
		has = has || remote(r.DeviceID, r.Backend)
	}
	if !has {
		return in
	}
	out := in
	out.Devices, out.Running = nil, nil
	for _, d := range in.Devices {
		if !remote(d.ID, d.Backend) {
			out.Devices = append(out.Devices, d)
		}
	}
	for _, r := range in.Running {
		if !remote(r.DeviceID, r.Backend) {
			out.Running = append(out.Running, r)
		}
	}
	return out
}

// isAcceleratorKind reports whether a kind is a local accelerator (cuda,
// igpu, npu, apple).
func isAcceleratorKind(kind string) bool {
	switch kind {
	case model.DeviceKindCUDA, model.DeviceKindIGPU, model.DeviceKindNPU, model.DeviceKindApple:
		return true
	}
	return false
}

func (d PolicyDevice) usable() bool { return d.Enabled && d.Present }

func (d PolicyDevice) kind() string {
	if d.Kind != "" {
		return d.Kind
	}
	return model.DeviceKind(d.ID)
}

func (d PolicyDevice) backend() string {
	if d.Backend != "" {
		return d.Backend
	}
	return model.DeviceBackend(d.ID)
}

func runBackend(r PolicyRun) string {
	if r.Backend != "" {
		return r.Backend
	}
	return model.DeviceBackend(r.DeviceID)
}

// runKind is the kind of a run's device (its row's, else by id).
func runKind(in PolicyInput, r PolicyRun) string {
	if d, ok := in.device(r.DeviceID); ok {
		return d.kind()
	}
	return model.DeviceKind(r.DeviceID)
}

func resumeBackend(r *model.ASRResume) string {
	if r.Backend != "" {
		return r.Backend
	}
	return model.DeviceBackend(r.DeviceID)
}

func (in PolicyInput) device(id string) (PolicyDevice, bool) {
	for _, d := range in.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return PolicyDevice{}, false
}

// firstUsable is the first usable device of the first kind that has one
// (devices in priority order).
func firstUsable(in PolicyInput, kinds ...string) (PolicyDevice, bool) {
	for _, k := range kinds {
		for _, d := range in.Devices {
			if d.usable() && d.kind() == k {
				return d, true
			}
		}
	}
	return PolicyDevice{}, false
}

// batteryDevice is P_batt: NPU → Intel GPU → Apple GPU (never CUDA, never
// a CPU; SPEC-v3 V5: a Mac on battery keeps using mlx:gpu).
func batteryDevice(in PolicyInput) (PolicyDevice, bool) {
	return firstUsable(in, model.DeviceKindNPU, model.DeviceKindIGPU, model.DeviceKindApple)
}

// acDevice is P_ac: CUDA → Intel GPU → Apple GPU (the NPU only through the
// manual list).
func acDevice(in PolicyInput) (PolicyDevice, bool) {
	return firstUsable(in, model.DeviceKindCUDA, model.DeviceKindIGPU, model.DeviceKindApple)
}

// manualList is the manual list in effect (model.NormalizeASRDeviceMode;
// idempotent, so the caller need not have normalized), nil in auto mode.
func manualList(in PolicyInput) []string {
	a := model.NormalizeASRDeviceMode(model.ASRSettings{DeviceMode: in.Mode, ManualDevices: in.Manual})
	if a.DeviceMode != model.ASRDeviceModeManual {
		return nil
	}
	return a.ManualDevices
}

func contains(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func ids(ds []PolicyDevice) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

// activeSet is S of a machine with a battery (package comment: TERMS) in
// priority order; manual reports that the manual list applies (AC power).
func activeSet(in PolicyInput) (s []PolicyDevice, manual bool) {
	if in.Power.OnBatteryPower() {
		if p, ok := batteryDevice(in); ok {
			return []PolicyDevice{p}, false
		}
		return nil, false
	}
	if list := manualList(in); list != nil {
		for _, d := range in.Devices {
			if d.usable() && isAcceleratorKind(d.kind()) && contains(list, d.ID) {
				s = append(s, d)
			}
		}
		return s, true
	}
	if p, ok := acDevice(in); ok {
		return []PolicyDevice{p}, false
	}
	return nil, false
}

// desktopPool is the 3b candidate pool of an auto snapshot on a desktop:
// every usable device (CPU devices as DesktopCPUFallback says), or the
// usable devices of the manual list (never a CPU device); priority order,
// paused devices included.
func desktopPool(in PolicyInput, list []string) []PolicyDevice {
	var pool []PolicyDevice
	for _, d := range in.Devices {
		if !d.usable() {
			continue
		}
		cpu := d.kind() == model.DeviceKindCPU
		switch {
		case list != nil:
			if !cpu && contains(list, d.ID) {
				pool = append(pool, d)
			}
		case !cpu || DesktopCPUFallback:
			pool = append(pool, d)
		}
	}
	return pool
}

// runDevices are the devices with pipeline asr runs, priority order (runs
// on a device without a row last, by id).
func runDevices(in PolicyInput) []string {
	busy := map[string]bool{}
	for _, r := range in.Running {
		busy[r.DeviceID] = true
	}
	var out []string
	for _, d := range in.Devices {
		if busy[d.ID] {
			out = append(out, d.ID)
			delete(busy, d.ID)
		}
	}
	var rest []string
	for id := range busy {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// runOutside is the highest-priority device outside allowed with a
// pipeline asr run (rule 5a).
func runOutside(in PolicyInput, allowed []string) (string, bool) {
	for _, id := range runDevices(in) {
		if !contains(allowed, id) {
			return id, true
		}
	}
	return "", false
}

func wait(code string) Placement { return Placement{Wait: code} }

func fixedSnap(j PolicyJob) bool { return j.SnapOK && !j.Snap.Auto }

// placeDesktop is rule 2: the phase-3b rules (sched planDevices of 3b).
func placeDesktop(in PolicyInput, j PolicyJob) Placement {
	if fixedSnap(j) {
		d, ok := in.device(j.Snap.DeviceID)
		switch {
		case !ok || !d.usable():
			return wait(model.WaitNoASRDevice)
		case d.Paused:
			return wait(model.WaitDevicePaused(d.ID))
		}
		return Placement{Devices: []string{d.ID}}
	}
	pool := desktopPool(in, manualList(in))
	if len(pool) == 0 {
		return wait(model.WaitNoASRDevice)
	}
	var out []string
	for _, d := range pool {
		if !d.Paused {
			out = append(out, d.ID)
		}
	}
	if len(out) == 0 {
		return wait(model.WaitDevicePaused(pool[0].ID))
	}
	return Placement{Devices: out}
}

// saverBackend reports whether b is a backend a battery device (NPU,
// Intel GPU) can have.
func saverBackend(b string) bool {
	return b == model.DeviceBackend(model.DeviceOVNPU) || b == model.DeviceBackend(model.DeviceOVGPU)
}

// placeBattery is rules 3–5 (machine with a battery).
func placeBattery(in PolicyInput, j PolicyJob) Placement {
	onBattery := in.Power.OnBatteryPower()
	s, manual := activeSet(in)
	if fixedSnap(j) {
		d, ok := in.device(j.Snap.DeviceID)
		switch {
		case !ok || !d.usable():
			return wait(model.WaitNoASRDevice)
		case d.kind() == model.DeviceKindCPU:
			return wait(model.WaitPowerNoDevice)
		}
		if onBattery {
			if pb, ok := batteryDevice(in); !ok || pb.ID != d.ID {
				return wait(model.WaitPowerWaitAC)
			}
		}
		if manual && contains(ids(s), d.ID) {
			// A pinned job on a device of the manual list runs next to the
			// list's other devices (NOTES "决定（电源管理 P1）").
			if x, busy := runOutside(in, ids(s)); busy {
				return wait(model.WaitPowerSwitchPending(x, d.ID))
			}
			if d.Paused {
				return wait(model.WaitDevicePaused(d.ID))
			}
			return Placement{Devices: []string{d.ID}}
		}
		return single(in, d)
	}
	if onBattery && j.Resume != nil {
		// Rule 4 (requirement 4b): a checkpoint of a backend the battery
		// device does not have waits for the charger, never moves to it.
		rb := resumeBackend(j.Resume)
		if (len(s) > 0 && rb != s[0].backend()) || (len(s) == 0 && !saverBackend(rb)) {
			return wait(model.WaitPowerWaitAC)
		}
	}
	if len(s) == 0 {
		if onBattery {
			return wait(model.WaitPowerNoSaverDevice)
		}
		return wait(model.WaitPowerNoDevice)
	}
	if !onBattery && j.Resume != nil {
		// "接回外部电源后，在原设备上从检查点继续": its device first.
		for i, d := range s {
			if d.ID == j.Resume.DeviceID && i > 0 {
				ordered := append([]PolicyDevice{d}, s[:i]...)
				s = append(ordered, s[i+1:]...)
				break
			}
		}
	}
	if manual {
		if x, busy := runOutside(in, ids(s)); busy {
			return wait(model.WaitPowerSwitchPending(x, s[0].ID))
		}
		var out []string
		for _, d := range s {
			if !d.Paused {
				out = append(out, d.ID)
			}
		}
		if len(out) == 0 {
			return wait(model.WaitDevicePaused(s[0].ID))
		}
		return Placement{Devices: out}
	}
	return single(in, s[0])
}

// single is rules 5a–5e for the one device p the job may use.
func single(in PolicyInput, p PolicyDevice) Placement {
	if x, busy := runOutside(in, []string{p.ID}); busy {
		return wait(model.WaitPowerSwitchPending(x, p.ID))
	}
	switch {
	case p.Paused:
		return wait(model.WaitDevicePaused(p.ID))
	case p.SelfTest:
		return wait(model.WaitDeviceSelfTest(p.ID))
	case p.Used >= max(p.Slots, 1), !in.MultiDevice && len(in.Running) > 0:
		return wait(model.WaitPowerQueue(p.ID))
	}
	return Placement{Devices: []string{p.ID}}
}
