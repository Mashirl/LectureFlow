// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package secrets

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// osProtection: Windows protects the key file with DPAPI.
var osProtection = ProtectionDPAPI

// dpapiEntropy is DPAPI's optional entropy: another program of the same
// account calling CryptUnprotectData without it cannot open the blob.
var dpapiEntropy = []byte("LectureFlow data key v1")

const dpapiDescription = "LectureFlow data key"

func dataBlob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

// takeBlob copies DPAPI's output and frees it (LocalFree), wiping the
// system's copy first.
func takeBlob(out *windows.DataBlob) []byte {
	if out.Data == nil {
		return nil
	}
	buf := unsafe.Slice(out.Data, out.Size)
	b := append([]byte(nil), buf...)
	clear(buf)
	windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return b
}

// protect encrypts plain with DPAPI for the current account (not the
// machine: CRYPTPROTECT_LOCAL_MACHINE is not set), never showing a prompt.
func protect(plain []byte) ([]byte, error) {
	desc, err := windows.UTF16PtrFromString(dpapiDescription)
	if err != nil {
		return nil, err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(dataBlob(plain), desc, dataBlob(dpapiEntropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI CryptProtectData: %w", err)
	}
	return takeBlob(&out), nil
}

// unprotect decrypts a protect blob; it fails for another account or
// computer, or a changed blob.
func unprotect(blob []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(dataBlob(blob), nil, dataBlob(dpapiEntropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI CryptUnprotectData: %w", err)
	}
	return takeBlob(&out), nil
}
