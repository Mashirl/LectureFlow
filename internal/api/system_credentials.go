// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// The stored credentials and where the data directory lives (cred-enc,
// user decision 2026-10-27; security review 2 "数据库明文凭据", L5). Two live
// rows of the diagnostics, after data_dir (withDataDirAccess adds them):
//
//   - credentials: how the data key of the stored API keys and the Colab
//     authorisation is protected (internal/secrets): Windows DPAPI or the
//     macOS Keychain (ok), a plain 0600 key file (warning: macOS web /
//     development mode), no key at all (error: stored unsealed). A warning
//     too while stored credentials cannot be decrypted with this
//     installation's key (the data directory came from another computer or
//     account): they have to be entered again.
//   - data_dir_location, only when there is something to say: the data
//     directory is in a folder a cloud service syncs (OneDrive, iCloud
//     Drive, Dropbox, Google Drive: the database and the key file go to the
//     cloud and to the other devices), or on a FAT / exFAT drive (no access
//     permissions: every account of the computer can read it).
const (
	checkCredentials     = "credentials"
	checkDataDirLocation = "data_dir_location"

	codeCredentialsDPAPI       = "check.credentials_dpapi"
	codeCredentialsKeychain    = "check.credentials_keychain"
	codeCredentialsFile        = "check.credentials_file"
	codeCredentialsNone        = "check.credentials_none"
	codeCredentialsUnavailable = "check.credentials_unavailable"
	codeCredentialsUnreadable  = "check.credentials_unreadable"
	codeCredentialsReplaced    = "check.credentials_replaced"
	codeHintCredentialsReenter = "check.hint_credentials_reenter"
	codeHintCredentialsNone    = "check.hint_credentials_none"
	codeDataDirCloudSync       = "check.data_dir_cloud_sync"
	codeDataDirNoACL           = "check.data_dir_no_acl"
	codeHintDataDirMove        = "check.hint_data_dir_move"

	credentialRowsCacheFor = 5 * time.Second
)

var credentialCheckCodes = map[string]errDef{
	codeCredentialsDPAPI:       {"保存的 API key 与 Colab 授权已加密，密钥由 Windows 数据保护（DPAPI）绑定在当前账户上", nil},
	codeCredentialsKeychain:    {"保存的 API key 与 Colab 授权已加密，密钥保存在 macOS 钥匙串中", nil},
	codeCredentialsFile:        {"保存的 API key 与 Colab 授权已加密，但密钥只是数据目录里的文件 {path}（仅当前账户可读），没有系统凭据库保护", []string{"path"}},
	codeCredentialsNone:        {"没能建立加密密钥，API key 与 Colab 授权以明文保存：{error}", []string{"error"}},
	codeCredentialsUnavailable: {"系统凭据库现在不可用（{error}）：本次没有使用已保存的 API key 与 Colab 授权，密钥文件保持不变，下次启动会再试；现在填写的 key 会以明文保存", []string{"error"}},
	codeCredentialsUnreadable:  {"有 {n} 项保存的凭据无法在这台电脑或这个账户上解密（数据目录可能来自别的电脑或账户），需要重新输入", []string{"n"}},
	codeCredentialsReplaced:    {"这台电脑或这个账户打不开原来的密钥文件，已生成新密钥（原文件保留为 {path}）；有 {n} 项保存的凭据需要重新输入", []string{"path", "n"}},
	codeHintCredentialsReenter: {"在设置页的“提供商”中为标有“需要重新输入”的提供商填写 API key；Colab 需要重新授权", nil},
	codeHintCredentialsNone:    {"查看日志 logs/lectureflow.log 中以 credentials 开头的记录，排除问题后重启 LectureFlow", nil},
	codeDataDirCloudSync:       {"数据目录 {path} 在 {service} 的同步文件夹中：数据库（含加密后的 API key）与密钥文件会上传到云端并同步到其他设备", []string{"path", "service"}},
	codeDataDirNoACL:           {"数据目录 {path} 所在的磁盘是 {fs} 格式，这种格式不记录访问权限，本机的任何账户都能读取其中的文件", []string{"path", "fs"}},
	codeHintDataDirMove:        {"在设置页的“数据目录”中把它移到不会同步的本地 NTFS / APFS 磁盘（例如默认位置）", nil},
}

func init() {
	for code, def := range credentialCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// Replaced in tests.
var (
	credentialStatus = secrets.CurrentStatus
	dataDirFSType    = fsType // system_credentials_*.go
	userHome         = os.UserHomeDir
	lookupEnv        = os.Getenv
)

// credentialRows caches the rows (GET /api/system is polled; the kv check
// reads the database).
var credentialRows struct {
	mu   sync.Mutex
	dir  string
	at   time.Time
	rows []SystemCheck
}

// withCredentialRows inserts the credentials row (and data_dir_location
// when it warns) after the data_dir row (at the end without one).
func (s *Server) withCredentialRows(rows []SystemCheck) []SystemCheck {
	extra := s.credentialChecks(false)
	if len(extra) == 0 {
		return rows
	}
	at := len(rows)
	for i, c := range rows {
		if c.Name == checkDataDir {
			at = i + 1
			break
		}
	}
	out := make([]SystemCheck, 0, len(rows)+len(extra))
	out = append(out, rows[:at]...)
	out = append(out, extra...)
	return append(out, rows[at:]...)
}

func (s *Server) credentialChecks(fresh bool) []SystemCheck {
	credentialRows.mu.Lock()
	defer credentialRows.mu.Unlock()
	if !fresh && credentialRows.dir == s.dataDir && time.Since(credentialRows.at) < credentialRowsCacheFor {
		return append([]SystemCheck(nil), credentialRows.rows...)
	}
	var rows []SystemCheck
	if c, ok := s.credentialsCheck(); ok {
		rows = append(rows, c)
	}
	if c, ok := s.dataDirLocationCheck(); ok {
		rows = append(rows, c)
	}
	credentialRows.dir, credentialRows.at, credentialRows.rows = s.dataDir, time.Now(), rows
	return append([]SystemCheck(nil), rows...)
}

// unreadableCredentials counts the stored credentials no key opens: the
// providers from the settings cache, the Colab credential from the
// database (bounded: a long transaction must not stall the page).
func (s *Server) unreadableCredentials() int {
	n := 0
	if s.set != nil {
		for _, p := range s.set.Providers() {
			if s.set.KeyUnreadable(p.ID) {
				n++
			}
		}
	}
	if in, ok := s.st.(db.SecretInspector); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, kv, err := in.UnreadableSecrets(ctx); err == nil {
			n += len(kv)
		}
	}
	return n
}

// credentialsCheck is the credentials row; none when the data key was
// never set up in this process (tests and tools that open the API without
// cmd's startup).
func (s *Server) credentialsCheck() (SystemCheck, bool) {
	c := SystemCheck{Name: checkCredentials, Status: statusOK}
	st := credentialStatus()
	if st.Protection == "" {
		return c, false
	}
	if st.Protection == secrets.ProtectionNone {
		c.Status = statusError
		why := st.Err
		if why == "" {
			why = "-"
		}
		if st.Unavailable { // sec3 L4: nothing replaced, tried again next start
			c.setDetail(codeCredentialsUnavailable, map[string]any{"error": why})
		} else {
			c.setDetail(codeCredentialsNone, map[string]any{"error": why})
		}
		c.setHint(codeHintCredentialsNone, nil)
		return c, true
	}
	if n := s.unreadableCredentials(); n > 0 {
		c.Status = statusWarn
		if st.Replaced != "" {
			c.setDetail(codeCredentialsReplaced, map[string]any{"path": st.Replaced, "n": n})
		} else {
			c.setDetail(codeCredentialsUnreadable, map[string]any{"n": n})
		}
		c.setHint(codeHintCredentialsReenter, nil)
		return c, true
	}
	switch st.Protection {
	case secrets.ProtectionDPAPI:
		c.setDetail(codeCredentialsDPAPI, nil)
	case secrets.ProtectionKeychain:
		c.setDetail(codeCredentialsKeychain, nil)
	default: // ProtectionFile
		c.Status = statusWarn
		c.setDetail(codeCredentialsFile, map[string]any{"path": st.KeyFile})
	}
	return c, true
}

func (s *Server) dataDirLocationCheck() (SystemCheck, bool) {
	dir := s.absDataDir()
	if dir == "" {
		return SystemCheck{}, false
	}
	c := SystemCheck{Name: checkDataDirLocation, Status: statusWarn}
	home, _ := userHome()
	if svc := cloudSyncService(dir, home, lookupEnv, credPathExists); svc != "" {
		c.setDetail(codeDataDirCloudSync, map[string]any{"path": dir, "service": svc})
		c.setHint(codeHintDataDirMove, nil)
		return c, true
	}
	if fs, noACL := dataDirFSType(dir); noACL {
		c.setDetail(codeDataDirNoACL, map[string]any{"path": dir, "fs": fs})
		c.setHint(codeHintDataDirMove, nil)
		return c, true
	}
	return SystemCheck{}, false
}

func credPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// cloudSyncService names the cloud service whose sync folder holds dir
// ("" for none), from the folder names the clients use: OneDrive (also
// "OneDrive - Org" and the OneDrive* environment variables), iCloud Drive
// (Windows iCloudDrive, macOS Mobile Documents; macOS Desktop & Documents
// when they are synced), Dropbox, Google Drive ("My Drive", "我的云端硬盘",
// macOS GoogleDrive-…), and other macOS File Provider folders under
// ~/Library/CloudStorage.
func cloudSyncService(dir, home string, getenv func(string) string, exists func(string) bool) string {
	clean := filepath.Clean(dir)
	within := func(root string) bool {
		if root == "" {
			return false
		}
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			return false
		}
		return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	for _, v := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		if within(getenv(v)) {
			return "OneDrive"
		}
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	for i, part := range parts {
		if svc := syncFolderService(part); svc != "" {
			return svc
		}
		if strings.EqualFold(part, "CloudStorage") && i > 0 && strings.EqualFold(parts[i-1], "Library") && i+1 < len(parts) {
			if svc := syncFolderService(parts[i+1]); svc != "" {
				return svc
			}
			name, _, _ := strings.Cut(parts[i+1], "-") // ~/Library/CloudStorage/Box-Box
			return name
		}
	}
	// macOS "Desktop & Documents Folders" in iCloud Drive: the folders keep
	// their place; iCloud Drive then holds a Desktop and a Documents folder.
	if runtime.GOOS == "darwin" && home != "" {
		for _, f := range []string{"Desktop", "Documents"} {
			if within(filepath.Join(home, f)) && exists(filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", f)) {
				return "iCloud Drive"
			}
		}
	}
	return ""
}

// syncFolderService names the service of a sync client's folder name.
func syncFolderService(name string) string {
	p := strings.ToLower(name)
	switch {
	case p == "onedrive" || strings.HasPrefix(p, "onedrive - ") || strings.HasPrefix(p, "onedrive-"):
		return "OneDrive"
	case p == "icloud drive" || p == "iclouddrive" || p == "mobile documents" || p == "com~apple~clouddocs":
		return "iCloud Drive"
	case p == "dropbox" || strings.HasPrefix(p, "dropbox (") || strings.HasPrefix(p, "dropbox-"):
		return "Dropbox"
	case p == "google drive" || p == "my drive" || p == "我的云端硬盘" || p == "googledrive" || strings.HasPrefix(p, "googledrive-"):
		return "Google Drive"
	}
	return ""
}
