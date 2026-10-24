// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Power management (v2-step20, task P4): the power state and the power
// policy's view in the resource snapshot (types_power.go). The API's
// GET /api/system power (api.PowerView = Power) uses PowerOf too.

// PowerSourceOf is Power.Source: "" while the AC state is unknown, battery
// on battery, else ac ("外接电源"). USB-C PD and a power adapter are not
// told apart (power-plan §4, lead ruling 16).
func PowerSourceOf(p devices.Power) string {
	switch {
	case !p.Known:
		return ""
	case p.OnBattery:
		return PowerSourceBattery
	}
	return PowerSourceAC
}

// PowerOf is the Power of a reading with the battery_aware setting (retired
// by power management, still sent).
func PowerOf(p devices.Power, batteryAware bool) Power {
	v := Power{Known: p.Known, OnBattery: p.OnBattery, Saver: p.Saver, BatteryAware: batteryAware,
		HasBattery: p.HasBattery, Charging: p.Charging, Source: PowerSourceOf(p)}
	v.BatteryPct = cloneInt(p.BatteryPct)
	return v
}

// PolicyOf is Snapshot.Policy of the scheduler's power status: the policy
// view (devices.PolicyView) with the counts and notices. Active and Notices
// are never null; an empty Mode is auto.
func PolicyOf(st sched.PowerStatus) *Policy {
	v := st.View
	p := &Policy{
		BatteryMachine: v.BatteryMachine,
		Mode:           v.Mode,
		ManualIgnored:  v.ManualIgnored,
		Reason:         v.Reason,
		Active:         append([]string{}, v.Active...),
		Next:           v.Next,
		NoneReason:     v.NoneReason,
		WaitAC:         st.WaitAC,
		WaitNoDevice:   st.WaitNoDevice,
		Notices:        append([]PowerNotice{}, st.Notices...),
	}
	if p.Mode == "" {
		p.Mode = model.ASRDeviceModeAuto
	}
	return p
}
