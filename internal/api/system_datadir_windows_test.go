// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// For real: a data directory with the permissions of a folder made under
// D:\ (Authenticated Users may modify) is reported; "收紧权限" makes it and
// what it holds private.
func TestDataDirRestrictWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(e.cfg.DataDir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(e.cfg.DataDir, "components")
	os.MkdirAll(inner, 0o755)
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || !strings.Contains(c.DetailInfo.Params["who"].(string), "Authenticated Users") && !strings.Contains(c.DetailInfo.Params["who"].(string), "S-1-5-11") {
		t.Fatalf("loose: %+v %+v", c, c.DetailInfo)
	}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK {
		t.Fatalf("restrict: %+v", out.Check)
	}
	sd2, err := windows.GetNamedSecurityInfo(inner, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if s := sd2.String(); strings.Contains(s, ";AU)") {
		t.Fatalf("what the data directory holds still lets others in: %s", s)
	}
}
