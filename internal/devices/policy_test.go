// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// Golden tests of the power-aware device policy (power-req.md part 3,
// power-brief §7 P1): table driven, every case states its input and the
// exact expected answer (P1 chose tables over a JSON golden file so that a
// change of the rules shows up as a reviewed edit of an expectation, never
// as a regenerated file; NOTES "决定（电源管理 P1）").

import (
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// ---------- builders ----------

var (
	pwDesktop = Power{Known: true}                                    // no battery, AC
	pwUnknown = Power{}                                               // unknown: a desktop
	pwAC      = Power{Known: true, HasBattery: true}                  // laptop on AC
	pwBattery = Power{Known: true, HasBattery: true, OnBattery: true} // laptop on battery
)

type devOpt func(*PolicyDevice)

func pdev(id string, opts ...devOpt) PolicyDevice {
	d := PolicyDevice{ID: id, Kind: model.DeviceKind(id), Backend: model.DeviceBackend(id), Enabled: true, Present: true, Slots: 1}
	for _, o := range opts {
		o(&d)
	}
	return d
}

func off(d *PolicyDevice)      { d.Enabled = false }
func gone(d *PolicyDevice)     { d.Present = false }
func held(d *PolicyDevice)     { d.Paused = true }
func testing1(d *PolicyDevice) { d.SelfTest, d.Used = true, d.Used+1 }
func slots(n int) devOpt       { return func(d *PolicyDevice) { d.Slots = n } }
func used(n int) devOpt        { return func(d *PolicyDevice) { d.Used = n } }

// The five devices in their default priority order (cuda:0 300, ov:GPU
// 200, ov:NPU 100, fw:cpu, ov:CPU).
func allDevs() []PolicyDevice {
	return []PolicyDevice{pdev(model.DeviceCUDA0), pdev(model.DeviceOVGPU), pdev(model.DeviceOVNPU),
		pdev(model.DeviceFWCPU), pdev(model.DeviceOVCPU)}
}

func pin(p Power, ds ...PolicyDevice) PolicyInput {
	// A copy: running() changes the devices' Used counts.
	return PolicyInput{Power: p, Mode: model.ASRDeviceModeAuto, MultiDevice: true, Devices: append([]PolicyDevice(nil), ds...)}
}

func (in PolicyInput) manual(ids ...string) PolicyInput {
	in.Mode, in.Manual = model.ASRDeviceModeManual, ids
	return in
}

func (in PolicyInput) running(runs ...PolicyRun) PolicyInput {
	in.Running = runs
	in.Devices = append([]PolicyDevice(nil), in.Devices...)
	// A run takes a slot of its device.
	for _, r := range runs {
		for i := range in.Devices {
			if in.Devices[i].ID == r.DeviceID {
				in.Devices[i].Used++
			}
		}
	}
	return in
}

func runOn(id string) PolicyRun {
	return PolicyRun{JobID: "run-" + id, DeviceID: id, Backend: model.DeviceBackend(id)}
}

func fixedRunOn(id string) PolicyRun {
	r := runOn(id)
	r.Fixed = true
	return r
}

func snapOn(id string, auto bool) model.ASRSnapshot {
	return model.ASRSnapshot{DeviceID: id, Backend: model.DeviceBackend(id), Model: "m", Precision: "p", Language: "en", Auto: auto}
}

var (
	jAuto = PolicyJob{JobID: "j", Snap: snapOn(model.DeviceCUDA0, true), SnapOK: true}
	jBad  = PolicyJob{JobID: "j"} // missing or unparsable snapshot
)

func jFixed(id string) PolicyJob { return PolicyJob{JobID: "j", Snap: snapOn(id, false), SnapOK: true} }

func (j PolicyJob) resume(id string) PolicyJob {
	j.Resume = &model.ASRResume{DeviceID: id, Backend: model.DeviceBackend(id), At: "2026-10-24T08:00:00Z"}
	return j
}

func on(ids ...string) Placement      { return Placement{Devices: ids, Bind: true} }
func onFixed(ids ...string) Placement { return Placement{Devices: ids} }
func waits(code string) Placement     { return Placement{Wait: code} }

func (p Placement) resumed() Placement {
	p.Resume = true
	return p
}

func without(ds []PolicyDevice, drop ...string) []PolicyDevice {
	var out []PolicyDevice
	for _, d := range ds {
		if !contains(drop, d.ID) {
			out = append(out, d)
		}
	}
	return out
}

func with(ds []PolicyDevice, id string, opts ...devOpt) []PolicyDevice {
	out := append([]PolicyDevice(nil), ds...)
	for i := range out {
		if out[i].ID == id {
			for _, o := range opts {
				o(&out[i])
			}
		}
	}
	return out
}

type placeCase struct {
	name string
	in   PolicyInput
	j    PolicyJob
	want Placement
}

func runPlace(t *testing.T, cases []placeCase) {
	t.Helper()
	for _, c := range cases {
		got := Place(c.in, c.j)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
	}
}

const (
	cuda = model.DeviceCUDA0
	igpu = model.DeviceOVGPU
	npu  = model.DeviceOVNPU
	fwc  = model.DeviceFWCPU
	ovc  = model.DeviceOVCPU
)

// ---------- requirement part 3: the three main cases ----------

func TestPolicyMainCases(t *testing.T) {
	intelOnly := without(allDevs(), cuda)
	runPlace(t, []placeCase{
		{"AC + CUDA → CUDA", pin(pwAC, allDevs()...), jAuto, on(cuda)},
		{"AC + only OpenVINO → Intel GPU", pin(pwAC, intelOnly...), jAuto, on(igpu)},
		{"battery → NPU", pin(pwBattery, allDevs()...), jAuto, on(npu)},
		{"battery, Intel only → NPU", pin(pwBattery, intelOnly...), jAuto, on(npu)},
		{"unparsable snapshot: placed like auto, never bound", pin(pwBattery, allDevs()...), jBad, onFixed(npu)},
	})
}

// ---------- every fallback path, suspension included ----------

func TestPolicyFallbacks(t *testing.T) {
	all := allDevs()
	runPlace(t, []placeCase{
		// AC: CUDA → Intel GPU → suspend
		{"AC, CUDA not present → Intel GPU", pin(pwAC, with(all, cuda, gone)...), jAuto, on(igpu)},
		{"AC, CUDA disabled → Intel GPU", pin(pwAC, with(all, cuda, off)...), jAuto, on(igpu)},
		{"AC, CUDA row missing → Intel GPU", pin(pwAC, without(all, cuda)...), jAuto, on(igpu)},
		{"AC, only NPU (+ CPU): the NPU is not in the AC chain", pin(pwAC, without(all, cuda, igpu)...), jAuto, waits(model.WaitPowerNoDevice)},
		{"AC, only CPU devices", pin(pwAC, without(all, cuda, igpu, npu)...), jAuto, waits(model.WaitPowerNoDevice)},
		{"AC, nothing usable", pin(pwAC, pdev(cuda, off), pdev(igpu, gone)), jAuto, waits(model.WaitPowerNoDevice)},
		{"AC, no rows", pin(pwAC), jAuto, waits(model.WaitPowerNoDevice)},
		// battery: NPU → Intel GPU → suspend; never CUDA
		{"battery, no NPU → Intel GPU", pin(pwBattery, without(all, npu)...), jAuto, on(igpu)},
		{"battery, NPU disabled → Intel GPU", pin(pwBattery, with(all, npu, off)...), jAuto, on(igpu)},
		{"battery, NPU not present → Intel GPU", pin(pwBattery, with(all, npu, gone)...), jAuto, on(igpu)},
		{"battery, only CUDA (+ CPU): CUDA is never a battery device", pin(pwBattery, without(all, igpu, npu)...), jAuto, waits(model.WaitPowerNoSaverDevice)},
		{"battery, only CUDA", pin(pwBattery, pdev(cuda)), jAuto, waits(model.WaitPowerNoSaverDevice)},
		{"battery, only CPU devices", pin(pwBattery, pdev(fwc), pdev(ovc)), jAuto, waits(model.WaitPowerNoSaverDevice)},
		{"battery, Intel devices unusable", pin(pwBattery, with(with(all, npu, off), igpu, gone)...), jAuto, waits(model.WaitPowerNoSaverDevice)},
		// a priority change does not change the chains
		{"AC, Intel GPU dragged first: still CUDA", pin(pwAC, pdev(igpu), pdev(npu), pdev(cuda)), jAuto, on(cuda)},
		{"battery, Intel GPU dragged first: still NPU", pin(pwBattery, pdev(igpu), pdev(cuda), pdev(npu)), jAuto, on(npu)},
	})
}

// ---------- the CPU is never chosen on a machine with a battery ----------

// Exhaustive over the five devices (each: no row, usable, usable and
// paused, disabled, not present) × AC / battery × auto / manual (with CPU
// devices in the list) / a list of CPU devices only × every snapshot (auto,
// unparsable, fixed on each device) × checkpoint (none, faster-whisper,
// OpenVINO) × where a pipeline job runs (nowhere, on each device).
func TestPolicyNeverCPUOnBatteryMachines(t *testing.T) {
	ids5 := []string{cuda, igpu, npu, fwc, ovc}
	type state int
	const (
		noRow state = iota
		usable
		pausedS
		disabledS
		absentS
		nStates
	)
	snaps := []PolicyJob{jAuto, jBad}
	for _, id := range ids5 {
		snaps = append(snaps, jFixed(id))
	}
	modes := []func(PolicyInput) PolicyInput{
		func(in PolicyInput) PolicyInput { return in },
		func(in PolicyInput) PolicyInput { return in.manual(ids5...) },
		func(in PolicyInput) PolicyInput { return in.manual(fwc, ovc) },
	}
	resumes := []string{"", cuda, igpu}
	isCPU := func(id string) bool { return model.DeviceKind(id) == model.DeviceKindCPU }
	n := 0
	combos := 1
	for range ids5 {
		combos *= int(nStates)
	}
	for c := 0; c < combos; c++ {
		var ds []PolicyDevice
		x := c
		for _, id := range ids5 {
			switch state(x % int(nStates)) {
			case usable:
				ds = append(ds, pdev(id))
			case pausedS:
				ds = append(ds, pdev(id, held))
			case disabledS:
				ds = append(ds, pdev(id, off))
			case absentS:
				ds = append(ds, pdev(id, gone))
			}
			x /= int(nStates)
		}
		for _, pw := range []Power{pwAC, pwBattery} {
			for _, mode := range modes {
				base := mode(pin(pw, ds...))
				runSets := [][]PolicyRun{nil}
				for _, d := range ds {
					if d.usable() {
						runSets = append(runSets, []PolicyRun{runOn(d.ID)})
					}
				}
				for _, runs := range runSets {
					in := base.running(runs...)
					for _, j := range snaps {
						for _, rs := range resumes {
							if rs != "" {
								j = j.resume(rs)
							} else {
								j.Resume = nil
							}
							n++
							p := Place(in, j)
							for _, id := range p.Devices {
								if isCPU(id) {
									t.Fatalf("Place chose %s: %+v / %+v", id, in, j)
								}
							}
							if p.Wait == "" && len(p.Devices) == 0 {
								t.Fatalf("no devices and no wait: %+v / %+v", in, j)
							}
							if h := SwitchHold(in, model.WaitPowerSwitchPending(cuda, igpu), p); len(h.Devices) > 0 {
								t.Fatalf("SwitchHold let a job start: %+v", h)
							}
						}
					}
					v := View(in)
					for _, id := range append(append([]string{}, v.Active...), v.Next) {
						if isCPU(id) && len(runs) == 0 {
							t.Fatalf("View shows %s as active: %+v / %+v", id, v, in)
						}
					}
					if v.Active == nil {
						t.Fatalf("View.Active nil: %+v", in)
					}
					for _, prev := range []Power{pwAC, pwUnknown, pwDesktop} {
						for i, a := range OnPowerChange(prev, in) {
							if isCPU(a.To) {
								t.Fatalf("migration to %s", a.To)
							}
							// Backend Gate round 1, fix 7: a CPU run never continues.
							if isCPU(in.Running[i].DeviceID) && a.Action == PowerContinue {
								t.Fatalf("a CPU run continues after %+v → %+v: %+v", prev, in.Power, a)
							}
						}
					}
				}
			}
		}
	}
	if n < 100000 {
		t.Fatalf("only %d placements checked", n)
	}
}

// A desktop (no battery, or an unknown power state) never sees a power_*
// code, over the same enumeration without the snapshots.
func TestPolicyDesktopNeverPowerCodes(t *testing.T) {
	ids5 := []string{cuda, igpu, npu, fwc, ovc}
	for c := 0; c < 1<<10; c++ {
		var ds []PolicyDevice
		for i, id := range ids5 {
			switch (c >> (2 * i)) & 3 {
			case 1:
				ds = append(ds, pdev(id))
			case 2:
				ds = append(ds, pdev(id, held))
			case 3:
				ds = append(ds, pdev(id, off))
			}
		}
		for _, pw := range []Power{pwDesktop, pwUnknown, {Known: true, OnBattery: true}} {
			for _, in := range []PolicyInput{pin(pw, ds...), pin(pw, ds...).manual(ids5...)} {
				for _, j := range []PolicyJob{jAuto, jBad, jFixed(cuda), jFixed(fwc), jAuto.resume(cuda)} {
					for _, in := range []PolicyInput{in, in.running(runOn(cuda))} {
						if p := Place(in, j); strings.HasPrefix(p.Wait, "power_") {
							t.Fatalf("desktop got %q: %+v / %+v", p.Wait, in, j)
						}
						for _, a := range OnPowerChange(pwUnknown, in) {
							if a.Action != PowerContinue {
								t.Fatalf("desktop power change: %+v", a)
							}
						}
					}
				}
			}
		}
	}
}

// ---------- manual override only on AC ----------

func TestPolicyManual(t *testing.T) {
	all := allDevs()
	runPlace(t, []placeCase{
		{"AC manual [cuda, igpu]: both candidates", pin(pwAC, all...).manual(cuda, igpu), jAuto, on(cuda, igpu)},
		{"AC manual, order of the list carries no meaning", pin(pwAC, all...).manual(igpu, cuda), jAuto, on(cuda, igpu)},
		{"AC manual [npu]: only the manual list puts the NPU on AC", pin(pwAC, all...).manual(npu), jAuto, on(npu)},
		{"battery + manual: the default policy (NPU)", pin(pwBattery, all...).manual(cuda, igpu), jAuto, on(npu)},
		{"battery + manual [cuda]: still never CUDA", pin(pwBattery, pdev(cuda), pdev(igpu)).manual(cuda), jAuto, on(igpu)},
		{"AC manual, every manual device unusable", pin(pwAC, with(with(all, cuda, off), igpu, gone)...).manual(cuda, igpu), jAuto, waits(model.WaitPowerNoDevice)},
		{"AC manual list with a CPU device: the CPU is dropped", pin(pwAC, all...).manual(fwc, igpu), jAuto, on(igpu)},
		{"AC manual list of CPU devices only: auto", pin(pwAC, all...).manual(fwc, ovc), jAuto, on(cuda)},
		{"3b single device id: manual [id]", PolicyInput{Power: pwAC, Mode: igpu, MultiDevice: true, Devices: all}, jAuto, on(igpu)},
		{"AC manual: a paused member is skipped", pin(pwAC, with(all, cuda, held)...).manual(cuda, igpu), jAuto, on(igpu)},
		{"AC manual: every member paused", pin(pwAC, with(with(all, cuda, held), igpu, held)...).manual(cuda, igpu), jAuto, waits(model.WaitDevicePaused(cuda))},
		{"AC manual: a busy member stays a candidate (3b slot checks)", pin(pwAC, all...).manual(cuda, igpu).running(runOn(cuda)), jAuto, on(cuda, igpu)},
		{"AC manual: a run outside the list (NPU after plugging in) → switch pending",
			pin(pwAC, all...).manual(cuda, igpu).running(runOn(npu)), jAuto, waits(model.WaitPowerSwitchPending(npu, cuda))},
		{"AC manual: fixed job on a list member runs next to the others",
			pin(pwAC, all...).manual(cuda, igpu).running(runOn(cuda)), jFixed(igpu), onFixed(igpu)},
		{"AC manual: fixed job outside the list waits for the list's runs",
			pin(pwAC, all...).manual(cuda, igpu).running(runOn(cuda)), jFixed(npu), waits(model.WaitPowerSwitchPending(cuda, npu))},
		{"desktop manual [igpu]: only the Intel GPU", pin(pwDesktop, all...).manual(igpu), jAuto, on(igpu)},
		{"desktop manual [igpu, npu]: both", pin(pwDesktop, all...).manual(npu, igpu), jAuto, on(igpu, npu)},
		{"desktop manual, the device not present", pin(pwDesktop, with(all, igpu, gone)...).manual(igpu), jAuto, waits(model.WaitNoASRDevice)},
	})
	v := View(pin(pwBattery, all...).manual(cuda, igpu))
	if !v.ManualIgnored || v.Mode != model.ASRDeviceModeAuto || v.Reason != PolicyReasonBattery || !reflect.DeepEqual(v.Active, []string{npu}) {
		t.Fatalf("battery + manual view: %+v", v)
	}
	v = View(pin(pwAC, all...).manual(cuda, igpu))
	if v.ManualIgnored || v.Mode != model.ASRDeviceModeManual || v.Reason != PolicyReasonManual || !reflect.DeepEqual(v.Active, []string{cuda, igpu}) {
		t.Fatalf("AC manual view: %+v", v)
	}
}

// ---------- unplugging: same backend migrates, another backend suspends ----------

func TestPolicyUnplug(t *testing.T) {
	all := allDevs()
	in := pin(pwBattery, all...).running(runOn(igpu), runOn(cuda), runOn(npu), fixedRunOn(igpu), runOn(ovc), runOn(fwc))
	got := OnPowerChange(pwAC, in)
	want := []RunAction{
		{JobID: "run-" + igpu, Action: PowerMigrate, From: igpu, To: npu},
		{JobID: "run-" + cuda, Action: PowerSuspend, From: cuda},
		{JobID: "run-" + npu, Action: PowerContinue, From: npu},
		{JobID: "run-" + igpu, Action: PowerSuspend, From: igpu}, // fixed on the Intel GPU: never migrates
		{JobID: "run-" + ovc, Action: PowerMigrate, From: ovc, To: npu},
		{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AC → battery:\n got  %+v\n want %+v", got, want)
	}
	// Without an NPU the Intel GPU is the battery device: its run continues.
	in2 := pin(pwBattery, without(all, npu)...).running(runOn(igpu), runOn(cuda))
	if got := OnPowerChange(pwAC, in2); got[0].Action != PowerContinue || got[1].Action != PowerSuspend {
		t.Fatalf("no NPU: %+v", got)
	}
	// No battery device at all: every run is suspended.
	in3 := pin(pwBattery, pdev(cuda)).running(runOn(cuda))
	if got := OnPowerChange(pwAC, in3); got[0].Action != PowerSuspend {
		t.Fatalf("no battery device: %+v", got)
	}
	// An unknown reading before counts as AC.
	if got := OnPowerChange(pwUnknown, in); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown → battery: %+v", got)
	}
	// Nothing else is a change that interrupts.
	for _, c := range []struct {
		prev Power
		in   PolicyInput
	}{
		{pwBattery, in}, // battery → battery
		{pwBattery, pin(pwAC, all...).running(runOn(npu), runOn(igpu))},                    // plugged in
		{pwAC, pin(pwAC, all...).running(runOn(cuda))},                                     // AC → AC
		{pwDesktop, pin(Power{Known: true, OnBattery: true}, all...).running(runOn(cuda))}, // no battery flag
		{pwAC, pin(pwUnknown, all...).running(runOn(cuda))},                                // reading failed
	} {
		for i, a := range OnPowerChange(c.prev, c.in) {
			want := PowerContinue
			if c.in.Power.BatteryMachine() && model.DeviceKind(c.in.Running[i].DeviceID) == model.DeviceKindCPU {
				// Backend Gate round 1, fix 7: a CPU run on a machine with a
				// battery is suspended on any power change.
				want = PowerSuspend
			}
			if a.Action != want || a.To != "" {
				t.Fatalf("%+v → %+v: %+v", c.prev, c.in.Power, a)
			}
		}
	}
	if got := OnPowerChange(pwAC, pin(pwBattery, all...)); got == nil || len(got) != 0 {
		t.Fatalf("no runs: %#v", got)
	}

	// Where the interrupted jobs go next.
	runPlace(t, []placeCase{
		{"battery, faster-whisper checkpoint → wait for the adapter", pin(pwBattery, all...), jAuto.resume(cuda), waits(model.WaitPowerWaitAC)},
		{"battery, fw:cpu checkpoint → wait for the adapter", pin(pwBattery, all...), jAuto.resume(fwc), waits(model.WaitPowerWaitAC)},
		{"battery, OpenVINO checkpoint → NPU, resumed", pin(pwBattery, all...), jAuto.resume(igpu), on(npu).resumed()},
		{"battery, faster-whisper checkpoint, no battery device → wait for the adapter", pin(pwBattery, pdev(cuda)), jAuto.resume(cuda), waits(model.WaitPowerWaitAC)},
		{"battery, OpenVINO checkpoint, no battery device → no saver device", pin(pwBattery, pdev(cuda)), jAuto.resume(igpu), waits(model.WaitPowerNoSaverDevice)},
		{"AC, CUDA checkpoint → back on CUDA, resumed", pin(pwAC, all...), jAuto.resume(cuda), on(cuda).resumed()},
		{"AC, Intel GPU checkpoint with CUDA there → CUDA from the start", pin(pwAC, all...), jAuto.resume(igpu), on(cuda)},
		{"AC, CUDA checkpoint, CUDA disabled meanwhile → Intel GPU from the start", pin(pwAC, with(all, cuda, off)...), jAuto.resume(cuda), on(igpu)},
		{"AC manual, Intel GPU checkpoint → Intel GPU first, resumed", pin(pwAC, all...).manual(cuda, igpu), jAuto.resume(igpu), on(igpu, cuda).resumed()},
		{"AC, CUDA checkpoint, CUDA busy → queue for CUDA", pin(pwAC, all...).running(runOn(cuda)), jAuto.resume(cuda), waits(model.WaitPowerQueue(cuda))},
		{"battery, fixed snapshot on the Intel GPU with an NPU → wait for the adapter", pin(pwBattery, all...), jFixed(igpu), waits(model.WaitPowerWaitAC)},
	})
}

// Backend Gate round 1, fix 7: a run on a CPU device (started while the
// machine counted as a desktop) is suspended as soon as the reading shows a
// machine with a battery, whatever the transition; only the unplug rule's
// same-backend migration (auto ov:CPU → an OpenVINO battery device,
// requirement 4a) moves it instead. A desktop reading changes nothing.
func TestPolicyCPURunsOnBatteryMachines(t *testing.T) {
	all := allDevs()
	fixedOVC := fixedRunOn(ovc)
	fixedOVC.JobID = "run-fixed-" + ovc
	cases := []struct {
		name string
		prev Power
		in   PolicyInput
		want []RunAction
	}{
		{"unknown → laptop on AC", pwUnknown, pin(pwAC, all...).running(runOn(fwc), runOn(ovc), runOn(cuda)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
			{JobID: "run-" + ovc, Action: PowerSuspend, From: ovc},
			{JobID: "run-" + cuda, Action: PowerContinue, From: cuda},
		}},
		{"no battery → laptop on AC", pwDesktop, pin(pwAC, all...).running(runOn(fwc), runOn(igpu)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
			{JobID: "run-" + igpu, Action: PowerContinue, From: igpu},
		}},
		{"AC → AC (any call on a machine with a battery)", pwAC, pin(pwAC, all...).running(runOn(ovc)), []RunAction{
			{JobID: "run-" + ovc, Action: PowerSuspend, From: ovc},
		}},
		{"battery → AC", pwBattery, pin(pwAC, all...).running(runOn(fwc), runOn(npu)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
			{JobID: "run-" + npu, Action: PowerContinue, From: npu},
		}},
		{"unknown → battery: ov:CPU migrates to the NPU (same backend), fw:cpu and a fixed ov:CPU are suspended", pwUnknown,
			pin(pwBattery, all...).running(runOn(ovc), runOn(fwc), fixedOVC), []RunAction{
				{JobID: "run-" + ovc, Action: PowerMigrate, From: ovc, To: npu},
				{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
				{JobID: "run-fixed-" + ovc, Action: PowerSuspend, From: ovc},
			}},
		{"unknown → battery without an OpenVINO battery device", pwUnknown, pin(pwBattery, pdev(cuda), pdev(ovc)).running(runOn(ovc)), []RunAction{
			{JobID: "run-" + ovc, Action: PowerSuspend, From: ovc},
		}},
		{"a CPU device without a row (kind by id)", pwUnknown, pin(pwAC, pdev(cuda)).running(runOn(fwc)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerSuspend, From: fwc},
		}},
		{"desktop readings: the CPU keeps running", pwUnknown, pin(pwDesktop, all...).running(runOn(fwc), runOn(ovc)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerContinue, From: fwc},
			{JobID: "run-" + ovc, Action: PowerContinue, From: ovc},
		}},
		{"a failed reading after AC: a desktop", pwAC, pin(pwUnknown, all...).running(runOn(fwc)), []RunAction{
			{JobID: "run-" + fwc, Action: PowerContinue, From: fwc},
		}},
	}
	for _, c := range cases {
		if got := OnPowerChange(c.prev, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
	}
	// Where a suspended CPU job goes: never back to the CPU.
	runPlace(t, []placeCase{
		{"AC, fw:cpu checkpoint, CUDA there → CUDA, resumed (same backend)", pin(pwAC, all...), jAuto.resume(fwc), on(cuda).resumed()},
		{"AC, fw:cpu checkpoint, Intel only → Intel GPU from the start", pin(pwAC, without(all, cuda)...), jAuto.resume(fwc), on(igpu)},
		{"AC, fw:cpu checkpoint, CPU devices only → no device", pin(pwAC, pdev(fwc), pdev(ovc)), jAuto.resume(fwc), waits(model.WaitPowerNoDevice)},
		{"battery, ov:CPU checkpoint → NPU, resumed", pin(pwBattery, all...), jAuto.resume(ovc), on(npu).resumed()},
	})
}

// ---------- plugging in never interrupts ----------

func TestPolicyPlugIn(t *testing.T) {
	all := allDevs()
	in := pin(pwAC, all...).running(runOn(npu))
	for _, a := range OnPowerChange(pwBattery, in) {
		if a.Action != PowerContinue {
			t.Fatalf("battery → AC: %+v", a)
		}
	}
	runPlace(t, []placeCase{
		{"NPU still runs: the next job waits for the switch", in, jAuto, waits(model.WaitPowerSwitchPending(npu, cuda))},
		{"NPU finished: CUDA", pin(pwAC, all...), jAuto, on(cuda)},
		{"a suspended CUDA job resumes on AC", pin(pwAC, all...), jAuto.resume(cuda), on(cuda).resumed()},
		{"the suspended CUDA job also waits while the NPU finishes", in, jAuto.resume(cuda), waits(model.WaitPowerSwitchPending(npu, cuda))},
		{"no CUDA: the Intel GPU waits for the NPU too", pin(pwAC, without(all, cuda)...).running(runOn(npu)), jAuto, waits(model.WaitPowerSwitchPending(npu, igpu))},
	})
	v := View(in)
	if !reflect.DeepEqual(v.Active, []string{npu}) || v.Next != cuda || v.Reason != PolicyReasonACAuto || v.NoneReason != "" {
		t.Fatalf("view while the switch is pending: %+v", v)
	}
}

// ---------- desktops keep the 3b rules ----------

func TestPolicyDesktopUnchanged(t *testing.T) {
	all := allDevs()
	all = with(all, ovc, off) // ov:CPU is disabled by default
	for _, pw := range []Power{pwDesktop, pwUnknown, {Known: true, OnBattery: true}} {
		runPlace(t, []placeCase{
			{"every usable device in priority order, CPU included", pin(pw, all...), jAuto, on(cuda, igpu, npu, fwc)},
			{"busy devices stay candidates (the scheduler's 3b checks)", pin(pw, all...).running(runOn(cuda), runOn(igpu)), jAuto, on(cuda, igpu, npu, fwc)},
			{"only fw:cpu: the CPU fallback (Q1)", pin(pw, pdev(fwc)), jAuto, on(fwc)},
			{"a paused device is skipped", pin(pw, with(all, cuda, held)...), jAuto, on(igpu, npu, fwc)},
			{"every device paused: the first is named", pin(pw, pdev(cuda, held), pdev(igpu, held)), jAuto, waits(model.WaitDevicePaused(cuda))},
			{"no usable device", pin(pw, pdev(cuda, off), pdev(igpu, gone)), jAuto, waits(model.WaitNoASRDevice)},
			{"fixed CUDA", pin(pw, all...), jFixed(cuda), onFixed(cuda)},
			{"fixed CUDA, another device busy: no switch rule", pin(pw, all...).running(runOn(igpu)), jFixed(cuda), onFixed(cuda)},
			{"fixed fw:cpu", pin(pw, all...), jFixed(fwc), onFixed(fwc)},
			{"fixed, paused", pin(pw, with(all, igpu, held)...), jFixed(igpu), waits(model.WaitDevicePaused(igpu))},
			{"fixed, missing row", pin(pw, all...), jFixed("ov:XPU"), waits(model.WaitNoASRDevice)},
			{"fixed, disabled", pin(pw, all...), jFixed(ovc), waits(model.WaitNoASRDevice)},
			{"fixed, not present", pin(pw, with(all, npu, gone)...), jFixed(npu), waits(model.WaitNoASRDevice)},
			{"multi_device off: still every candidate (the scheduler's rule)", func() PolicyInput { in := pin(pw, all...).running(runOn(cuda)); in.MultiDevice = false; return in }(), jAuto, on(cuda, igpu, npu, fwc)},
			{"a checkpoint does not reorder a desktop", pin(pw, all...), jAuto.resume(igpu), on(cuda, igpu, npu, fwc)},
		})
	}
	if DesktopCPUFallback != true {
		t.Fatal("Q1: desktops keep the CPU fallback")
	}
	v := View(pin(pwDesktop, with(all, cuda, held)...))
	if v.BatteryMachine || v.OnBattery || v.Reason != PolicyReasonDesktop || v.Mode != model.ASRDeviceModeAuto ||
		!reflect.DeepEqual(v.Active, []string{cuda, igpu, npu, fwc}) || v.NoneReason != "" || v.Next != "" {
		t.Fatalf("desktop view: %+v", v)
	}
	v = View(pin(pwDesktop, all...).manual(igpu, npu))
	if v.Reason != PolicyReasonManual || v.Mode != model.ASRDeviceModeManual || !reflect.DeepEqual(v.Active, []string{igpu, npu}) {
		t.Fatalf("desktop manual view: %+v", v)
	}
	v = View(pin(pwUnknown))
	if v.NoneReason != model.WaitNoASRDevice || v.Active == nil || len(v.Active) != 0 {
		t.Fatalf("desktop without devices: %+v", v)
	}
}

// ---------- single active device: queue, pause, self-test, global pause, fixed ----------

func TestPolicySingleActive(t *testing.T) {
	all := allDevs()
	noMulti := func(in PolicyInput) PolicyInput { in.MultiDevice = false; return in }
	runPlace(t, []placeCase{
		{"battery, NPU busy → queue for the NPU (not the Intel GPU)", pin(pwBattery, all...).running(runOn(npu)), jAuto, waits(model.WaitPowerQueue(npu))},
		{"AC, CUDA busy → queue for CUDA (not the Intel GPU)", pin(pwAC, all...).running(runOn(cuda)), jAuto, waits(model.WaitPowerQueue(cuda))},
		{"AC, CUDA with two slots, one used → CUDA", pin(pwAC, with(all, cuda, slots(2))...).running(runOn(cuda)), jAuto, on(cuda)},
		{"AC, two slots but multi_device off → queue", noMulti(pin(pwAC, with(all, cuda, slots(2))...).running(runOn(cuda))), jAuto, waits(model.WaitPowerQueue(cuda))},
		{"AC, slots used by the count alone", pin(pwAC, with(all, cuda, used(1))...), jAuto, waits(model.WaitPowerQueue(cuda))},
		{"battery, NPU paused → wait for it (a pause is no fallback)", pin(pwBattery, with(all, npu, held)...), jAuto, waits(model.WaitDevicePaused(npu))},
		{"AC, CUDA paused → wait for it", pin(pwAC, with(all, cuda, held)...), jAuto, waits(model.WaitDevicePaused(cuda))},
		{"battery, a self-test holds the NPU", pin(pwBattery, with(all, npu, testing1)...), jAuto, waits(model.WaitDeviceSelfTest(npu))},
		{"global pause, battery", func() PolicyInput { in := pin(pwBattery, all...); in.PausedAll = true; return in }(), jAuto, waits(model.WaitGPUPaused)},
		{"global pause, AC", func() PolicyInput { in := pin(pwAC, all...); in.PausedAll = true; return in }(), jFixed(cuda), waits(model.WaitGPUPaused)},
		{"global pause, desktop", func() PolicyInput { in := pin(pwDesktop, all...); in.PausedAll = true; return in }(), jAuto, waits(model.WaitGPUPaused)},
		// fixed snapshots on a machine with a battery
		{"fixed, missing row", pin(pwAC, all...), jFixed("ov:XPU"), waits(model.WaitNoASRDevice)},
		{"fixed, disabled", pin(pwAC, with(all, cuda, off)...), jFixed(cuda), waits(model.WaitNoASRDevice)},
		{"fixed, not present", pin(pwBattery, with(all, npu, gone)...), jFixed(npu), waits(model.WaitNoASRDevice)},
		{"fixed on fw:cpu → no device (never the CPU)", pin(pwAC, all...), jFixed(fwc), waits(model.WaitPowerNoDevice)},
		{"fixed on ov:CPU, battery → no device", pin(pwBattery, all...), jFixed(ovc), waits(model.WaitPowerNoDevice)},
		{"battery, fixed CUDA → wait for the adapter", pin(pwBattery, all...), jFixed(cuda), waits(model.WaitPowerWaitAC)},
		{"battery, fixed Intel GPU without an NPU → runs", pin(pwBattery, without(all, npu)...), jFixed(igpu), onFixed(igpu)},
		{"battery, fixed Intel GPU without a battery device", pin(pwBattery, pdev(cuda), pdev(igpu, off)), jFixed(cuda), waits(model.WaitPowerWaitAC)},
		{"battery, fixed NPU", pin(pwBattery, all...), jFixed(npu), onFixed(npu)},
		{"AC, fixed NPU", pin(pwAC, all...), jFixed(npu), onFixed(npu)},
		{"AC, fixed Intel GPU while CUDA runs → switch pending", pin(pwAC, all...).running(runOn(cuda)), jFixed(igpu), waits(model.WaitPowerSwitchPending(cuda, igpu))},
		{"AC, fixed CUDA busy → queue", pin(pwAC, all...).running(runOn(cuda)), jFixed(cuda), waits(model.WaitPowerQueue(cuda))},
		{"AC, fixed CUDA paused", pin(pwAC, with(all, cuda, held)...), jFixed(cuda), waits(model.WaitDevicePaused(cuda))},
		{"AC, fixed CUDA under a self-test", pin(pwAC, with(all, cuda, testing1)...), jFixed(cuda), waits(model.WaitDeviceSelfTest(cuda))},
		{"a run outside wins over the device's own pause", pin(pwAC, with(all, cuda, held)...).running(runOn(npu)), jAuto, waits(model.WaitPowerSwitchPending(npu, cuda))},
		{"several runs outside: the highest-priority one is named", pin(pwAC, all...).running(runOn(npu), runOn(igpu)), jAuto, waits(model.WaitPowerSwitchPending(igpu, cuda))},
	})
}

// ---------- lead ruling 9.3: the switch barrier ----------

func TestPolicySwitchHold(t *testing.T) {
	all := with(allDevs(), cuda, slots(2))
	in := pin(pwAC, all...).running(runOn(cuda))
	// FIFO: the fixed job F (earlier) waits for CUDA to drain; the auto job
	// A after it would take CUDA's free slot and keep F waiting forever.
	f := Place(in, jFixed(igpu))
	if f.Wait != model.WaitPowerSwitchPending(cuda, igpu) {
		t.Fatalf("F: %+v", f)
	}
	a := Place(in, jAuto)
	if !reflect.DeepEqual(a, on(cuda)) {
		t.Fatalf("A without the barrier: %+v", a)
	}
	if h := SwitchHold(in, f.Wait, a); !reflect.DeepEqual(h, waits(f.Wait)) {
		t.Fatalf("A with the barrier: %+v", h)
	}
	// CUDA drained: F starts on the Intel GPU; A then waits for the switch back.
	in2 := pin(pwAC, all...)
	if f := Place(in2, jFixed(igpu)); !reflect.DeepEqual(f, onFixed(igpu)) {
		t.Fatalf("F after the drain: %+v", f)
	}
	in3 := pin(pwAC, all...).running(fixedRunOn(igpu))
	if a := Place(in3, jAuto); a.Wait != model.WaitPowerSwitchPending(igpu, cuda) {
		t.Fatalf("A while F runs: %+v", a)
	}
	// Not applied: a desktop, an earlier code that is no switch, a waiting placement.
	if h := SwitchHold(pin(pwDesktop, all...), f.Wait, a); !reflect.DeepEqual(h, a) {
		t.Fatalf("desktop: %+v", h)
	}
	if h := SwitchHold(in, model.WaitPowerQueue(cuda), a); !reflect.DeepEqual(h, a) {
		t.Fatalf("not a switch: %+v", h)
	}
	w := waits(model.WaitDevicePaused(cuda))
	if h := SwitchHold(in, f.Wait, w); !reflect.DeepEqual(h, w) {
		t.Fatalf("waiting placement: %+v", h)
	}
	// Manual mode on AC too: a fixed job outside the list.
	inM := pin(pwAC, all...).manual(cuda).running(runOn(cuda))
	fm := Place(inM, jFixed(igpu))
	if h := SwitchHold(inM, fm.Wait, Place(inM, jAuto)); h.Wait != model.WaitPowerSwitchPending(cuda, igpu) {
		t.Fatalf("manual: %+v / %+v", fm, h)
	}
}

// ---------- self-test ----------

func TestPolicySelfTest(t *testing.T) {
	all := allDevs()
	yes, startNo, no := SelfTestDecision{Allowed: true, Start: true}, SelfTestDecision{Allowed: true}, SelfTestDecision{}
	cases := []struct {
		name string
		in   PolicyInput
		id   string
		want SelfTestDecision
	}{
		{"desktop, any device", pin(pwDesktop, all...).running(runOn(igpu)), cuda, yes},
		{"desktop, unknown power", pin(pwUnknown, all...), fwc, yes},
		{"battery, the NPU", pin(pwBattery, all...), npu, yes},
		{"battery, CUDA", pin(pwBattery, all...), cuda, no},
		{"battery, the Intel GPU while an NPU exists", pin(pwBattery, all...), igpu, no},
		{"battery, the Intel GPU without an NPU", pin(pwBattery, without(all, npu)...), igpu, yes},
		{"battery, a CPU device", pin(pwBattery, all...), fwc, no},
		// Backend Gate round 1, fix 7: never a CPU device on a machine with a battery.
		{"AC, fw:cpu", pin(pwAC, all...), fwc, no},
		{"AC, ov:CPU", pin(pwAC, all...), ovc, no},
		{"battery, ov:CPU", pin(pwBattery, all...), ovc, no},
		{"AC, a CPU device without a row (kind by id)", pin(pwAC, pdev(cuda)), ovc, no},
		{"AC, only CPU devices", pin(pwAC, pdev(fwc), pdev(ovc)), fwc, no},
		{"desktop, ov:CPU", pin(pwDesktop, all...), ovc, yes},
		{"battery without a battery device", pin(pwBattery, pdev(cuda)), cuda, no},
		{"AC, CUDA", pin(pwAC, all...), cuda, yes},
		{"AC, the NPU (allowed on AC)", pin(pwAC, all...), npu, yes},
		{"AC, a pipeline job on another device", pin(pwAC, all...).running(runOn(cuda)), npu, startNo},
		{"AC, a pipeline job on the device itself (the scheduler's 3b rule)", pin(pwAC, all...).running(runOn(cuda)), cuda, yes},
		{"AC, another device's self-test holds a slot", pin(pwAC, with(all, igpu, testing1)...), cuda, startNo},
		{"battery, the NPU while the Intel GPU still runs", pin(pwBattery, all...).running(runOn(igpu)), npu, startNo},
	}
	for _, c := range cases {
		if got := SelfTest(c.in, c.id); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// ---------- view ----------

func TestPolicyView(t *testing.T) {
	all := allDevs()
	cases := []struct {
		name string
		in   PolicyInput
		want PolicyView
	}{
		{"AC auto, idle", pin(pwAC, all...),
			PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonACAuto, Active: []string{cuda}}},
		{"AC auto, Intel only", pin(pwAC, without(all, cuda)...),
			PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonACAuto, Active: []string{igpu}}},
		{"battery, idle", pin(pwBattery, all...),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonBattery, Active: []string{npu}}},
		{"battery, NPU paused stays active", pin(pwBattery, with(all, npu, held)...),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonBattery, Active: []string{npu}}},
		{"battery, running on the NPU", pin(pwBattery, all...).running(runOn(npu)),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonBattery, Active: []string{npu}}},
		{"AC, the NPU still runs: switch pending", pin(pwAC, all...).running(runOn(npu)),
			PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonACAuto, Active: []string{npu}, Next: cuda}},
		{"battery, no saver device", pin(pwBattery, pdev(cuda), pdev(fwc)),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonBattery, Active: []string{}, NoneReason: model.WaitPowerNoSaverDevice}},
		{"AC, no device", pin(pwAC, pdev(npu), pdev(fwc)),
			PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonACAuto, Active: []string{}, NoneReason: model.WaitPowerNoDevice}},
		{"battery, a CUDA run not yet suspended", pin(pwBattery, pdev(cuda), pdev(fwc)).running(runOn(cuda)),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, Reason: PolicyReasonBattery, Active: []string{cuda}}},
		{"AC manual, two runs", pin(pwAC, all...).manual(igpu, cuda).running(runOn(igpu), runOn(cuda)),
			PolicyView{BatteryMachine: true, Mode: model.ASRDeviceModeManual, Reason: PolicyReasonManual, Active: []string{cuda, igpu}}},
		{"battery + manual list", pin(pwBattery, all...).manual(cuda),
			PolicyView{BatteryMachine: true, OnBattery: true, Mode: model.ASRDeviceModeAuto, ManualIgnored: true, Reason: PolicyReasonBattery, Active: []string{npu}}},
	}
	for _, c := range cases {
		if got := View(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
	}
	if id, ok := BatteryDevice(pin(pwAC, all...)); !ok || id != npu {
		t.Fatalf("BatteryDevice = %q %v", id, ok)
	}
	if _, ok := BatteryDevice(pin(pwBattery, pdev(cuda), pdev(fwc))); ok {
		t.Fatal("BatteryDevice without NPU / Intel GPU")
	}
}
