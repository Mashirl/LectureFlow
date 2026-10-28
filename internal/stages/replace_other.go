// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package stages

// fileInUse: on these systems an open file never stops its removal or
// replacement.
func fileInUse(error, string) bool { return false }
