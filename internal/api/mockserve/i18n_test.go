// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// The mock sends the same codes as the real API for the texts that P14.3
// turned into codes (i18n-ui): diagnostics details and hints, RoleView
// errors, the holiday data's source.

// catalogCodes reads testdata/api/error_codes.json (the API's catalog).
func catalogCodes(t *testing.T) map[string]struct {
	Params []string `json:"params"`
} {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "api", "error_codes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]struct {
		Params []string `json:"params"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantCatalogCode(t *testing.T, what string, info *model.ErrorInfo, codes map[string]struct {
	Params []string `json:"params"`
}) {
	t.Helper()
	if info == nil {
		t.Errorf("%s: no code", what)
		return
	}
	def, ok := codes[info.Code]
	if !ok {
		t.Errorf("%s: code %q is not in error_codes.json", what, info.Code)
		return
	}
	for k := range info.Params {
		found := false
		for _, p := range def.Params {
			found = found || p == k
		}
		if !found {
			t.Errorf("%s: param %q is not a param of %s", what, k, info.Code)
		}
	}
}

func TestMockSystemChecksAreCoded(t *testing.T) {
	srv := testServer(t)
	codes := catalogCodes(t)
	for _, scen := range []string{"1", "diag"} {
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system?mock="+scen, nil, http.StatusOK, &sys)
		for _, c := range sys.Checks {
			raw := c.Name == "ffmpeg" || c.Name == "python" || (c.Name == "cuda" && c.Status == "ok") // program output
			if raw {
				if c.DetailInfo != nil {
					t.Errorf("%s %s: program output with a code %+v", scen, c.Name, c.DetailInfo)
				}
			} else {
				wantCatalogCode(t, scen+" "+c.Name+" detail", c.DetailInfo, codes)
			}
			if c.Hint != "" {
				wantCatalogCode(t, scen+" "+c.Name+" hint", c.HintInfo, codes)
			}
		}
	}
}

func TestMockRoleAndHolidayCodes(t *testing.T) {
	noLatency(t)
	srv := testServer(t)
	codes := catalogCodes(t)
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings?mock=unset", nil, http.StatusOK, &s)
	seen := 0
	for _, r := range s.Roles {
		if r.Configured {
			if r.ErrorInfo != nil {
				t.Errorf("%s: configured with error_info %+v", r.Role, r.ErrorInfo)
			}
			continue
		}
		seen++
		if r.ErrorInfo == nil || (r.ErrorInfo.Code != settings.CodeRoleNotConfigured && r.ErrorInfo.Code != settings.CodeRoleProviderDeleted) || r.ErrorInfo.Params["role"] != r.Role {
			t.Errorf("%s: error_info %+v", r.Role, r.ErrorInfo)
		}
	}
	if seen == 0 {
		t.Error("the unset scenario has no unconfigured role")
	}
	var h api.HolidaysView
	doJSON(t, "GET", srv.URL+"/api/settings/holidays", nil, http.StatusOK, &h)
	wantCatalogCode(t, "holidays source", h.SourceInfo, codes)
	if h.SourceInfo == nil || h.SourceInfo.Code != "holidays.source_cn" || h.SourceInfo.Params["years"] == "" {
		t.Errorf("source_info %+v", h.SourceInfo)
	}
}

// Deleting a lecture with a running stage: lecture.running, single and in a
// batch (the front end no longer matches the v1.1 text).
func TestMockBusyDeleteIsCoded(t *testing.T) {
	srv := testServer(t)
	var courses []api.Course
	doJSON(t, "GET", srv.URL+"/api/courses", nil, http.StatusOK, &courses)
	var busy *api.Lecture
	for _, c := range courses {
		var ls []api.Lecture
		doJSON(t, "GET", srv.URL+"/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &ls)
		for i := range ls {
			if !ls[i].CanDelete {
				busy = &ls[i]
				break
			}
		}
		if busy != nil {
			break
		}
	}
	if busy == nil {
		t.Fatal("no busy lecture in the fixtures")
	}
	code, b := rawCall(t, "DELETE", srv.URL+"/api/lectures/"+busy.ID, nil)
	var e api.ErrorBody
	if err := json.Unmarshal(b, &e); err != nil || code != http.StatusConflict || e.Error.Code != "lecture.running" || e.Error.Params["title"] != busy.Title {
		t.Fatalf("DELETE busy: %d %s", code, b)
	}
	var res api.BatchResponse
	doJSON(t, "POST", srv.URL+"/api/lectures/delete", map[string]any{"ids": []string{busy.ID}}, http.StatusOK, &res)
	if len(res.Results) != 1 || res.Results[0].OK || res.Results[0].ErrorInfo == nil || res.Results[0].ErrorInfo.Code != "lecture.running" || res.Results[0].Error == "" {
		t.Fatalf("batch delete busy: %+v", res)
	}
}
