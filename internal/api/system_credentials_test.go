// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/secrets"
	"lectureflow/internal/settings"
)

// fakeCredentialStatus makes secrets.CurrentStatus answer *st for the test.
func fakeCredentialStatus(t *testing.T, st *secrets.Status) {
	t.Helper()
	old := credentialStatus
	credentialStatus = func() secrets.Status { return *st }
	clearCredentialRows()
	t.Cleanup(func() {
		credentialStatus = old
		clearCredentialRows()
	})
}

func clearCredentialRows() {
	credentialRows.mu.Lock()
	credentialRows.dir = ""
	credentialRows.mu.Unlock()
}

func systemRows(t *testing.T, e *env) []SystemCheck {
	t.Helper()
	clearCredentialRows()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	return sys.Checks
}

func rowNamed(rows []SystemCheck, name string) (SystemCheck, int) {
	for i, c := range rows {
		if c.Name == name {
			return c, i
		}
	}
	return SystemCheck{}, -1
}

// The credentials row follows data_dir: ok with DPAPI or the Keychain, a
// warning for a plain key file and while stored credentials cannot be
// decrypted (with the hint to enter them again), an error without a key;
// absent when the key was never set up (tests, tools).
func TestCredentialsRow(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	st := secrets.Status{}
	fakeCredentialStatus(t, &st)
	if _, i := rowNamed(systemRows(t, e), checkCredentials); i >= 0 {
		t.Fatal("a credentials row without a key setup")
	}

	st = secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: filepath.Join(e.cfg.DataDir, secrets.KeyFileName)}
	rows := systemRows(t, e)
	c, i := rowNamed(rows, checkCredentials)
	_, d := rowNamed(rows, checkDataDir)
	if i != d+1 || c.Status != statusOK || c.DetailInfo == nil || c.DetailInfo.Code != codeCredentialsDPAPI || c.HintInfo != nil {
		t.Fatalf("dpapi: at %d (data_dir %d) %+v", i, d, c)
	}
	st.Protection = secrets.ProtectionKeychain
	if c, _ := rowNamed(systemRows(t, e), checkCredentials); c.Status != statusOK || c.DetailInfo.Code != codeCredentialsKeychain {
		t.Fatalf("keychain: %+v", c)
	}
	st.Protection = secrets.ProtectionFile
	c, _ = rowNamed(systemRows(t, e), checkCredentials)
	if c.Status != statusWarn || c.DetailInfo.Code != codeCredentialsFile || c.DetailInfo.Params["path"] != st.KeyFile {
		t.Fatalf("file: %+v", c)
	}
	st = secrets.Status{Protection: secrets.ProtectionNone, Err: "DPAPI CryptProtectData: test failure"}
	c, _ = rowNamed(systemRows(t, e), checkCredentials)
	if c.Status != statusError || c.DetailInfo.Code != codeCredentialsNone || c.DetailInfo.Params["error"] != st.Err || c.HintInfo.Code != codeHintCredentialsNone {
		t.Fatalf("none: %+v", c)
	}
}

// Stored credentials sealed with another installation's key: the row
// warns with their number (and the kept key file after a replacement), and
// the provider view says the key has to be entered again — never with the
// key or the ciphertext.
func TestCredentialsRowUnreadableAndProviderView(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{1}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	const fake = "sk-test-api-cred-enc-5151"
	name, typ, base, key := "Elsewhere", "openai", "https://llm.example.org", fake
	p, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.SetKey(bytes.Repeat([]byte{2}, secrets.KeySize)); err != nil { // this computer's key
		t.Fatal(err)
	}
	if err := e.set.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: "x"}
	fakeCredentialStatus(t, &st)
	c, _ := rowNamed(systemRows(t, e), checkCredentials)
	if c.Status != statusWarn || c.DetailInfo.Code != codeCredentialsUnreadable || c.DetailInfo.Params["n"] != float64(1) || c.HintInfo.Code != codeHintCredentialsReenter {
		t.Fatalf("unreadable: %+v %+v", c, c.DetailInfo)
	}
	st.Replaced = filepath.Join(e.cfg.DataDir, secrets.KeyFileName+".unreadable-20261027-120000")
	c, _ = rowNamed(systemRows(t, e), checkCredentials)
	if c.DetailInfo.Code != codeCredentialsReplaced || c.DetailInfo.Params["path"] != st.Replaced {
		t.Fatalf("replaced: %+v", c.DetailInfo)
	}

	var views []ProviderView
	e.call("GET", "/api/providers", nil, http.StatusOK, &views)
	var v *ProviderView
	for i := range views {
		if views[i].ID == p.ID {
			v = &views[i]
		}
	}
	if v == nil || !v.KeyUnreadable || v.KeyHint != "" || v.KeyErrorInfo == nil || v.KeyErrorInfo.Code != settings.CodeProviderKeyUnreadable ||
		v.KeyErrorInfo.Params["provider"] != name {
		t.Fatalf("provider view %+v", v)
	}
	_, raw := e.do("GET", "/api/providers", nil)
	if strings.Contains(string(raw), fake) || strings.Contains(string(raw), secrets.Prefix) {
		t.Fatal("the provider list holds the key or the ciphertext")
	}
}

// The data directory in a cloud service's sync folder or on a drive that
// keeps no permissions.
func TestDataDirLocationRow(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	oldFS := dataDirFSType
	t.Cleanup(func() { dataDirFSType = oldFS; clearCredentialRows() })
	if _, i := rowNamed(systemRows(t, e), checkDataDirLocation); i >= 0 {
		t.Fatal("a location row for a plain local folder")
	}
	dataDirFSType = func(string) (string, bool) { return "exFAT", true }
	c, _ := rowNamed(systemRows(t, e), checkDataDirLocation)
	if c.Status != statusWarn || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirNoACL || c.DetailInfo.Params["fs"] != "exFAT" || c.HintInfo.Code != codeHintDataDirMove {
		t.Fatalf("exFAT: %+v", c)
	}
}

func TestCloudSyncService(t *testing.T) {
	env := map[string]string{"OneDrive": `C:\Users\u\OneDrive`}
	getenv := func(k string) string { return env[k] }
	none := func(string) bool { return false }
	for dir, want := range map[string]string{
		`C:\Users\u\OneDrive\LectureFlow`:                                     "OneDrive",
		`C:\Users\u\OneDrive - Contoso\Data`:                                  "OneDrive",
		`C:\Users\u\iCloudDrive\LF`:                                           "iCloud Drive",
		`C:\Users\u\Dropbox\LF`:                                               "Dropbox",
		`C:\Users\u\Dropbox (Personal)\LF`:                                    "Dropbox",
		`G:\My Drive\LectureFlow`:                                             "Google Drive",
		`G:\我的云端硬盘\LectureFlow`:                                               "Google Drive",
		"/Users/u/Library/Mobile Documents/com~apple~CloudDocs/LF":            "iCloud Drive",
		"/Users/u/Library/CloudStorage/GoogleDrive-u@example.org/My Drive/LF": "Google Drive",
		"/Users/u/Library/CloudStorage/Box-Box/LF":                            "Box",
		`C:\Users\u\AppData\Roaming\LectureFlow`:                              "",
		`D:\LectureFlow`:                                                      "",
		`C:\Users\u\Documents\OneDriveNotes`:                                  "",
	} {
		if got := cloudSyncService(dir, "", getenv, none); got != want {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}
	if runtime.GOOS == "darwin" {
		home := "/Users/u"
		synced := func(p string) bool {
			return p == filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", "Documents")
		}
		if got := cloudSyncService(filepath.Join(home, "Documents", "LF"), home, getenv, synced); got != "iCloud Drive" {
			t.Errorf("synced Documents: %q", got)
		}
		if got := cloudSyncService(filepath.Join(home, "Documents", "LF"), home, getenv, none); got != "" {
			t.Errorf("local Documents: %q", got)
		}
	}
}

// The diagnostics export holds neither a key nor a sealed value, also with
// a sealed key and one that cannot be decrypted.
func TestDiagnosticsExportHoldsNoCredential(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{3}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	const keyA, keyB = "sk-test-diag-cred-enc-aaaa", "sk-test-diag-cred-enc-bbbb"
	typ, base := "openai", "https://llm.example.org"
	for i, k := range []string{keyA, keyB} {
		name, key := []string{"Sealed", "Foreign"}[i], k
		if _, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := secrets.SetKey(bytes.Repeat([]byte{4}, secrets.KeySize)); err != nil { // keyB is sealed with this one
				t.Fatal(err)
			}
			if err := secrets.SetFallbackKeys(bytes.Repeat([]byte{3}, secrets.KeySize)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := secrets.SetFallbackKeys(); err != nil { // keyA (sealed with key 3) no longer opens: an unreadable provider
		t.Fatal(err)
	}
	if err := e.set.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := secrets.Status{Protection: secrets.ProtectionDPAPI}
	fakeCredentialStatus(t, &st)
	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	for name, data := range unzipAll(t, body) {
		for _, needle := range []string{keyA, keyB, secrets.Prefix} {
			if strings.Contains(data, needle) {
				t.Errorf("%s holds %q", name, needle)
			}
		}
	}
}

// sec3 L2: without a data key the settings view carries the warning, a key
// saved now is reported as stored in the clear (also in the save's own
// response), and the diagnostics row is an error; sec3 L4: a store that is
// only unavailable now says so.
func TestNoDataKeyIsVisible(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	t.Cleanup(secrets.ResetForTest)
	secrets.ResetForTest()
	st := secrets.Status{Protection: secrets.ProtectionNone, Err: "DPAPI CryptProtectData: test failure"}
	fakeCredentialStatus(t, &st)
	secrets.SetStatusForTest(st) // settings.CredentialsErrorInfo reads the real status

	var v Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &v)
	if v.CredentialsErrorInfo == nil || v.CredentialsErrorInfo.Code != settings.CodeCredentialsUnavailable {
		t.Fatalf("settings view %+v", v.CredentialsErrorInfo)
	}
	var pv ProviderView
	e.call("POST", "/api/providers", map[string]any{"name": "Plain", "type": "openai", "base_url": "https://llm.example.org", "api_key": "sk-test-plain-cred-enc-0001"}, http.StatusCreated, &pv)
	if !pv.KeyUnprotected || pv.KeyErrorInfo == nil || pv.KeyErrorInfo.Code != settings.CodeProviderKeyUnprotected || pv.KeyHint == "" {
		t.Fatalf("save response %+v", pv)
	}
	c, _ := rowNamed(systemRows(t, e), checkCredentials)
	if c.Status != statusError || c.DetailInfo.Code != codeCredentialsNone {
		t.Fatalf("no key: %+v", c)
	}
	st.Unavailable = true
	c, _ = rowNamed(systemRows(t, e), checkCredentials)
	if c.Status != statusError || c.DetailInfo.Code != codeCredentialsUnavailable || c.DetailInfo.Params["error"] != st.Err {
		t.Fatalf("unavailable: %+v", c)
	}
	// a working key: no warning in the settings view
	secrets.SetStatusForTest(secrets.Status{Protection: secrets.ProtectionDPAPI})
	v = Settings{}
	e.call("GET", "/api/settings", nil, http.StatusOK, &v)
	if v.CredentialsErrorInfo != nil {
		t.Fatalf("warning with a key: %+v", v.CredentialsErrorInfo)
	}
}
