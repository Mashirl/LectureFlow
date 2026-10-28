// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package sysprobe

import (
	"context"
	"runtime"
	"strconv"
	"strings"
)

func memInfo(ctx context.Context) (total, avail int, ok bool) {
	out, err := run(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || b <= 0 {
		return 0, 0, false
	}
	vm, err := run(ctx, "/usr/bin/vm_stat")
	if err != nil {
		return int(b >> 20), -1, true
	}
	a, ok := parseVMStat(vm)
	if !ok {
		a = -1
	}
	return int(b >> 20), a, true
}

type cpuState struct{}

func (cpuState) percent(ctx context.Context) int {
	out, err := run(ctx, "/bin/ps", "-A", "-o", "%cpu=")
	if err != nil {
		return -1
	}
	pct, ok := parsePSCPU(out, runtime.NumCPU())
	if !ok {
		return -1
	}
	return pct
}

func processTable(ctx context.Context) (map[int]procEntry, bool) {
	out, err := run(ctx, "/bin/ps", "-A", "-o", "pid=,ppid=,rss=")
	if err != nil {
		return nil, false
	}
	return parsePSTable(out), true
}

func osInfo(ctx context.Context) (osName, cpu string) {
	osName = "macOS"
	if out, err := run(ctx, "/usr/bin/sw_vers", "-productVersion"); err == nil {
		osName += " " + strings.TrimSpace(out)
	}
	if out, err := run(ctx, "/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
		cpu = strings.TrimSpace(out)
	}
	return osName, cpu
}

func appleGPU(ctx context.Context) (string, bool) {
	out, err := run(ctx, "/usr/sbin/system_profiler", "-json", "SPDisplaysDataType")
	if err == nil {
		if name, ok := parseAppleGPU(out); ok {
			return name, true
		}
	}
	if out, err := run(ctx, "/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string"); err == nil && strings.HasPrefix(out, "Apple") {
		return cleanName(out), true
	}
	return "", false
}

func appleGPUUtil(ctx context.Context) (int, bool) {
	out, err := run(ctx, "/usr/sbin/ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	if err != nil {
		return 0, false
	}
	return parseIORegUtil(out)
}
