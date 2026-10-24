// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeASRDeviceMode(t *testing.T) {
	for _, c := range []struct {
		name     string
		mode     string
		list     []string
		wantMode string
		wantList []string
	}{
		{"empty is auto", "", []string{"cuda:0"}, ASRDeviceModeAuto, nil},
		{"auto clears the list", " auto ", []string{"cuda:0"}, ASRDeviceModeAuto, nil},
		{"manual kept", "manual", []string{"cuda:0", "ov:GPU"}, ASRDeviceModeManual, []string{"cuda:0", "ov:GPU"}},
		{"manual trimmed, blanks and duplicates dropped", "manual", []string{" ov:GPU", "", "ov:GPU", "cuda:0 "}, ASRDeviceModeManual, []string{"ov:GPU", "cuda:0"}},
		{"manual drops CPU devices", "manual", []string{"fw:cpu", "ov:NPU", "ov:CPU"}, ASRDeviceModeManual, []string{"ov:NPU"}},
		{"manual without devices is auto", "manual", nil, ASRDeviceModeAuto, nil},
		{"manual with only CPU is auto", "manual", []string{"fw:cpu"}, ASRDeviceModeAuto, nil},
		{"3b single device", "cuda:0", nil, ASRDeviceModeManual, []string{"cuda:0"}},
		{"3b single device ignores a stale list", "ov:NPU", []string{"cuda:0"}, ASRDeviceModeManual, []string{"ov:NPU"}},
		{"3b CPU device is auto", "fw:cpu", nil, ASRDeviceModeAuto, nil},
		{"3b OpenVINO CPU is auto", "ov:CPU", nil, ASRDeviceModeAuto, nil},
		{"unknown id kept for the policy to report", "ov:XYZ", nil, ASRDeviceModeManual, []string{"ov:XYZ"}},
	} {
		in := ASRSettings{Model: "large-v3", Language: "en", DeviceMode: c.mode, ManualDevices: c.list, MultiDevice: true, MemReserveMB: 7}
		got := NormalizeASRDeviceMode(in)
		if got.DeviceMode != c.wantMode || !reflect.DeepEqual(got.ManualDevices, c.wantList) {
			t.Errorf("%s: %q %v, want %q %v", c.name, got.DeviceMode, got.ManualDevices, c.wantMode, c.wantList)
		}
		// Nothing else changes.
		got.DeviceMode, got.ManualDevices = in.DeviceMode, in.ManualDevices
		if !reflect.DeepEqual(got, in) {
			t.Errorf("%s: other fields changed: %+v", c.name, got)
		}
	}
}

// The manual list is omitted when empty: settings JSON written before the
// power management stays the same.
func TestASRSettingsManualDevicesJSON(t *testing.T) {
	b, _ := json.Marshal(ASRSettings{DeviceMode: ASRDeviceModeAuto})
	if s := string(b); contains(s, "manual_devices") {
		t.Fatalf("empty manual list in %s", s)
	}
	b, _ = json.Marshal(ASRSettings{DeviceMode: ASRDeviceModeManual, ManualDevices: []string{"cuda:0", "ov:GPU"}})
	if s := string(b); !contains(s, `"device_mode":"manual"`) || !contains(s, `"manual_devices":["cuda:0","ov:GPU"]`) {
		t.Fatalf("manual settings %s", s)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestWaitPowerCodes(t *testing.T) {
	if got := WaitPowerQueue(DeviceOVNPU); got != "power_queue:ov:NPU" {
		t.Fatalf("queue %q", got)
	}
	w := WaitPowerSwitchPending(DeviceOVNPU, DeviceCUDA0)
	if w != "power_switch_pending:ov:NPU:cuda:0" {
		t.Fatalf("switch %q", w)
	}
	from, to, ok := ParseWaitPowerSwitchPending(w)
	if !ok || from != DeviceOVNPU || to != DeviceCUDA0 {
		t.Fatalf("parse %q %q %v", from, to, ok)
	}
	for _, bad := range []string{"power_queue:ov:NPU", "power_switch_pending:ov:NPU", "power_switch_pending:ov:NPU:cuda",
		"power_switch_pending:ov:NPU:cuda:0:x", "power_switch_pending::NPU:cuda:0", ""} {
		if _, _, ok := ParseWaitPowerSwitchPending(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	for _, c := range []string{WaitPowerWaitAC, WaitPowerNoSaverDevice, WaitPowerNoDevice} {
		if c == "" || c == WaitOnBattery {
			t.Errorf("code %q", c)
		}
	}
}

// Parts are omitted when empty, so a v1.1 snapshot and a bound 3b snapshot
// keep their bytes; a resumed run's parts round-trip.
func TestASRSnapshotParts(t *testing.T) {
	v11 := `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}`
	s, ok := ParseASRSnapshot(v11)
	if !ok || MarshalSnapshot(s) != v11 {
		t.Fatalf("v1.1 snapshot %q", MarshalSnapshot(s))
	}
	bound := `{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en","auto":true}`
	s, ok = ParseASRSnapshot(bound)
	if !ok || MarshalSnapshot(s) != bound {
		t.Fatalf("bound snapshot %q", MarshalSnapshot(s))
	}
	s.Parts = []ASRPart{
		{FromMS: 0, ToMS: 600000, DeviceID: DeviceOVGPU, Model: "large-v3-turbo", Precision: PrecisionINT8},
		{FromMS: 600000, ToMS: 3000000, DeviceID: DeviceOVNPU, Model: "large-v3-turbo", Precision: PrecisionFP16},
	}
	raw := MarshalSnapshot(s)
	want := bound[:len(bound)-1] + `,"parts":[{"from_ms":0,"to_ms":600000,"device_id":"ov:GPU","model":"large-v3-turbo","precision":"int8"},` +
		`{"from_ms":600000,"to_ms":3000000,"device_id":"ov:NPU","model":"large-v3-turbo","precision":"fp16"}]}`
	if raw != want {
		t.Fatalf("parts\n got %s\nwant %s", raw, want)
	}
	back, ok := ParseASRSnapshot(raw)
	if !ok || !reflect.DeepEqual(back, s) {
		t.Fatalf("round trip %+v", back)
	}
	// Parts are not part of the A4.3 comparison.
	if !SameSnapshot(StageASR, raw, bound) {
		t.Fatal("parts changed the A4.3 comparison")
	}
	// Pinning ("沿用原模型") drops the parts (backend Gate round 1, fix 3):
	// the new run's parts are its own.
	if got, want := PinASRSnapshot(raw), `{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"en"}`; got != want {
		t.Fatalf("pinned\n got %s\nwant %s", got, want)
	}
}

func TestPowerStatsJSONAndCountedAttempts(t *testing.T) {
	b, _ := json.Marshal(JobStats{})
	if string(b) != "{}" {
		t.Fatalf("empty stats %s", b)
	}
	st := JobStats{PowerInterrupts: 2, ASRResume: &ASRResume{DeviceID: DeviceCUDA0, Backend: BackendFasterWhisper, At: "2026-10-24T07:00:00Z"},
		PowerEvent: &PowerEvent{Code: "power.suspended", At: "2026-10-24T07:00:00Z", FromDevice: DeviceCUDA0}}
	b, _ = json.Marshal(st)
	want := `{"power_interrupts":2,"asr_resume":{"device_id":"cuda:0","backend":"faster-whisper","at":"2026-10-24T07:00:00Z"},` +
		`"power_event":{"code":"power.suspended","at":"2026-10-24T07:00:00Z","from_device":"cuda:0"}}`
	if string(b) != want {
		t.Fatalf("stats\n got %s\nwant %s", b, want)
	}
	for _, c := range []struct{ attempt, interrupts, want int }{{0, 0, 0}, {2, 0, 2}, {3, 2, 1}, {2, 2, 0}, {1, 3, 0}} {
		j := Job{Attempt: c.attempt, Stats: JobStats{PowerInterrupts: c.interrupts}}
		if got := CountedAttempts(j); got != c.want {
			t.Errorf("attempt %d interrupts %d: %d, want %d", c.attempt, c.interrupts, got, c.want)
		}
	}
}

func TestEqualHelpers(t *testing.T) {
	a := ASRSnapshot{DeviceID: DeviceOVNPU, Backend: BackendOpenVINO, Model: "m", Precision: PrecisionFP16, Language: "en", Auto: true}
	b := a
	b.Parts = []ASRPart{}
	if !EqualASRSnapshot(a, b) {
		t.Fatal("nil and empty parts differ")
	}
	b.Parts = []ASRPart{{ToMS: 1, DeviceID: DeviceOVGPU}}
	if EqualASRSnapshot(a, b) {
		t.Fatal("parts ignored")
	}
	c := a
	c.Language = "de"
	if EqualASRSnapshot(a, c) {
		t.Fatal("language ignored")
	}
	s := ASRSettings{DeviceMode: ASRDeviceModeAuto, MemReserveMB: 1}
	u := s
	u.ManualDevices = []string{}
	if !EqualASRSettings(s, u) {
		t.Fatal("nil and empty manual list differ")
	}
	u.ManualDevices = []string{DeviceCUDA0}
	if EqualASRSettings(s, u) {
		t.Fatal("manual list ignored")
	}
}
