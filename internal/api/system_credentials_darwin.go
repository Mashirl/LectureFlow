// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package api

import (
	"strings"

	"golang.org/x/sys/unix"
)

// fsType names the file system holding dir (statfs f_fstypename); noACL
// for msdos (FAT) and exfat, which keep no owners or permissions.
func fsType(dir string) (name string, noACL bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", false
	}
	b := st.Fstypename[:]
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	switch name = string(b); name {
	case "msdos":
		return "FAT", true
	case "exfat":
		return "exFAT", true
	}
	return name, false
}
