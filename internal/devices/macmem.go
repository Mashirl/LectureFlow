// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// mac-opti M6: the CPU and memory readings of macOS, one measure for the
// resource bar (internal/resources reads them) and for the scheduler's
// memory admission (availableMemoryMBDarwin, memory_darwin.go). The Mach
// host statistics are read without cgo (machstat_darwin.go); this file is
// pure, so it is tested everywhere.
//
// The memory is the unified memory on Apple silicon: the total is
// hw.memsize — the same number as hwdetect's RAMMB / UnifiedMemMB (sysctl
// -n hw.memsize) — and "in use" is Activity Monitor's "已使用内存": app
// memory (internal − purgeable pages) + wired + compressed. What is not in
// use is what the admission may count as available.

// MacCPUTicks is host_statistics' HOST_CPU_LOAD_INFO: the ticks of all
// cores in CPU_STATE_USER, SYSTEM, IDLE and NICE.
type MacCPUTicks [4]uint32

const macCPUStateIdle = 2 // CPU_STATE_IDLE

// MacCPUPct is the busy share (user + system + nice) of the ticks between
// two readings, in percent; nil when no tick passed. The counters are
// 32-bit and wrap.
func MacCPUPct(prev, cur MacCPUTicks) *float64 {
	var busy, total uint64
	for i := range cur {
		d := uint64(cur[i] - prev[i])
		total += d
		if i != macCPUStateIdle {
			busy += d
		}
	}
	if total == 0 {
		return nil
	}
	v := float64(busy) * 100 / float64(total)
	return &v
}

// MacVM is the part of vm_statistics64 (HOST_VM_INFO64) the memory reading
// uses, in pages.
type MacVM struct {
	Internal   uint64 // internal_page_count: anonymous memory ("Anonymous pages" of vm_stat)
	Purgeable  uint64 // purgeable_count
	Wire       uint64 // wire_count
	Compressor uint64 // compressor_page_count: pages the compressor occupies
}

// MacMemUsedBytes is Activity Monitor's "已使用内存": app memory (internal
// − purgeable) + wired + compressed, in bytes.
func MacMemUsedBytes(v MacVM, pageSize uint64) uint64 {
	app := uint64(0)
	if v.Internal > v.Purgeable {
		app = v.Internal - v.Purgeable
	}
	return (app + v.Wire + v.Compressor) * pageSize
}
