// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import "lectureflow/internal/sched"

// Power management (user requirement 2026-10-24, part 2 "资源栏布局";
// .claude/power-brief.md §6). JSON types FROZEN by the lead for v2-step20;
// filled by task P4, shown by task P5. The resource bar has three areas:
//
//   - upper "系统与电源": Snapshot.System and Snapshot.Power (with the line
//     "2 个任务等待电源适配器" from Policy.WaitAC);
//   - middle "当前活跃的推理设备": the cards of Snapshot.Devices with Active
//     (the 3b card contents; "为什么是它" = Policy.Reason), or
//     Policy.NoneReason when there is none;
//   - bottom: the other devices, one row each (Device.Active false).

// Power sources (Power.Source).
const (
	PowerSourceAC      = "ac"
	PowerSourceBattery = "battery"
)

// System is the upper area's readings, sampled with the GPU (every 2 s
// while a front end is connected). Every field is null when it cannot be
// read; nothing requires administrator rights (the UI shows "--").
type System struct {
	// CPUPct is the total CPU utilisation (PDH "\Processor
	// Information(_Total)\% Processor Utility", capped at 100; null on the
	// first sample of the rate counter).
	CPUPct *int `json:"cpu_pct"`
	// MemUsedMB / MemTotalMB: physical memory in use and installed
	// (GlobalMemoryStatusEx: ullTotalPhys − ullAvailPhys, ullTotalPhys).
	MemUsedMB  *int `json:"mem_used_mb"`
	MemTotalMB *int `json:"mem_total_mb"`
	// CPUTempC is the highest thermal zone temperature (PDH "\Thermal Zone
	// Information(*)\Temperature", Kelvin → °C, rounded); null when no zone
	// reports one (common on desktops and some laptops).
	CPUTempC *int `json:"cpu_temp_c"`

	// mac-opti M6 (apple.go). Unsupported names the readings this system
	// cannot give at all (ReadingCPUTemp on macOS): the front end leaves
	// them out instead of showing "--" (null alone means "not known right
	// now"); omitted when every reading is supported (Windows).
	Unsupported []string `json:"unsupported,omitempty"`
	// UnifiedMemory: the memory is shared by the CPU and the GPU (Apple
	// silicon): MemUsedMB / MemTotalMB are the unified memory, the GPU has
	// no memory of its own ("显存"). Omitted (false) elsewhere.
	UnifiedMemory bool `json:"unified_memory,omitempty"`
}

// Policy is the power policy's state (sched.PowerStatus →
// devices.PolicyView plus the counts and notices).
type Policy struct {
	// BatteryMachine: the power-aware rules of a machine with a battery
	// apply (devices.Power.BatteryMachine); false = desktop (3b rules).
	BatteryMachine bool `json:"battery_machine"`
	// Mode is the effective device mode: auto | manual (manual only when a
	// manual list is set and it applies: a desktop, or AC power).
	Mode string `json:"mode"`
	// ManualIgnored: a manual list is set but the machine runs on battery
	// ("手动指定 · 使用电池时不生效").
	ManualIgnored bool `json:"manual_ignored"`
	// Reason is why the active devices are active (devices.PolicyReason*:
	// ac_auto "外接电源 · 自动选择", battery "电池 · 电源感知", manual "手动指
	// 定", desktop "台式机 · 自动").
	Reason string `json:"reason"`
	// Active are the device ids of the middle area (priority order; never
	// null).
	Active []string `json:"active"`
	// Next is the device that becomes active when the running one finished
	// its lecture (power_switch_pending); omitted otherwise.
	Next string `json:"next,omitempty"`
	// NoneReason is why there is no active device: a waiting_for code
	// (power_no_saver_device, power_no_device, no_asr_device), shown with
	// its waiting.* text; omitted when there are active devices.
	NoneReason string `json:"none_reason,omitempty"`
	// WaitAC is the number of jobs waiting for the power adapter
	// (power_wait_ac); WaitNoDevice of those waiting because no
	// transcription device qualifies (power_no_saver_device,
	// power_no_device).
	WaitAC       int `json:"wait_ac"`
	WaitNoDevice int `json:"wait_no_device"`
	// Notices are the policy's recent notifications, oldest first (never
	// null): the front end announces each ID once (toast / browser
	// notification with notes.{info.code}).
	Notices []PowerNotice `json:"notices"`
}

// PowerNotice is one notification (sched.PowerNotice).
type PowerNotice = sched.PowerNotice
