// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package colab

import "syscall"

// processAlive: signal 0 reaches the process (a zombie counts as alive
// until it is reaped; the stopped gcloud's children are reaped by init /
// launchd).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
