// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package api

// ownJobMemoryLimit: no Job Objects on this system.
func ownJobMemoryLimit() uint64 { return 0 }

// wantOSJobLimit: the OS sets no cap here (the worker limits itself).
func wantOSJobLimit(uint64) uint64 { return 0 }
