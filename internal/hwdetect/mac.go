// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

// macOS probes and their parsers. No build constraint: the parsers are pure
// and the assembly takes an injected Runner, so they are tested everywhere.

import (
	"context"
	"strconv"
	"strings"
)

func detectDarwin(ctx context.Context, hw *HW, o Options) {
	if out, err := o.run(ctx, o.Timeout, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
		hw.CPU = ParseSysctlString(string(out))
	}
	if out, err := o.run(ctx, o.Timeout, "sysctl", "-n", "hw.memsize"); err == nil {
		hw.RAMMB = ParseMemsizeMB(string(out))
	}
	// hw.optional.arm64 is 1 on Apple Silicon (also for an x86_64 process
	// under Rosetta); the key does not exist on Intel Macs (sysctl fails).
	if out, err := o.run(ctx, o.Timeout, "sysctl", "-n", "hw.optional.arm64"); err == nil {
		hw.AppleSilicon = ParseSysctlBool(string(out))
	}
	if !hw.AppleSilicon && strings.HasPrefix(hw.CPU, "Apple ") {
		hw.AppleSilicon = true
	}
	if hw.AppleSilicon {
		hw.Chip = hw.CPU
	}
	if out, err := o.run(ctx, o.Timeout, "pmset", "-g", "batt"); err == nil {
		hw.HasBattery = ParsePmsetBattery(string(out))
	}
}

// ParseSysctlString is the trimmed value of `sysctl -n <string key>`.
func ParseSysctlString(out string) string {
	return strings.Join(strings.Fields(out), " ")
}

// ParseMemsizeMB parses `sysctl -n hw.memsize` (bytes) into MiB; 0 = unknown.
func ParseMemsizeMB(out string) int {
	v, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return int(v / bytesPerMB)
}

// ParseSysctlBool parses a 0/1 sysctl value.
func ParseSysctlBool(out string) bool {
	return strings.TrimSpace(out) == "1"
}

// ParsePmsetBattery reports whether `pmset -g batt` lists an internal
// battery. A desktop Mac prints only the power source line
// ("Now drawing from 'AC Power'"); a laptop adds a line starting with
// " -InternalBattery-0".
func ParsePmsetBattery(out string) bool {
	return strings.Contains(out, "InternalBattery")
}
