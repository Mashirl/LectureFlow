// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"testing"

	"lectureflow/internal/model"
)

// The switch "修复退化的转写窗口" (redecode, 2026-10-27): GET /api/settings
// shows it as asr_redecode (on when kv asr.redecode was never written); PUT
// /api/settings/asr takes "redecode" next to the ASRSettings fields, writes
// kv asr.redecode ("off" / "on") and leaves it alone when the body has no
// such field; the ASR settings themselves stay as they were.
func TestRedecodeSwitchRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	kv := func() (string, bool) {
		v, ok, err := e.st.GetKV(ctx, model.KVASRRedecode)
		if err != nil {
			t.Fatal(err)
		}
		return v, ok
	}
	get := func() Settings {
		var sv Settings
		e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
		return sv
	}
	on := func(sv Settings) bool {
		if sv.ASRRedecode == nil {
			t.Fatal("GET /api/settings without asr_redecode")
		}
		return *sv.ASRRedecode
	}

	// default: never written, on
	before := get()
	if !on(before) {
		t.Fatal("default off")
	}
	if _, ok := kv(); ok {
		t.Fatal("kv written by a GET")
	}

	var sv Settings
	e.call("PUT", "/api/settings/asr", map[string]any{"redecode": false}, http.StatusOK, &sv)
	if on(sv) || on(get()) {
		t.Fatal("still on after PUT redecode false")
	}
	if v, ok := kv(); !ok || v != model.ASRRedecodeOff {
		t.Fatalf("kv %q %v", v, ok)
	}
	if sv.ASR.Language != before.ASR.Language || sv.ASR.MultiDevice != before.ASR.MultiDevice ||
		sv.ASR.DeviceMode != before.ASR.DeviceMode {
		t.Fatalf("ASR settings changed: %+v, before %+v", sv.ASR, before.ASR)
	}

	// another ASR field without "redecode": the switch stays off
	e.call("PUT", "/api/settings/asr", map[string]any{"multi_device": false}, http.StatusOK, &sv)
	if on(sv) || sv.ASR.MultiDevice {
		t.Fatalf("after a PUT without redecode: on %v, multi_device %v", on(sv), sv.ASR.MultiDevice)
	}

	e.call("PUT", "/api/settings/asr", map[string]any{"redecode": true}, http.StatusOK, &sv)
	if !on(sv) || !on(get()) {
		t.Fatal("still off after PUT redecode true")
	}
	if v, _ := kv(); v != model.ASRRedecodeOn {
		t.Fatalf("kv %q", v)
	}

	// a value that is no boolean is a bad request, and changes nothing
	e.callErrInfo("PUT", "/api/settings/asr", map[string]any{"redecode": "off"}, http.StatusBadRequest)
	if !on(get()) {
		t.Fatal("switched by a bad request")
	}
	// an invalid ASR field fails before the switch is written
	e.callErrInfo("PUT", "/api/settings/asr", map[string]any{"redecode": false, "language": ""}, http.StatusBadRequest)
	if !on(get()) {
		t.Fatal("switched by a request the settings rejected")
	}
}
