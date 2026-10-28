// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A data directory other accounts may change (like one made under D:\,
// where Authenticated Users may modify): exit 3, whatever the files say;
// no copy is made.
func TestSelfUpdateVerifyUntrustedWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(filepath.Dir(updatesDir(t)), "loose")
	p, _ := windows.UTF16PtrFromString(data)
	if err := windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "updates")
	os.Mkdir(dir, 0o755)
	k, _ := writePending(t, dir)
	useKey(t, k)
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-copy", tmp},
		{"-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"},
	} {
		code, out := runVerify(t, args...)
		if code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") || strings.Count(out, "\n") != 1 {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("a copy was made: %v", ents)
	}
}
