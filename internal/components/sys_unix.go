// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package components

import (
	"errors"
	"os/exec"
	"syscall"
)

// diskFree returns the bytes available to this user on the volume of path.
func diskFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

const exeSuffix = ""

// venvPython is the interpreter of a venv directory.
func venvPython(venv string) string { return venv + "/bin/python" }

func hideWindow(cmd *exec.Cmd) {}
