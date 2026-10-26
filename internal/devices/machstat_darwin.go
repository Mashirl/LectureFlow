// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package devices

import (
	"encoding/binary"
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// The Mach host statistics without cgo (mac-opti M6, macmem.go; the
// application is built with CGO_ENABLED=0): libSystem's functions are
// called through assembly trampolines (machstat_darwin.s) and the
// runtime's libc call syscall.syscall, the way golang.org/x/sys/unix calls
// libSystem. None needs administrator rights.

//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics host_statistics "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics64 host_statistics64 "/usr/lib/libSystem.B.dylib"

var (
	libc_mach_host_self_trampoline_addr    uintptr
	libc_host_statistics_trampoline_addr   uintptr
	libc_host_statistics64_trampoline_addr uintptr
)

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// host_statistics flavors (mach/host_info.h).
const (
	hostCPULoadInfo = 3 // HOST_CPU_LOAD_INFO: host_cpu_load_info, 4 natural_t
	hostVMInfo64    = 4 // HOST_VM_INFO64: vm_statistics64
)

// vmStatistics64 is struct vm_statistics64 (mach/vm_statistics.h; 152
// bytes, HOST_VM_INFO64_COUNT = 38 integer_t).
type vmStatistics64 struct {
	FreeCount                          uint32
	ActiveCount                        uint32
	InactiveCount                      uint32
	WireCount                          uint32
	ZeroFillCount                      uint64
	Reactivations                      uint64
	Pageins                            uint64
	Pageouts                           uint64
	Faults                             uint64
	CowFaults                          uint64
	Lookups                            uint64
	Hits                               uint64
	Purges                             uint64
	PurgeableCount                     uint32
	SpeculativeCount                   uint32
	Decompressions                     uint64
	Compressions                       uint64
	Swapins                            uint64
	Swapouts                           uint64
	CompressorPageCount                uint32
	ThrottledCount                     uint32
	ExternalPageCount                  uint32
	InternalPageCount                  uint32
	TotalUncompressedPagesInCompressor uint64
}

// hostPort is mach_host_self(), taken once (every call adds a reference
// to the same port name).
var hostPort = sync.OnceValue(func() uintptr {
	r, _, _ := syscall_syscall(libc_mach_host_self_trampoline_addr, 0, 0, 0)
	return r
})

// ReadMacCPUTicks is host_statistics(HOST_CPU_LOAD_INFO): the ticks of all
// cores (MacCPUPct of two readings is the CPU load).
func ReadMacCPUTicks() (MacCPUTicks, error) {
	var t MacCPUTicks
	n := uint32(len(t))
	r, _, _ := syscall_syscall6(libc_host_statistics_trampoline_addr, hostPort(), hostCPULoadInfo,
		uintptr(unsafe.Pointer(&t)), uintptr(unsafe.Pointer(&n)), 0, 0)
	if r != 0 {
		return t, fmt.Errorf("host_statistics(HOST_CPU_LOAD_INFO): kern_return %d", r)
	}
	return t, nil
}

// readMacVM is host_statistics64(HOST_VM_INFO64), the pages MacMemUsedBytes uses.
func readMacVM() (MacVM, error) {
	var v vmStatistics64
	n := uint32(unsafe.Sizeof(v) / 4)
	r, _, _ := syscall_syscall6(libc_host_statistics64_trampoline_addr, hostPort(), hostVMInfo64,
		uintptr(unsafe.Pointer(&v)), uintptr(unsafe.Pointer(&n)), 0, 0)
	if r != 0 {
		return MacVM{}, fmt.Errorf("host_statistics64(HOST_VM_INFO64): kern_return %d", r)
	}
	return MacVM{Internal: uint64(v.InternalPageCount), Purgeable: uint64(v.PurgeableCount),
		Wire: uint64(v.WireCount), Compressor: uint64(v.CompressorPageCount)}, nil
}

// ReadMacMemory returns the physical (on Apple silicon: unified) memory,
// hw.memsize, and how much of it is in use by Activity Monitor's measure
// (MacMemUsedBytes), in bytes.
func ReadMacMemory() (total, used uint64, err error) {
	total, err = sysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	vm, err := readMacVM()
	if err != nil {
		return 0, 0, err
	}
	page, err := sysctlUint64("hw.pagesize")
	if err != nil || page == 0 {
		page = uint64(syscall.Getpagesize())
	}
	return total, min(MacMemUsedBytes(vm, page), total), nil
}

// sysctlUint64 reads a numeric sysctl. syscall.Sysctl returns the raw
// bytes as a string with one trailing NUL byte cut off, so a little-endian
// value ending in a zero byte comes back short: pad it again.
func sysctlUint64(name string) (uint64, error) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return 0, err
	}
	if len(s) == 0 || len(s) > 8 {
		return 0, fmt.Errorf("sysctl %s: %d bytes", name, len(s))
	}
	var buf [8]byte
	copy(buf[:], s)
	return binary.LittleEndian.Uint64(buf[:]), nil
}
