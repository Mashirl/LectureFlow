// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset is where the locked byte lies: far beyond the lock file's few
// bytes (reading or copying the file never touches it), at the offset
// SQLite itself locks, which every Windows file system accepts.
const lockOffset = 0x40000000

func lockRange() *windows.Overlapped { return &windows.Overlapped{Offset: lockOffset} }

// lockFile takes the exclusive lock without waiting; errLockHeld when
// another handle holds it (in this process too).
func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRange())
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return errLockHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange())
}
