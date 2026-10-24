// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package resources

import "errors"

// The upper area's readings exist on Windows only (system.go); elsewhere
// every value is null ("--").

func newSystemReader() (systemReader, error) {
	return nil, errors.New("CPU and thermal counters are only available on Windows")
}

func readMemStatus() (totalPhys, availPhys uint64, err error) {
	return 0, 0, errors.New("the memory status is only read on Windows")
}
