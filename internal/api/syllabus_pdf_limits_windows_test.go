// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownJobMemoryLimit is the per-process memory limit of the job this process
// is in (0: none).
func ownJobMemoryLimit() uint64 {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY == 0 {
		return 0
	}
	return uint64(info.ProcessMemoryLimit)
}

// wantOSJobLimit: the extraction runs in a Job Object with the cap.
func wantOSJobLimit(limit uint64) uint64 { return limit }
