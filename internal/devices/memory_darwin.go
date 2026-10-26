// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

// availableMemoryMBDarwin is AvailableMemoryMB on macOS (mac-opti M6; the
// lead wires it into power_darwin.go's availableMemoryMB): the unified
// memory not in use by Activity Monitor's measure — hw.memsize minus app
// memory, wired and compressed (ReadMacMemory, macmem.go) —, the same
// measure the resource bar shows as "统一内存 已用 / 总量". Like Windows'
// ullAvailPhys it counts the file cache and purgeable memory as available.
func availableMemoryMBDarwin() (int, error) {
	total, used, err := ReadMacMemory()
	if err != nil {
		return 0, err
	}
	return int((total - used) >> 20), nil
}
