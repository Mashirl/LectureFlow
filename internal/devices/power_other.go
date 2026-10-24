// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package devices

import "errors"

// Outside Windows the power state and the available memory are unknown
// (LectureFlow targets Windows; the tests and the scheduler handle both).

func readPower() Power { return Power{} }

func availableMemoryMB() (int, error) {
	return 0, errors.New("devices: available memory is only read on Windows")
}
