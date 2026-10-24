// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package colab

import (
	"os/exec"
	"time"
)

func hideWindow(cmd *exec.Cmd) { cmd.WaitDelay = time.Second }
