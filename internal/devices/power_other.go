// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows && !darwin

package devices

import "errors"

// Outside Windows and macOS the power state and the available memory are unknown
// (LectureFlow targets Windows and macOS; the tests and the scheduler handle both).

func readPower() Power { return Power{} }

func availableMemoryMB() (int, error) {
	return 0, errors.New("devices: available memory is only read on Windows")
}
