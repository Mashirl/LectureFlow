// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"reflect"
	"strings"
)

// ---------------------------------------------------------------------------
// Power management (user requirement 2026-10-24, .claude/power-req.md; lead
// plan .claude/power-plan.md). FROZEN by the lead for v2-step20
// (.claude/power-brief.md, NOTES.md "电源管理冻结（core-builder）"); changes
// only through the lead. The selection rules themselves are
// devices.Place / OnPowerChange / View (internal/devices/policy.go).
// ---------------------------------------------------------------------------

// ASRDeviceModeManual is ASRSettings.DeviceMode "手动指定": the devices of
// ASRSettings.ManualDevices may be used (several at the same time), on AC
// power only; on battery a machine with a battery always follows the
// power-aware default (requirement 6). A manual list never contains a CPU
// device (DeviceKindCPU), on any machine.
const ASRDeviceModeManual = "manual"

// NormalizeASRDeviceMode returns a with DeviceMode and ManualDevices in the
// power-management form (pure; settings.Load and SetASR apply it, task P4):
//   - "" or "auto" → auto, ManualDevices cleared (nil);
//   - "manual" → manual with ManualDevices trimmed, blanks and duplicates
//     dropped (first occurrence kept), CPU devices dropped; an empty result
//     → auto (nil list). Order is kept but carries no meaning: devices are
//     tried in their priority order (the device list);
//   - anything else is a phase-3b single device id: manual [id], or auto when
//     id is a CPU device (DeviceKind == cpu: a manual list never holds one).
//
// SetASR validates the request before normalizing (manual with an empty
// list, a CPU device or an unknown id is refused: settings
// CodeManualDevicesEmpty / CodeManualDevicesCPU / CodeManualDevicesUnknown);
// reading stored settings only normalizes. Nothing else is changed.
func NormalizeASRDeviceMode(a ASRSettings) ASRSettings {
	mode := strings.TrimSpace(a.DeviceMode)
	switch mode {
	case "", ASRDeviceModeAuto:
		a.DeviceMode, a.ManualDevices = ASRDeviceModeAuto, nil
		return a
	case ASRDeviceModeManual:
	default:
		// A 3b settings value: the id of the one device to use.
		a.ManualDevices = []string{mode}
	}
	seen := map[string]bool{}
	var list []string
	for _, id := range a.ManualDevices {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || DeviceKind(id) == DeviceKindCPU {
			continue
		}
		seen[id] = true
		list = append(list, id)
	}
	if len(list) == 0 {
		a.DeviceMode, a.ManualDevices = ASRDeviceModeAuto, nil
		return a
	}
	a.DeviceMode, a.ManualDevices = ASRDeviceModeManual, list
	return a
}

// waiting_for values of the power policy (SPEC §9.5; requirement 7: every
// wait, suspension and switch the policy causes says "电源感知"). The front
// end shows waiting.{code} (task P5; ids are replaced by the device names).
// They replace WaitOnBattery on machines with a battery.
const (
	// WaitPowerWaitAC: "挂起：电源感知 - 等待电源适配器". On battery, a job
	// whose checkpoint (JobStats.ASRResume) or fixed snapshot belongs to a
	// device that is not the battery device's backend / not the battery
	// device: it continues on AC power, never on the NPU or Intel GPU.
	WaitPowerWaitAC = "power_wait_ac"
	// WaitPowerNoSaverDevice: "挂起：电源感知 - 当前没有可用的省电转写设备".
	// On battery, neither an NPU nor an Intel GPU is enabled and present.
	WaitPowerNoSaverDevice = "power_no_saver_device"
	// WaitPowerNoDevice: "挂起：电源感知 - 没有可用的转写设备（不使用 CPU 转写）".
	// A machine with a battery on AC power: no CUDA device and no Intel GPU
	// is enabled and present (auto), or none of the manual devices is (also:
	// a fixed snapshot on a CPU device).
	WaitPowerNoDevice = "power_no_device"
	// WaitPowerQueuePrefix: power_queue:{id}, "排队：电源感知 - 等待 {device}
	// 空出（笔记本同一时间只用一台转写设备）". The one active device of a
	// machine with a battery is busy (its slots are full, or multi_device is
	// off and a job runs); the job is not given to another device
	// (requirement 5).
	WaitPowerQueuePrefix = "power_queue:"
	// WaitPowerSwitchPendingPrefix: power_switch_pending:{from}:{to},
	// "电源感知：{from} 完成当前讲座后换到 {to}". A pipeline asr job runs on
	// device from, which is not the device the policy chooses now (to; e.g.
	// the NPU still runs after the charger was plugged in again): the job
	// waits until from finished its current lecture (requirement 4: plugging
	// in never interrupts).
	WaitPowerSwitchPendingPrefix = "power_switch_pending:"
)

func WaitPowerQueue(id string) string { return WaitPowerQueuePrefix + id }

// WaitPowerSwitchPending builds power_switch_pending:{from}:{to}. Device ids
// contain exactly one ':' (cuda:0, ov:GPU, ov:NPU, ov:CPU, fw:cpu), which is
// how ParseWaitPowerSwitchPending splits the two.
func WaitPowerSwitchPending(from, to string) string {
	return WaitPowerSwitchPendingPrefix + from + ":" + to
}

// ParseWaitPowerSwitchPending splits a power_switch_pending value into its
// two device ids; ok is false for any other value or when the rest is not
// two ids of the form "x:y" each.
func ParseWaitPowerSwitchPending(w string) (from, to string, ok bool) {
	rest, found := strings.CutPrefix(w, WaitPowerSwitchPendingPrefix)
	if !found {
		return "", "", false
	}
	p := strings.Split(rest, ":")
	if len(p) != 4 {
		return "", "", false
	}
	for _, s := range p {
		if s == "" {
			return "", "", false
		}
	}
	return p[0] + ":" + p[1], p[2] + ":" + p[3], true
}

// ASRPart is one part of an asr run that was resumed from a checkpoint
// (power-plan §3): the audio range [FromMS, ToMS) and the device, model and
// precision that transcribed it. The workers write them into result.json
// "parts"; the asr stage stores them in the snapshot (ASRSnapshot.Parts)
// when the run has two parts or more.
type ASRPart struct {
	FromMS    int64  `json:"from_ms"`
	ToMS      int64  `json:"to_ms"`
	DeviceID  string `json:"device_id"`
	Model     string `json:"model"`
	Precision string `json:"precision"`
}

// EqualASRSnapshot reports whether two snapshots are the same value, field
// by field, Parts included (nil and empty Parts are equal). ASRSnapshot has
// a slice since power management and can no longer be compared with ==;
// this is the replacement (not the A4.3 comparison, which is SameSnapshot).
func EqualASRSnapshot(a, b ASRSnapshot) bool {
	if len(a.Parts) == 0 {
		a.Parts = nil
	}
	if len(b.Parts) == 0 {
		b.Parts = nil
	}
	return reflect.DeepEqual(a, b)
}

// EqualASRSettings is == for ASRSettings, which has a slice since power
// management (ManualDevices; nil and empty are equal).
func EqualASRSettings(a, b ASRSettings) bool {
	if len(a.ManualDevices) == 0 {
		a.ManualDevices = nil
	}
	if len(b.ManualDevices) == 0 {
		b.ManualDevices = nil
	}
	return reflect.DeepEqual(a, b)
}

// ASRResume is job.stats.asr_resume: the device an asr run was interrupted
// on by the power policy (migrate or suspend). The scheduler writes it with
// the power requeue and clears it when the job is done; the policy reads it
// (devices.PolicyJob.Resume): on battery a job whose Backend is not the
// battery device's waits WaitPowerWaitAC; on AC it goes back to DeviceID
// when that device is in the active set. The checkpoint file itself
// (asr.ckpt.json) is the worker's business: the worker resumes whenever
// job id, input hash and backend match, whatever this field says.
type ASRResume struct {
	DeviceID string `json:"device_id"`
	Backend  string `json:"backend"`
	At       string `json:"at"` // UTC RFC3339, when the run was interrupted
}

// PowerEvent is job.stats.power_event: the last power-policy event of an
// asr job, shown as the stage card's note (notes.{Code}) while the job is
// queued or running (task P4 maps it to StageView.NoteInfo). Code is one of
// the sched notice codes power.switched / power.suspended / power.resumed.
// FromDevice / ToDevice are device ids.
type PowerEvent struct {
	Code       string `json:"code"`
	At         string `json:"at"` // UTC RFC3339
	FromDevice string `json:"from_device,omitempty"`
	ToDevice   string `json:"to_device,omitempty"`
}

// CountedAttempts is the number of attempts that count against
// MaxAutoRetries (SPEC §4.2): job.attempt minus the requeues caused by the
// power policy (JobStats.PowerInterrupts), never below 0, so plugging and
// unplugging the charger never uses up the automatic retries after real
// failures (power-plan §1). SPEC-v3 V14: the requeues of a lecture Colab
// handed back (JobStats.RemoteRequeues) do not count either. FailJob, AutoRetry and the scheduler's finish
// compare this value with MaxAutoRetries (task P1).
func CountedAttempts(j Job) int {
	n := j.Attempt - j.Stats.PowerInterrupts - j.Stats.RemoteRequeues
	if n < 0 {
		return 0
	}
	return n
}
