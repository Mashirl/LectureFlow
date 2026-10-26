// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package resources

import (
	"runtime"

	"lectureflow/internal/devices"
)

// The upper area's readers on macOS (mac-opti M6, apple.go): the Mach host
// statistics of devices.ReadMacCPUTicks and devices.ReadMacMemory (no cgo,
// no administrator rights). The CPU temperature cannot be read.

func newSystemReader() (systemReader, error) {
	if _, err := devices.ReadMacCPUTicks(); err != nil {
		return nil, err
	}
	return &machSystemReader{}, nil
}

// machSystemReader keeps the previous CPU ticks: the first Read has no
// CPU value (like the PDH rate counter on Windows). The kernel adds up the
// ticks of idle cores lazily (on an Apple M6 the sum sometimes stays the
// same for most of a second): a Read without a new tick keeps the previous
// ticks and repeats the last value.
type machSystemReader struct {
	prev   devices.MacCPUTicks
	primed bool
	last   *float64
}

func (r *machSystemReader) Read() (sysReading, error) {
	cur, err := devices.ReadMacCPUTicks()
	if err != nil {
		return sysReading{}, err
	}
	if !r.primed {
		r.prev, r.primed = cur, true
		return sysReading{}, nil
	}
	if v := devices.MacCPUPct(r.prev, cur); v != nil {
		r.prev, r.last = cur, v
	}
	return sysReading{CPU: r.last}, nil
}

func (r *machSystemReader) Close() {}

// readMemStatus: the physical (unified) memory and what is not in use by
// Activity Monitor's measure, as MemOf expects.
func readMemStatus() (totalPhys, availPhys uint64, err error) {
	total, used, err := devices.ReadMacMemory()
	if err != nil {
		return 0, 0, err
	}
	return total, total - used, nil
}

// platformReadings: no CPU temperature on macOS; the memory is unified on
// Apple silicon.
func platformReadings() ([]string, bool) {
	return []string{ReadingCPUTemp}, runtime.GOARCH == "arm64"
}
