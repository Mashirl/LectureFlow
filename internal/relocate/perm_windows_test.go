// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"lectureflow/internal/dirperm"
)

var autoInherited = regexp.MustCompile(`^D:(P?)AI`)

// permOf is dir's DACL in SDDL (without the "automatically inherited"
// flag, which Windows adds when a DACL is written back).
func permOf(t *testing.T, dir string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if i := strings.Index(s, "D:"); i >= 0 {
		s = s[i:]
	}
	return autoInherited.ReplaceAllString(s, "D:$1")
}

// loosen gives dir the permissions of a folder made under D:\
// (Authenticated Users may modify).
func loosen(t *testing.T, dir string) {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if ex, _ := dirperm.Check(dir); !ex.Write {
		t.Fatalf("the loosened directory is not exposed: %+v", ex)
	}
}

func assertPrivate(t *testing.T, dir string) {
	t.Helper()
	got := permOf(t, dir)
	if !strings.HasPrefix(got, "D:P(") || strings.Contains(got, ";AU)") || strings.Contains(got, ";WD)") || strings.Contains(got, ";BU)") {
		t.Fatalf("%s: DACL %s", dir, got)
	}
	if ex, err := dirperm.Check(dir); err != nil || ex.Exposed() {
		t.Fatalf("%s: %+v %v", dir, ex, err)
	}
}
