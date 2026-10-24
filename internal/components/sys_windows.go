// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"errors"
	"os/exec"
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFree returns the bytes available to this user on the volume of path.
func diskFree(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, e
	}
	return int64(avail), nil
}

// ERROR_HANDLE_DISK_FULL (39) and ERROR_DISK_FULL (112).
func isDiskFull(err error) bool {
	var en syscall.Errno
	return errors.As(err, &en) && (en == 39 || en == 112)
}

const exeSuffix = ".exe"

// venvPython is the interpreter of a venv directory.
func venvPython(venv string) string { return venv + `\Scripts\python.exe` }

const createNoWindow = 0x08000000

// hideWindow keeps console programs (uv, python) from opening a window when
// the service runs without a console (desktop mode).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
