// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package nmhost

import (
	"errors"
	"os/exec"
	"syscall"
)

// startDetached starts cmd in its own session, so the browser ending the
// host (and its process group) does not end the app.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// accessWrite is access(2) with W_OK.
const accessWrite = 0x2

// ReadOnlyVolume reports whether dir is on a read-only file system (a
// mounted disk image; UnstableLauncher).
func ReadOnlyVolume(dir string) bool {
	return errors.Is(syscall.Access(dir, accessWrite), syscall.EROFS)
}
