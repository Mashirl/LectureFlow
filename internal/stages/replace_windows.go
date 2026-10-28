// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package stages

import (
	"errors"

	"golang.org/x/sys/windows"
)

// fileInUse reports whether err (of removing, renaming or replacing path)
// means that another program has path open: a sharing or lock violation,
// a mapped file, or an "access denied" (what replacing an open file gives)
// for a path that cannot be opened for deletion because of a sharing
// violation. A plain "access denied" (a read-only file, the permissions)
// is not one.
func fileInUse(err error, path string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION),
		errors.Is(err, windows.ERROR_USER_MAPPED_FILE):
		return true
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return heldOpen(path)
	}
	return false
}

// heldOpen: path exists and opening it for deletion, sharing everything,
// fails with a sharing violation: a program has it open without allowing
// deletion.
func heldOpen(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err == nil {
		windows.CloseHandle(h)
		return false
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
