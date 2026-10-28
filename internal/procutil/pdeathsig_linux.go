// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build linux

package procutil

import (
	"os/exec"
	"syscall"
)

// endWithParent: the kernel sends the child SIGKILL when its parent ends
// (PR_SET_PDEATHSIG). Strictly, when the thread that started it ends: the
// Go runtime ends a thread only when a goroutine locked to it
// (runtime.LockOSThread) returns without unlocking, which nothing in
// LectureFlow does.
func endWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
