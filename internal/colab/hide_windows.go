// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package colab

import (
	"os/exec"
	"syscall"
	"time"
)

// hideWindow keeps ffmpeg from flashing a console window (the desktop
// app's service has no console).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.WaitDelay = time.Second
}
