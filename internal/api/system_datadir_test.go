// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"lectureflow/internal/dirperm"
)

func dataDirRow(t *testing.T, e *env) SystemCheck {
	t.Helper()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if c.Name == checkDataDir {
			return c
		}
	}
	t.Fatal("no data_dir row")
	return SystemCheck{}
}

// fakeDataDirAccess makes dirperm.Check of the data directory answer *e
// (and Restrict record its calls) for the test.
func fakeDataDirAccess(t *testing.T, e *dirperm.Exposure, restrictErr *error) *int {
	t.Helper()
	oc, or := checkDataDirAccess, restrictDataDir
	calls := 0
	checkDataDirAccess = func(string) (dirperm.Exposure, error) { return *e, nil }
	restrictDataDir = func(string) error {
		calls++
		return *restrictErr
	}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	t.Cleanup(func() {
		checkDataDirAccess, restrictDataDir = oc, or
		dataDirAccess.mu.Lock()
		dataDirAccess.dir = ""
		dataDirAccess.mu.Unlock()
	})
	return &calls
}

// The data_dir row warns while other accounts can change or read the data
// directory; "收紧权限" (POST /api/system/data-dir/restrict) makes it
// private, or says why it could not.
func TestDataDirAccessRowAndRestrict(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	exp := dirperm.Exposure{}
	var rerr error
	calls := fakeDataDirAccess(t, &exp, &rerr)
	abs, _ := filepath.Abs(e.cfg.DataDir)

	if c := dataDirRow(t, e); c.Status != statusOK || c.DetailInfo == nil || c.DetailInfo.Code != "check.dir_writable" || c.HintInfo != nil {
		t.Fatalf("private: %+v", c)
	}

	exp = dirperm.Exposure{Write: true, Read: true, Who: []string{"Authenticated Users", "Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = "" // past the cache
	dataDirAccess.mu.Unlock()
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.DetailInfo.Params["path"] != abs ||
		c.DetailInfo.Params["who"] != "Authenticated Users, Users" || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirRestrict {
		t.Fatalf("shared: %+v %+v %+v", c, c.DetailInfo, c.HintInfo)
	}
	exp = dirperm.Exposure{Read: true, Who: []string{"Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if c := dataDirRow(t, e); c.Status != statusWarn || c.DetailInfo.Code != codeDataDirReadable {
		t.Fatalf("readable: %+v", c)
	}

	// the permissions cannot be set
	rerr = errors.New("access denied")
	info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusInternalServerError)
	if info.Code != codeDataDirRestrictFailed || info.Params["path"] != abs || info.Params["error"] != "access denied" {
		t.Fatalf("failed: %+v", info)
	}
	// set, but others still get in (FAT, another owner)
	rerr = nil
	info = e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed {
		t.Fatalf("still exposed: %+v", info)
	}
	// done
	checkDataDirAccess = func(string) (dirperm.Exposure, error) { return dirperm.Exposure{}, nil }
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Name != checkDataDir || out.Check.Status != statusOK || *calls != 3 {
		t.Fatalf("restricted: %+v (%d calls)", out.Check, *calls)
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v", c)
	}
}
