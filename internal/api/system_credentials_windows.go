// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import (
	"strings"

	"golang.org/x/sys/windows"
)

// fsType names the file system of the volume holding dir; noACL for FAT,
// FAT32 and exFAT, which keep no access permissions.
func fsType(dir string) (name string, noACL bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", false
	}
	root := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &root[0], uint32(len(root))); err != nil {
		return "", false
	}
	fs := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(&root[0], nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return "", false
	}
	name = windows.UTF16ToString(fs)
	return name, isNoACLFS(name)
}

func isNoACLFS(name string) bool {
	switch strings.ToUpper(name) {
	case "FAT", "FAT12", "FAT16", "FAT32", "EXFAT", "VFAT", "MSDOS":
		return true
	}
	return false
}
