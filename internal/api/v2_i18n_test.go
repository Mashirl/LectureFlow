// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// SPEC-v2 P14.3 (i18n-ui): the texts the API still sent in Chinese — the
// diagnostics' detail and hint, RoleView.error, the holiday data's source —
// also come as codes of the catalog, so the front end can translate them.

// wantCataloged fails unless info is a catalog code whose params are all
// listed for it.
func wantCataloged(t *testing.T, what string, info *model.ErrorInfo) {
	t.Helper()
	if info == nil {
		t.Errorf("%s: no code", what)
		return
	}
	def, ok := errorCatalog[info.Code]
	if !ok {
		t.Errorf("%s: code %q is not in the catalog", what, info.Code)
		return
	}
	for k := range info.Params {
		found := false
		for _, p := range def.params {
			found = found || p == k
		}
		if !found {
			t.Errorf("%s: param %q is not a param of %s", what, k, info.Code)
		}
	}
}

// checksByName indexes the checks of a System.
func checksByName(sys System) map[string]SystemCheck {
	out := map[string]SystemCheck{}
	for _, c := range sys.Checks {
		out[c.Name] = c
	}
	return out
}

// wantCheck checks one row: the detail code (and hint code, "" = none) and
// that the transitional Chinese strings are the catalog texts of the codes.
func wantCheck(t *testing.T, c SystemCheck, detail, hint string) {
	t.Helper()
	if c.DetailInfo == nil || c.DetailInfo.Code != detail {
		t.Errorf("%s: detail_info %+v, want %s", c.Name, c.DetailInfo, detail)
	} else {
		wantCataloged(t, c.Name+" detail", c.DetailInfo)
		if c.Name != "disk" && c.Detail != zhText(*c.DetailInfo) {
			t.Errorf("%s: detail %q is not the text of %s", c.Name, c.Detail, detail)
		}
	}
	if hint == "" {
		if c.HintInfo != nil || c.Hint != "" {
			t.Errorf("%s: unexpected hint %q %+v", c.Name, c.Hint, c.HintInfo)
		}
		return
	}
	if c.HintInfo == nil || c.HintInfo.Code != hint {
		t.Errorf("%s: hint_info %+v, want %s", c.Name, c.HintInfo, hint)
		return
	}
	wantCataloged(t, c.Name+" hint", c.HintInfo)
	if c.Hint != zhText(*c.HintInfo) {
		t.Errorf("%s: hint %q is not the text of %s", c.Name, c.Hint, hint)
	}
}

func TestSystemChecksAreCoded(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	by := checksByName(sys)
	wantCheck(t, by["data_dir"], "check.dir_writable", "")
	if p, _ := by["data_dir"].DetailInfo.Params["path"].(string); p == "" {
		t.Errorf("data_dir: no path param: %+v", by["data_dir"].DetailInfo)
	}
	wantCheck(t, by["faster-whisper"], "check.import_ok", "")
	if m := by["faster-whisper"].DetailInfo.Params["module"]; m != "faster_whisper" {
		t.Errorf("faster-whisper module %v", m)
	}
	wantCheck(t, by["pypdf"], "check.import_ok", "")
	wantCheck(t, by["gpu"], "check.gpu_vram", "")
	if p := by["gpu"].DetailInfo.Params; p["total"] != float64(12000) || p["free"] != float64(9000) {
		t.Errorf("gpu params %v", p)
	}
	wantCheck(t, by["cuda"], "check.not_run", "")
	wantCheck(t, by["api_key:deepseek"], "check.key_set", "")
	if k, _ := by["api_key:deepseek"].DetailInfo.Params["key"].(string); !strings.HasPrefix(k, "sk-…") {
		t.Errorf("key param %q", k)
	}
	wantCheck(t, by["api:deepseek"], "check.api_ok", "")
	wantCheck(t, by["disk"], "check.disk_free", "")
	if mb := by["disk"].DetailInfo.Params["free_mb"]; mb != float64(100<<10) {
		t.Errorf("disk free_mb %v", mb)
	}
	// A program's own output is not a code: shown as it is.
	for _, n := range []string{"ffmpeg", "python"} {
		if c := by[n]; c.DetailInfo != nil || c.Detail == "" {
			t.Errorf("%s: %+v", n, c)
		}
	}

	// After a recheck the CUDA row is the worker's own self-test line (a
	// fresh value: decoding into sys would keep the old pointers).
	var after System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &after)
	if c := checksByName(after)["cuda"]; c.DetailInfo != nil || !strings.HasPrefix(c.Detail, "SELFTEST ok") {
		t.Errorf("cuda after recheck: %+v", c)
	}
}

func TestSystemCheckFailuresAreCoded(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.runCmd = func(context.Context, string, ...string) (string, string, error) {
			return "", "Traceback ...\nModuleNotFoundError: No module named 'faster_whisper'", errUnknownCmd
		}
		s.freeSpace = func(string) (int64, error) { return 1 << 30, nil }
	})
	e.srv.diag.waitStartup()
	name, typ, base := "nokey", model.ProviderOpenAI, "https://llm.example.invalid/v1"
	if _, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base}); err != nil {
		t.Fatal(err)
	}
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	by := checksByName(sys)
	wantCheck(t, by["ffmpeg"], "check.run_failed", "check.hint_ffmpeg")
	wantCheck(t, by["python"], "check.run_failed", "check.hint_python")
	wantCheck(t, by["faster-whisper"], "check.import_failed", "check.hint_pip")
	if p := by["faster-whisper"].DetailInfo.Params; p["module"] != "faster_whisper" || !strings.Contains(p["error"].(string), "No module named") {
		t.Errorf("import params %v", p)
	}
	wantCheck(t, by["pypdf"], "check.import_failed", "check.hint_pip")
	wantCheck(t, by["cuda"], "check.selftest_failed", "check.hint_selftest")
	wantCheck(t, by["api_key:nokey"], "check.key_missing", "check.hint_key_settings")
	if p := by["api_key:nokey"].HintInfo.Params; p["provider"] != "nokey" {
		t.Errorf("key hint params %v", p)
	}
	wantCheck(t, by["api:nokey"], "check.api_skipped_no_key", "")
	wantCheck(t, by["disk"], "check.disk_free", "check.hint_disk_error")
	if c := by["disk"]; c.Status != statusError || c.Detail != "剩余 1.0 GB" || c.DetailInfo.Params["free_mb"] != float64(1024) {
		t.Errorf("disk %+v", c)
	}
}

// A check that is still running is marked by its code (the front end shows
// a spinner for check.checking instead of comparing a Chinese text).
func TestSystemCheckPendingIsCoded(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, func(s *Server) {
		s.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return "", "", ctx.Err()
			}
			return fakeCmd(ctx, name, args...)
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	pending := 0
	for _, c := range sys.Checks {
		if c.DetailInfo != nil && c.DetailInfo.Code == checkPending {
			pending++
			wantCheck(t, c, checkPending, "")
			if c.Status != statusWarn {
				t.Errorf("%s: pending status %s", c.Name, c.Status)
			}
		}
	}
	if by := checksByName(sys); by["ffmpeg"].DetailInfo == nil || by["ffmpeg"].DetailInfo.Code != checkPending {
		t.Errorf("ffmpeg while its command runs: %+v", by["ffmpeg"])
	}
	if pending < 4 { // ffmpeg, python and the two imports wait for the command
		t.Errorf("%d pending checks", pending)
	}
	close(release)
	e.srv.diag.waitStartup()
	var done System
	e.call("GET", "/api/system", nil, http.StatusOK, &done)
	for _, c := range done.Checks {
		if c.DetailInfo != nil && c.DetailInfo.Code == checkPending {
			t.Errorf("%s still pending after the startup checks", c.Name)
		}
	}
}

func TestRoleViewErrorInfo(t *testing.T) {
	e := newEnv(t)
	var s Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &s)
	seen := 0
	for _, r := range s.Roles {
		if r.Configured {
			if r.ErrorInfo != nil || r.Error != "" {
				t.Errorf("%s: configured role with an error %+v", r.Role, r)
			}
			continue
		}
		seen++
		if r.ErrorInfo == nil || r.ErrorInfo.Code != settings.CodeRoleNotConfigured || r.ErrorInfo.Params["role"] != r.Role || r.Error == "" {
			t.Errorf("%s: error_info %+v (error %q)", r.Role, r.ErrorInfo, r.Error)
		}
	}
	if seen == 0 {
		t.Fatal("no unconfigured role in the test settings")
	}
	// The code is the one errors.As finds on the settings error.
	_, err := e.set.RoleTarget(model.RoleGlossaryJanitor)
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != settings.CodeRoleNotConfigured {
		t.Fatalf("RoleTarget error %v", err)
	}
}

func TestHolidaysSourceInfo(t *testing.T) {
	e := newEnv(t)
	var v HolidaysView
	e.call("GET", "/api/settings/holidays", nil, http.StatusOK, &v)
	wantCataloged(t, "holidays source", v.SourceInfo)
	if v.SourceInfo == nil || v.SourceInfo.Code != "holidays.source_cn" || v.SourceInfo.Detail != v.Source || v.Source == "" {
		t.Fatalf("source_info %+v (source %q)", v.SourceInfo, v.Source)
	}
	if y, _ := v.SourceInfo.Params["years"].(string); !strings.Contains(y, "2026") {
		t.Errorf("years %q", y)
	}
	// The years are those of the built-in file, also while user data replaces it.
	e.call("PUT", "/api/settings/holidays", HolidaysView{Years: map[string][]string{"2030": {"2030-01-01"}}}, http.StatusOK, &v)
	if !v.Edited || v.SourceInfo == nil || !strings.Contains(v.SourceInfo.Params["years"].(string), "2026") {
		t.Errorf("after editing: %+v", v)
	}
}
