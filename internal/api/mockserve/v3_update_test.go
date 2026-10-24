// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
)

func TestMockAppUpdate(t *testing.T) {
	old := updDownloadDur
	updDownloadDur = 200 * time.Millisecond
	t.Cleanup(func() { updDownloadDur = old })
	u := testServer(t).URL

	get := func(sc string) api.UpdateStatus {
		t.Helper()
		code, b, _ := mockDo(t, "GET", u+"/api/app/update?mock="+sc, "")
		var s api.UpdateStatus
		if code != 200 || json.Unmarshal(b, &s) != nil {
			t.Fatalf("get %s: %d %s", sc, code, b)
		}
		return s
	}
	if s := get("1"); s.State != "none" {
		t.Errorf("default scenario: %+v", s)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/app/update/download?mock=1", ""); code != 409 || !strings.Contains(string(b), "update.not_available") {
		t.Errorf("download without update: %d %s", code, b)
	}
	if s := get("update_ready"); s.State != "ready" || s.Version != updVersion || s.Percent != 100 || s.InstallOnQuit {
		t.Errorf("ready: %+v", s)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/app/update/install-on-quit?mock=update_ready", `{"enabled":true}`); code != 200 || !strings.Contains(string(b), `"install_on_quit":true`) {
		t.Errorf("install on quit: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/app/update/install-on-quit?mock=update_ready", `{"enabled":false}`); code != 200 || !strings.Contains(string(b), `"install_on_quit":false`) {
		t.Errorf("install on quit off: %d %s", code, b)
	}
	if s := get("update_error"); s.State != "error" || s.Error == nil || s.Error.Code != "update.download_failed" {
		t.Errorf("error: %+v", s)
	}

	if s := get("update_available"); s.State != "available" || s.Version != updVersion {
		t.Fatalf("available: %+v", s)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/app/update/download?mock=update_available", ""); code != 202 || !strings.Contains(string(b), `"downloading"`) {
		t.Fatalf("download: %d %s", code, b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for get("update_available").State != "ready" {
		if time.Now().After(deadline) {
			t.Fatal("the download never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}

	code, b, _ := mockDo(t, "GET", u+"/api/app/activity?mock=1", "")
	var a api.AppActivity
	if code != 200 || json.Unmarshal(b, &a) != nil {
		t.Fatalf("activity: %d %s", code, b)
	}
}

type compListView struct {
	Components []struct {
		ID            string `json:"id"`
		State         string `json:"state"`
		Installed     string `json:"installed_version"`
		UpdateVersion string `json:"update_version"`
	} `json:"components"`
	Tasks []struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		State string `json:"state"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"tasks"`
}

func TestMockComponentsSettings(t *testing.T) {
	old := obInstallDuration
	obInstallDuration = 300 * time.Millisecond
	oldPhase := obPhase
	obPhase = 20 * time.Millisecond
	t.Cleanup(func() { obInstallDuration, obPhase = old, oldPhase })
	u := testServer(t).URL

	list := func() compListView {
		t.Helper()
		code, b, _ := mockDo(t, "GET", u+"/api/components?mock=1", "")
		var v compListView
		if code != 200 || json.Unmarshal(b, &v) != nil {
			t.Fatalf("list: %d %s", code, b)
		}
		return v
	}
	comp := func(v compListView, id string) (state, installed, update string) {
		for _, c := range v.Components {
			if c.ID == id {
				return c.State, c.Installed, c.UpdateVersion
			}
		}
		t.Fatalf("no component %s", id)
		return
	}
	waitTask := func(kind, state string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			for _, tk := range list().Tasks {
				if tk.Kind == kind && tk.State == state {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s task in state %s", kind, state)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}

	if code, b, _ := mockDo(t, "DELETE", u+"/api/components/asr-cuda?mock=1", ""); code != 409 || !strings.Contains(string(b), "components.not_installed") {
		t.Errorf("uninstall not installed: %d %s", code, b)
	}
	if code, _, _ := mockDo(t, "POST", u+"/api/components/_demo?mock=1", ""); code != 204 {
		t.Fatalf("demo: %d", code)
	}
	v := list()
	if st, _, up := comp(v, "asr-cuda"); st != "installed" || up != "1.2.0-1" {
		t.Errorf("asr-cuda after demo: %s %s", st, up)
	}
	if st, _, _ := comp(v, "asr-openvino"); st != "broken" {
		t.Errorf("asr-openvino after demo: %s", st)
	}
	if code, b, _ := mockDo(t, "DELETE", u+"/api/components/core?mock=1", ""); code != 409 || !strings.Contains(string(b), "components.bundled") {
		t.Errorf("uninstall core: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "DELETE", u+"/api/components/python?mock=1", ""); code != 409 || !strings.Contains(string(b), "components.in_use") {
		t.Errorf("uninstall python: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/components/python/update?mock=1", ""); code != 409 || !strings.Contains(string(b), "components.up_to_date") {
		t.Errorf("update python: %d %s", code, b)
	}

	// update: busy while it runs, then the new version
	if code, b, _ := mockDo(t, "POST", u+"/api/components/asr-cuda/update?mock=1", ""); code != 202 || !strings.Contains(string(b), `"update"`) {
		t.Fatalf("update: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "DELETE", u+"/api/components/asr-cuda?mock=1", ""); code != 409 || !strings.Contains(string(b), "components.busy") && !strings.Contains(string(b), "components.in_use") {
		t.Errorf("uninstall while updating: %d %s", code, b)
	}
	waitTask("update", "done")
	if st, inst, up := comp(list(), "asr-cuda"); st != "installed" || inst != "1.2.0-1" || up != "" {
		t.Errorf("after update: %s %s %s", st, inst, up)
	}
	// repair of the broken runtime
	if code, b, _ := mockDo(t, "POST", u+"/api/components/asr-openvino/update?mock=1", ""); code != 202 {
		t.Fatalf("repair: %d %s", code, b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st, _, _ := comp(list(), "asr-openvino"); st == "installed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("repair never finished")
		}
		time.Sleep(30 * time.Millisecond)
	}

	// uninstall: a model (no dependents)
	if code, b, _ := mockDo(t, "DELETE", u+"/api/components/model:faster-whisper:large-v3:float16?mock=1", ""); code != 204 {
		t.Errorf("uninstall model: %d %s", code, b)
	}
	if st, _, _ := comp(list(), "model:faster-whisper:large-v3:float16"); st != "available" {
		t.Errorf("model after uninstall: %s", st)
	}

	// import: by path (desktop), a failing archive, a wrong file, multipart
	if code, b, _ := mockDo(t, "POST", u+"/api/components/import?mock=1", `{"path":"D:\\x\\fail.zip"}`); code != 202 || !strings.Contains(string(b), "components.import_checksum") {
		t.Errorf("import fail: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "POST", u+"/api/components/import?mock=1", `{"path":"D:\\x\\notes.txt"}`); code != 400 || !strings.Contains(string(b), "components.import_invalid") {
		t.Errorf("import txt: %d %s", code, b)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "medium.zip")
	fw.Write([]byte("PK"))
	mw.Close()
	req, _ := http.NewRequest("POST", u+"/api/components/import?mock=1", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("multipart import: %d", res.StatusCode)
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		if st, _, _ := comp(list(), compImportID); st == "installed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("import never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
