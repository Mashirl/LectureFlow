// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import "testing"

// mac-opti M6: the macOS CPU and memory measures (macmem.go).

func TestMacCPUPct(t *testing.T) {
	for _, c := range []struct {
		name      string
		prev, cur MacCPUTicks
		want      any
	}{
		{"busy share", MacCPUTicks{100, 50, 1000, 0}, MacCPUTicks{400, 150, 1600, 0}, 40.0}, // 400 of 1000
		{"nice counts", MacCPUTicks{0, 0, 0, 0}, MacCPUTicks{0, 0, 75, 25}, 25.0},
		{"idle", MacCPUTicks{10, 10, 10, 0}, MacCPUTicks{10, 10, 210, 0}, 0.0},
		{"no tick", MacCPUTicks{5, 5, 5, 5}, MacCPUTicks{5, 5, 5, 5}, nil},
		{"wrapped counter", MacCPUTicks{0xFFFFFFF0, 0, 0, 0}, MacCPUTicks{0x10, 0, 32, 0}, 50.0}, // 32 busy, 32 idle
	} {
		got := MacCPUPct(c.prev, c.cur)
		switch w := c.want.(type) {
		case nil:
			if got != nil {
				t.Errorf("%s: %v, want nil", c.name, *got)
			}
		case float64:
			if got == nil || *got != w {
				t.Errorf("%s: %v, want %v", c.name, got, w)
			}
		}
	}
}

// The numbers of a vm_stat run on the 16 GB Apple M6 (16 KiB pages):
// anonymous 271451, purgeable 5975, wired 105624, occupied by the
// compressor 1338 → 372438 pages ≈ 5.68 GiB, as Activity Monitor's
// "已使用内存".
func TestMacMemUsedBytes(t *testing.T) {
	v := MacVM{Internal: 271451, Purgeable: 5975, Wire: 105624, Compressor: 1338}
	if got := MacMemUsedBytes(v, 16384); got != 372438*16384 {
		t.Fatalf("used %d", got)
	}
	// more purgeable than internal pages (never seen; no underflow)
	if got := MacMemUsedBytes(MacVM{Internal: 1, Purgeable: 5, Wire: 2}, 4096); got != 2*4096 {
		t.Fatalf("used %d", got)
	}
}
