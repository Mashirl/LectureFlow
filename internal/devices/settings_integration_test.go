// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"reflect"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// The startup order of cmd (brief section 7 C): settings → Registry.Load →
// MigrateLegacy(set.ASR()) → SetDeviceSource → Detect. The launch snapshot
// then comes from the rows, and device_mode is checked against them.
func TestRegistryAsSettingsDeviceSource(t *testing.T) {
	st := regStore(t)
	st.SetKV(tctx(), model.KVSettingsImported, "1")
	set, err := settings.Open(tctx(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyA := set.ASR()
	legacyA.Model, legacyA.ComputeType, legacyA.Language = "large-v2", "int8_float16", "en"
	if err := set.SetASR(tctx(), legacyA); err != nil {
		t.Fatal(err)
	}
	legacyJSON := set.CurrentASR()

	w := newWorld()
	r, _ := newReg(t, st, w)
	if _, err := r.MigrateLegacy(tctx(), set.ASR()); err != nil {
		t.Fatal(err)
	}
	set.SetDeviceSource(r)
	// The migrated cuda:0 is the same effective configuration as the legacy
	// snapshot (A4.3: no rerun dialog for old lectures).
	if got := set.CurrentASR(); got != `{"device_id":"cuda:0","backend":"faster-whisper","model":"large-v2","precision":"int8_float16","language":"en","auto":true}` ||
		!model.SameSnapshot(model.StageASR, got, legacyJSON) {
		t.Fatalf("after migration: %s (legacy %s)", got, legacyJSON)
	}
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	a := set.ASR()
	a.DeviceMode = model.DeviceOVNPU
	if err := set.SetASR(tctx(), a); err != nil {
		t.Fatal(err)
	}
	// Power management (power-brief §3.2): the 3b single device id is
	// stored as manual [ov:NPU], and a launch writes an AUTO snapshot whose
	// effective device is the NPU (before: a fixed snapshot without "auto").
	if got := set.ASR(); got.DeviceMode != model.ASRDeviceModeManual || !reflect.DeepEqual(got.ManualDevices, []string{model.DeviceOVNPU}) {
		t.Fatalf("stored device mode: %q %v", got.DeviceMode, got.ManualDevices)
	}
	if got := set.CurrentASR(); got != `{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en","auto":true}` {
		t.Fatalf("manual [ov:NPU]: %s", got)
	}
	a.DeviceMode = "ov:XYZ" // no such row (fw:cpu always has one since 3b Gate round 1)
	if err := set.SetASR(tctx(), a); err == nil {
		t.Fatal("device_mode of a missing row accepted")
	}

	// 3b Gate round 1, item 2: every row disabled (no snapshot to name) →
	// an auto snapshot of the fallback, never the legacy fixed cuda:0; the
	// fallback is devices.Fallback of faster-whisper.
	a.DeviceMode = model.ASRDeviceModeAuto
	if err := set.SetASR(tctx(), a); err != nil {
		t.Fatal(err)
	}
	off := false
	for _, d := range r.List() {
		if _, err := r.Update(tctx(), d.ID, model.ASRDevicePatch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
	}
	fb := Fallback(model.BackendFasterWhisper)
	want := model.MarshalSnapshot(model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper,
		Model: fb.Model, Precision: fb.Precision, Language: "en", Auto: true})
	if got := set.CurrentASR(); got != want || got != model.MarshalSnapshot(settings.FallbackASRSnapshot("en")) {
		t.Fatalf("no enabled row: %s, want %s", got, want)
	}
}
