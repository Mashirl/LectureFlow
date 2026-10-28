// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build unix

package agent

import (
	"os/exec"
	"os/signal"
	"syscall"
)

// setProcAttr puts the worker into its own process group, so a cancel
// reaches its children too.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the worker's process group to stop (SIGTERM).
func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// kill ends the worker's process group (SIGKILL).
func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killedBySIGKILL reports whether the process died of SIGKILL (the OOM
// killer, when the agent did not send it).
func killedBySIGKILL(ps *exec.ExitError) bool {
	st, ok := ps.Sys().(syscall.WaitStatus)
	return ok && st.Signaled() && st.Signal() == syscall.SIGKILL
}

// ignoreSessionSignals: a lost SSH session must not kill the agent before
// it has stopped its workers and marked their jobs (SIGHUP), and a closed
// stdout must give write errors instead of SIGPIPE.
func ignoreSessionSignals() { signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE) }

func diskFreeMB(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize) >> 20, true
}
