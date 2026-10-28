// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/api"
)

// The mock serves the switch "修复退化的转写窗口" like the real server:
// asr_redecode on by default, PUT /api/settings/asr "redecode" switches it,
// a PUT without it leaves it as it is.
func TestMockRedecodeSwitch(t *testing.T) {
	noLatency(t)
	root := filepath.Join("..", "..", "..")
	srv := httptest.NewServer(newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond))
	t.Cleanup(srv.Close)
	on := func(s api.Settings) bool {
		t.Helper()
		if s.ASRRedecode == nil {
			t.Fatal("no asr_redecode")
		}
		return *s.ASRRedecode
	}
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if !on(s) {
		t.Fatal("off by default")
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/asr", map[string]any{"redecode": false}, http.StatusOK, &s)
	if on(s) {
		t.Fatal("on after PUT redecode false")
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/asr", map[string]any{"multi_device": true}, http.StatusOK, &s)
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if on(s) {
		t.Fatal("a PUT without redecode switched it on")
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/asr", map[string]any{"redecode": true}, http.StatusOK, &s)
	if !on(s) {
		t.Fatal("off after PUT redecode true")
	}
}
