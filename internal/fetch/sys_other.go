// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package fetch

import (
	"errors"
	"os/exec"
	"syscall"
)

func hideWindow(*exec.Cmd) {}

// diskFree returns the bytes available to unprivileged users on the
// filesystem of dir.
func diskFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }
