// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"lectureflow/internal/devices"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
)

// Power management freeze (.claude/power-brief.md): the wiring cmd does in
// task P1 — the scheduler's power status feeds the resource watcher, the
// power reader (with the development simulation) feeds the scheduler, and
// the power request keeps the machine awake. A signature change breaks this
// build.
var (
	_ func() sched.PowerStatus                       = (*sched.Scheduler)(nil).PowerStatus
	_ func() sched.PowerStatus                       = resources.Deps{}.PowerStatus
	_ func(func(string) string) func() devices.Power = devices.NewPowerReader
	_ func() devices.Power                           = sched.Options{}.Power
	_ func(bool) error                               = (*devices.KeepAwake)(nil).Set
	_ func(bool)                                     = sched.Options{}.KeepAwake
)
