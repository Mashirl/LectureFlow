// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mkdirSDDL creates dir with the security descriptor sddl.
func mkdirSDDL(t *testing.T, dir, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(dir)
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		t.Fatal(err)
	}
}

// A data directory like one created under D:\ (Authenticated Users may
// modify it), or an updates directory another account may change, is not
// trusted.
func TestCheckTrustLooseWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	data := filepath.Join(privateDataDir(t), "D")
	mkdirSDDL(t, data, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;AU)")
	err = CheckTrust(filepath.Join(data, "updates"))
	if CodeOf(err) != CodeUntrustedDir || !strings.Contains(InfoOf(err).Params["who"].(string), "Authenticated Users") && !strings.Contains(InfoOf(err).Params["who"].(string), "S-1-5-11") {
		t.Fatalf("loose data directory: %v %+v", err, InfoOf(err).Params)
	}
	// private data directory, but its updates directory lets others in
	data2 := privateDataDir(t)
	mkdirSDDL(t, filepath.Join(data2, "updates"), "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;WD)")
	if err := CheckTrust(filepath.Join(data2, "updates")); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("loose updates directory: %v", err)
	}
	os.RemoveAll(data)
}
