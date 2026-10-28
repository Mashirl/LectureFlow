// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows && !linux

package procutil

import "os/exec"

// endWithParent: macOS (and the BSDs) have no parent death signal; a child
// can outlive a crash of this process. Its open files never block their
// removal on these systems, so the next start is not held up by it.
func endWithParent(*exec.Cmd) {}
