// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"lectureflow/internal/devices"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
)

// Phase 3b freeze (.claude/v3b-brief.md): the device registry is the
// DeviceSource of the settings and of the scheduler, and the stages' self-test
// is the scheduler's DeviceTestFunc. A signature change breaks this build.
var (
	_ settings.DeviceSource = (*devices.Registry)(nil)
	_ sched.DeviceSource    = (*devices.Registry)(nil)
	_ sched.DeviceTestFunc  = stages.Deps{}.DeviceTest
)
