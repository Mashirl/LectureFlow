// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package hwdetect

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func platformOptions() Options {
	return Options{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Run:        runCommand,
		DiskFreeMB: diskFreeMB,
	}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func diskFreeMB(dir string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) / bytesPerMB), nil
}
