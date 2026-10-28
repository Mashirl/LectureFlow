// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build linux

package api

import "golang.org/x/sys/unix"

// fsType names the file system holding dir when it keeps no permissions
// (FAT, exFAT: statfs f_type); other file systems are not named.
func fsType(dir string) (name string, noACL bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", false
	}
	switch uint64(st.Type) {
	case unix.MSDOS_SUPER_MAGIC:
		return "FAT", true
	case unix.EXFAT_SUPER_MAGIC:
		return "exFAT", true
	}
	return "", false
}
