// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// mac-opti M6: the real Mach readers on this Mac (they only read the
// system's counters).

func TestReadMacMemory(t *testing.T) {
	total, used, err := ReadMacMemory()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		t.Fatal(err)
	}
	if want, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err != nil || total != want {
		t.Fatalf("total %d, sysctl says %s (the padded sysctl bytes)", total, out)
	}
	if used == 0 || used > total {
		t.Fatalf("used %d of %d", used, total)
	}
	avail, err := availableMemoryMBDarwin()
	if err != nil || avail <= 0 || uint64(avail) > total>>20 {
		t.Fatalf("available %d MB of %d MB: %v", avail, total>>20, err)
	}
	t.Logf("unified memory %d MB, in use %d MB, available %d MB", total>>20, used>>20, avail)
}

func TestReadMacCPUTicks(t *testing.T) {
	a, err := ReadMacCPUTicks()
	if err != nil {
		t.Fatal(err)
	}
	if a[0]+a[1]+a[2]+a[3] == 0 {
		t.Fatalf("no ticks %v", a)
	}
}
