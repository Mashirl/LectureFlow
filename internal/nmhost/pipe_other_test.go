// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package nmhost

// runTestChild: the low integrity pipe server of pipe_windows_test.go
// exists only on Windows.
func runTestChild() (int, bool) { return 0, false }
