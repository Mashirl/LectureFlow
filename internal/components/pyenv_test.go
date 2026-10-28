// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/pyenv"
)

// The post-install import check blocks openvino's telemetry package before
// importing anything (`import openvino` alone reported to Google
// Analytics) and runs with pyenv's switches, even when the service's own
// environment turns them off (NOTES.md "无遥测").
func TestImportCheckWithoutTelemetry(t *testing.T) {
	e := newTestEnv(t)
	log := filepath.Join(t.TempDir(), "py.jsonl")
	t.Setenv("LF_FAKE_PY_LOG", log)
	t.Setenv("HF_HUB_DISABLE_TELEMETRY", "0")
	m := e.open()
	id, _ := m.Install([]string{IDASRCUDA}, Sources{})
	if st := wait(t, m, id, TaskFailed, TaskDone); st.State != TaskDone {
		t.Fatalf("install: %s %+v", st.State, st.Error)
	}
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	checks := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var call struct {
			Args         []string `json:"args"`
			TelemetryOff bool     `json:"telemetry_off"`
		}
		if err := json.Unmarshal(sc.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		if !call.TelemetryOff {
			t.Errorf("python %q ran without %q", call.Args, pyenv.TelemetryOff)
		}
		if len(call.Args) >= 3 && call.Args[0] == "-I" && call.Args[1] == "-c" {
			checks++
			if !strings.HasPrefix(call.Args[2], pyenv.BlockTelemetryModules+";") {
				t.Errorf("import check %q does not block the telemetry packages first", call.Args[2])
			}
		}
	}
	if checks == 0 {
		t.Fatal("no import check ran")
	}
}
