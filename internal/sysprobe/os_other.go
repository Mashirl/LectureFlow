// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !linux && !darwin

package sysprobe

// Other systems (Windows: the agent runs here only in the tests of this
// repository; a Windows remote is plan step 5): only what the standard
// library knows. nvidia-smi still works where it is installed.

import (
	"context"
	"runtime"
)

func memInfo(context.Context) (total, avail int, ok bool) { return 0, 0, false }

type cpuState struct{}

func (cpuState) percent(context.Context) int { return -1 }

func processTable(context.Context) (map[int]procEntry, bool) { return nil, false }

func osInfo(context.Context) (osName, cpu string) { return runtime.GOOS, "" }

func appleGPU(context.Context) (string, bool)  { return "", false }
func appleGPUUtil(context.Context) (int, bool) { return 0, false }
