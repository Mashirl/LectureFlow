// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package devices

import (
	"syscall"
	"unsafe"
)

var (
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
)

// winbase.h / minwinbase.h values.
const (
	powerRequestContextVersion      = 0   // POWER_REQUEST_CONTEXT_VERSION
	powerRequestContextSimpleString = 0x1 // POWER_REQUEST_CONTEXT_SIMPLE_STRING
	powerRequestSystemRequired      = 1   // PowerRequestSystemRequired
	powerRequestExecutionRequired   = 3   // PowerRequestExecutionRequired
	invalidHandleValue              = ^uintptr(0)
)

// reasonContext is REASON_CONTEXT with the SimpleReasonString member of the
// union; the padding covers the larger Detailed member (HMODULE, two
// ULONGs, a pointer), so the struct is never smaller than the system's.
type reasonContext struct {
	Version uint32
	Flags   uint32
	Reason  *uint16
	_       [2]uintptr
}

func platformAwakeCreate(reason string) (uintptr, []uint16, error) {
	buf, err := syscall.UTF16FromString(reason)
	if err != nil {
		return 0, nil, err
	}
	rc := reasonContext{Version: powerRequestContextVersion, Flags: powerRequestContextSimpleString, Reason: &buf[0]}
	h, _, e := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
	if h == 0 || h == invalidHandleValue {
		return 0, nil, e
	}
	return h, buf, nil
}

func platformAwakeSet(h uintptr) error {
	if r, _, e := procPowerSetRequest.Call(h, powerRequestSystemRequired); r == 0 {
		return e
	}
	if r, _, e := procPowerSetRequest.Call(h, powerRequestExecutionRequired); r == 0 {
		procPowerClearRequest.Call(h, powerRequestSystemRequired)
		return e
	}
	return nil
}

func platformAwakeClear(h uintptr) error {
	r1, _, e1 := procPowerClearRequest.Call(h, powerRequestExecutionRequired)
	r2, _, e2 := procPowerClearRequest.Call(h, powerRequestSystemRequired)
	switch {
	case r1 == 0:
		return e1
	case r2 == 0:
		return e2
	}
	return nil
}

func platformAwakeClose(h uintptr) error {
	if h == 0 {
		return nil
	}
	if r, _, e := procCloseHandle.Call(h); r == 0 {
		return e
	}
	return nil
}
