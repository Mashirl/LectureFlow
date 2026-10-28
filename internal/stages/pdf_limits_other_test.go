// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package stages

// tryCommit allocates n bytes (no OS cap is set up on this system: the
// worker limits itself).
func tryCommit(n uintptr) bool {
	b := make([]byte, n)
	b[len(b)-1] = 1
	return true
}
