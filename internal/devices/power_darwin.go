// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

var pmsetBattCmd = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pmset", "-g", "batt")
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func readPower() Power {
	out, err := pmsetBattCmd()
	if err != nil {
		return Power{}
	}
	return PowerFromPmset(string(out))
}

func availableMemoryMB() (int, error) {
	return 0, errors.New("devices: available memory is only read on Windows")
}
