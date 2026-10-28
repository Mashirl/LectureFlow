// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
)

// Who may use the data directory (security review sec-update M2). The data
// directory holds the database with the providers' API keys, the Python
// environments the service runs and the in-app update's files; a data
// directory moved to a place like D:\ used to inherit "Authenticated Users:
// modify" from there. LectureFlow now makes the data directories it creates
// or moves private (internal/dirperm); one that already exists is never
// changed behind the user's back:
//
//   - the data_dir row of the diagnostics turns into a warning when other
//     accounts of this computer can change (check.data_dir_shared) or read
//     (check.data_dir_readable) the data directory, with the hint
//     check.hint_data_dir_restrict (live, like disk; cached a few seconds);
//   - POST /api/system/data-dir/restrict makes it private when the user
//     asks: the directory itself gets the private permissions, what lies
//     below inherits them. 200 {"check": the data_dir row}; 409
//     data_dir.restrict_failed when other accounts can still reach it
//     afterwards (another account owns the folder, or the drive keeps no
//     permissions: FAT, exFAT); 500 data_dir.restrict_failed when the
//     permissions cannot be set.
//
// While other accounts can change the data directory, the in-app update
// neither downloads nor installs anything (selfupdate.untrusted_dir).

// Codes of the data_dir row and of the endpoint (the API's catalog; init
// adds them).
const (
	codeDataDirShared         = "check.data_dir_shared"
	codeDataDirReadable       = "check.data_dir_readable"
	codeHintDataDirRestrict   = "check.hint_data_dir_restrict"
	codeDataDirRestrictFailed = "data_dir.restrict_failed"
	checkDataDir              = "data_dir"
	dataDirAccessCacheFor     = 5 * time.Second
)

var dataDirCheckCodes = map[string]errDef{
	codeDataDirShared:         {"{path} 可写，但本机的其他账户也能修改其中的文件（{who}）", []string{"path", "who"}},
	codeDataDirReadable:       {"{path} 可写，但本机的其他账户也能读取其中的文件（{who}）", []string{"path", "who"}},
	codeHintDataDirRestrict:   {"点击“收紧权限”，让数据目录只有你的账户（以及系统与管理员）可以访问", nil},
	codeDataDirRestrictFailed: {"没能收紧数据目录 {path} 的权限：{error}", []string{"path", "error"}},
}

func init() {
	for code, def := range dataDirCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("POST /api/system/data-dir/restrict", s.postRestrictDataDir)
	})
}

// dataDirAccess caches dirperm.Check of the data directory (GET /api/system
// is polled).
var dataDirAccess struct {
	mu  sync.Mutex
	dir string
	at  time.Time
	e   dirperm.Exposure
	err error
}

// checkDataDirAccess is dirperm.Check (tests replace it).
var checkDataDirAccess = dirperm.Check

func (s *Server) dataDirExposure(fresh bool) (dirperm.Exposure, error) {
	dataDirAccess.mu.Lock()
	defer dataDirAccess.mu.Unlock()
	if !fresh && dataDirAccess.dir == s.dataDir && time.Since(dataDirAccess.at) < dataDirAccessCacheFor {
		return dataDirAccess.e, dataDirAccess.err
	}
	e, err := checkDataDirAccess(s.dataDir)
	dataDirAccess.dir, dataDirAccess.at, dataDirAccess.e, dataDirAccess.err = s.dataDir, time.Now(), e, err
	return e, err
}

// withDataDirAccess turns an ok data_dir row into a warning while other
// accounts can reach the data directory (see the top of this file).
func (s *Server) withDataDirAccess(rows []SystemCheck) []SystemCheck {
	for i, c := range rows {
		if c.Name == checkDataDir && c.Status == statusOK {
			rows[i] = s.dataDirAccessRow(c, false)
		}
	}
	return s.withCredentialRows(rows) // cred-enc: credentials, data_dir_location (system_credentials.go)
}

func (s *Server) dataDirAccessRow(c SystemCheck, fresh bool) SystemCheck {
	e, err := s.dataDirExposure(fresh)
	if err != nil || !e.Exposed() {
		return c
	}
	abs, aerr := filepath.Abs(s.dataDir)
	if aerr != nil {
		abs = s.dataDir
	}
	params := map[string]any{"path": abs, "who": strings.Join(e.Who, ", ")}
	c.Status = statusWarn
	if e.Write {
		c.setDetail(codeDataDirShared, params)
	} else {
		c.setDetail(codeDataDirReadable, params)
	}
	c.setHint(codeHintDataDirRestrict, nil)
	return c
}

// restrictDataDir is dirperm.Restrict (tests replace it).
var restrictDataDir = dirperm.Restrict

// postRestrictDataDir: POST /api/system/data-dir/restrict.
func (s *Server) postRestrictDataDir(w http.ResponseWriter, r *http.Request) {
	abs, err := filepath.Abs(s.dataDir)
	if err != nil {
		abs = s.dataDir
	}
	fail := func(status int, err error) {
		writeErrorInfo(w, status, model.ErrorInfo{Code: codeDataDirRestrictFailed,
			Params: map[string]any{"path": abs, "error": err.Error()}, Detail: err.Error()})
	}
	if err := restrictDataDir(s.dataDir); err != nil {
		slog.Warn("api: restricting the data directory", "dir", abs, "err", err)
		fail(http.StatusInternalServerError, err)
		return
	}
	e, err := s.dataDirExposure(true)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	if e.Exposed() {
		err := &restrictIncomplete{who: e.Who}
		slog.Warn("api: the data directory is still reachable by other accounts", "dir", abs, "who", e.Who)
		fail(http.StatusConflict, err)
		return
	}
	slog.Info("api: the data directory admits this account only now", "dir", abs)
	c := SystemCheck{Name: checkDataDir, Status: statusOK}
	c.setDetail("check.dir_writable", map[string]any{"path": abs})
	writeJSON(w, http.StatusOK, map[string]any{"check": c})
}

type restrictIncomplete struct{ who []string }

func (e *restrictIncomplete) Error() string {
	return "other accounts can still reach it (" + strings.Join(e.who, ", ") +
		"): another account owns the folder, or the drive keeps no permissions (FAT, exFAT)"
}
