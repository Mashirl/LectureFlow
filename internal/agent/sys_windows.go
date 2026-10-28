// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package agent

// Windows (the agent runs here only in this repository's tests; a Windows
// remote with Job Objects is plan step 5).

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

func setProcAttr(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killedBySIGKILL(*exec.ExitError) bool { return false }

func ignoreSessionSignals() {}

func diskFreeMB(dir string) (int64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, false
	}
	return int64(free >> 20), true
}
