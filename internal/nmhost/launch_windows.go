// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts cmd outside the host's job object when the job
// allows it (Firefox puts native hosts in a job and kills the job when the
// port closes; it allows breaking away). A job that refuses breakaway
// fails CreateProcess with access denied: then the app starts inside it,
// as the same command (environment and standard streams included; a Cmd
// whose Start failed cannot be started again).
func startDetached(cmd *exec.Cmd) error {
	base := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base | windows.CREATE_BREAKAWAY_FROM_JOB}
	err := cmd.Start()
	if err == nil {
		return nil
	}
	retry := &exec.Cmd{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir,
		Stdin: cmd.Stdin, Stdout: cmd.Stdout, Stderr: cmd.Stderr, ExtraFiles: cmd.ExtraFiles}
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base}
	if err2 := retry.Start(); err2 != nil {
		return err
	}
	*cmd = *retry
	return nil
}

// ReadOnlyVolume is false on Windows (UnstableLauncher is a macOS rule).
func ReadOnlyVolume(string) bool { return false }
