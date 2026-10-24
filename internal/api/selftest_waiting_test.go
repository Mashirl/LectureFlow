// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Power management, backend Gate round 1, fix 5 (core-builder): the
// self-test's power-aware waiting reason reaches the API (SelfTestView
// waiting_for, omitted when empty, so every earlier example is unchanged).
func TestSelfTestViewWaitingFor(t *testing.T) {
	st := sched.DeviceTestState{DeviceID: model.DeviceOVNPU, State: sched.DeviceTestWaiting,
		WaitingFor: model.WaitPowerQueue(model.DeviceCUDA0)}
	v := selfTestView(model.DeviceOVNPU, st, true)
	if v.WaitingFor != model.WaitPowerQueue(model.DeviceCUDA0) {
		t.Fatalf("view %+v", v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"waiting_for":"power_queue:cuda:0"`) {
		t.Fatalf("json %s", b)
	}
	st.WaitingFor = ""
	if b, _ := json.Marshal(selfTestView(model.DeviceOVNPU, st, true)); strings.Contains(string(b), "waiting_for") {
		t.Fatalf("empty waiting_for not omitted: %s", b)
	}
	if v := selfTestView(model.DeviceOVNPU, sched.DeviceTestState{}, false); v.WaitingFor != "" || v.State != sched.DeviceTestIdle {
		t.Fatalf("idle %+v", v)
	}
}

// selftest.power_cpu (lead, after the backend Gate round 1): the
// scheduler's coded refusal of a CPU device's self-test on a machine with a
// battery answers 409 with that code, like selftest.power_battery.
func TestSelfTestRefusedPowerCPU(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	de.setClip(true)
	de.ds.mu.Lock()
	de.ds.startErr = &model.CodedError{Info: model.ErrorInfo{Code: sched.CodeSelfTestPowerCPU, Params: map[string]any{"device": "CPU"}},
		Err: sched.ErrSelfTestPowerCPU}
	de.ds.mu.Unlock()
	info := de.callErrInfo("POST", de.devicePath(model.DeviceCUDA0, "selftest"), nil, http.StatusConflict)
	if info.Code != sched.CodeSelfTestPowerCPU || info.Params["device"] != "CPU" {
		t.Fatalf("%+v", info)
	}
}
