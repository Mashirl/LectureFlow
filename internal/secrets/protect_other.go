// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package secrets

// osProtection: Go has no credential store here (the macOS Keychain is the
// desktop shell's, through safeStorage; calling it from Go needs cgo). The
// key file is a 0600 file.
var osProtection = ProtectionNone

func protect([]byte) ([]byte, error) { return nil, errNoOSProtection }

func unprotect([]byte) ([]byte, error) {
	return nil, errNoOSProtection
}
