// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package resources

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// The upper area's readers on Windows (system.go): one PDH query with the
// CPU counter (CPUCounter, else CPUCounterFallback) and ThermalCounter,
// and GlobalMemoryStatusEx. None needs administrator rights; a counter
// that cannot be added is left out (its value stays null).

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx is MEMORYSTATUSEX (sysinfoapi.h).
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readMemStatus() (totalPhys, availPhys uint64, err error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx: %w", e)
	}
	return m.TotalPhys, m.AvailPhys, nil
}

type pdhSystemReader struct {
	query, cpu, thermal uintptr // a counter is 0 when it could not be added
	collected           bool    // one collection done (the CPU rate is known from the next)
}

func newSystemReader() (systemReader, error) {
	q, err := pdhOpenQuery()
	if err != nil {
		return nil, err
	}
	r := &pdhSystemReader{query: q}
	cpu, errCPU := pdhAddCounter(q, CPUCounter)
	if errCPU != nil {
		cpu, errCPU = pdhAddCounter(q, CPUCounterFallback)
	}
	r.cpu = cpu
	thermal, errThermal := pdhAddCounter(q, ThermalCounter)
	r.thermal = thermal
	if r.cpu == 0 && r.thermal == 0 {
		r.Close()
		return nil, errors.Join(errCPU, errThermal)
	}
	return r, nil
}

// Read collects once: CPU nil on the first collection; Zones nil without
// the thermal counter set.
func (r *pdhSystemReader) Read() (sysReading, error) {
	if r.query == 0 {
		return sysReading{}, fmt.Errorf("pdh query closed")
	}
	primed := r.collected
	r.collected = true
	if st, _, _ := procPdhCollectQueryData.Call(r.query); st != 0 {
		if uint32(st) == pdhNoData {
			return sysReading{}, nil
		}
		return sysReading{}, fmt.Errorf("PdhCollectQueryData: 0x%08X", uint32(st))
	}
	var out sysReading
	if r.cpu != 0 && primed {
		if v, ok := pdhSingle(r.cpu); ok {
			out.CPU = &v
		}
	}
	if r.thermal != 0 {
		if zs, ok, err := pdhArray(r.thermal); err == nil && ok {
			out.Zones = zs
		}
	}
	return out, nil
}

func (r *pdhSystemReader) Close() {
	if r.query != 0 {
		procPdhCloseQuery.Call(r.query)
		r.query, r.cpu, r.thermal = 0, 0, 0
	}
}
