// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package secrets

import (
	"bytes"
	"testing"

	"golang.org/x/sys/windows"
)

// DPAPI round trip for the current account; a changed blob, or one made
// without LectureFlow's entropy, does not open.
func TestDPAPIRoundTrip(t *testing.T) {
	key := testKey(0x5a)
	blob, err := protect(key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, key) {
		t.Fatal("the blob holds the key")
	}
	got, err := unprotect(blob)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unprotect: %x %v", got, err)
	}
	mod := append([]byte(nil), blob...)
	mod[len(mod)-5] ^= 0x40
	if _, err := unprotect(mod); err == nil {
		t.Fatal("a changed blob opened")
	}
	if _, err := unprotect(mod[:10]); err == nil {
		t.Fatal("a truncated blob opened")
	}
	// without the entropy (another program of the same account)
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(dataBlob(blob), nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err == nil {
		takeBlob(&out)
		t.Fatal("opened without LectureFlow's entropy")
	}
}
