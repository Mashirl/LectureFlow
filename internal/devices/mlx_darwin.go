// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// detectMLX reports mlx:gpu on Apple Silicon when the MLX interpreter
// (config python_mlx) exists. The label is the chip name
// (`sysctl -n machdep.cpu.brand_string`, e.g. "Apple M3 Pro"), else
// MLXLabelDefault. Whether MLX itself imports is the worker's business: a
// broken environment fails the run with ERROR_CODE device_unavailable.
func detectMLX(ctx context.Context, python string) (string, bool) {
	if runtime.GOARCH != "arm64" || !executableExists(python) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output()
	if name := strings.TrimSpace(string(out)); err == nil && strings.HasPrefix(name, "Apple") {
		return name, true
	}
	return MLXLabelDefault, true
}
