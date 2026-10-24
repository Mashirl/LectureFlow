// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// Power management (power-brief §3.2, task P4): the device mode is auto or
// manual with a list; reading normalizes (model.NormalizeASRDeviceMode),
// writing validates first and stores the normalized value.

// laptopDevices is a registry with the four rows of a laptop.
func laptopDevices() *fakeDevices {
	return &fakeDevices{rows: map[string]model.ASRDevice{
		model.DeviceCUDA0: {ID: model.DeviceCUDA0, Label: "NVIDIA GeForce RTX 5070 Ti Laptop GPU"},
		model.DeviceOVGPU: {ID: model.DeviceOVGPU, Label: "Intel(R) Graphics"},
		model.DeviceOVNPU: {ID: model.DeviceOVNPU, Label: "Intel(R) AI Boost"},
		model.DeviceFWCPU: {ID: model.DeviceFWCPU, Label: "CPU (faster-whisper)"},
		model.DeviceOVCPU: {ID: model.DeviceOVCPU}, // no label: the id is the param
	}}
}

func TestLoadNormalizesDeviceMode(t *testing.T) {
	s, st := newService(t)
	cases := []struct {
		name, stored string
		mode         string
		list         []string
	}{
		{"3b single device", `{"model":"large-v2","device_mode":"cuda:0"}`, model.ASRDeviceModeManual, []string{"cuda:0"}},
		{"3b single device, stale list", `{"model":"large-v2","device_mode":"ov:NPU","manual_devices":["cuda:0"]}`, model.ASRDeviceModeManual, []string{"ov:NPU"}},
		{"3b CPU device", `{"model":"large-v2","device_mode":"fw:cpu"}`, model.ASRDeviceModeAuto, nil},
		{"manual cleaned", `{"model":"large-v2","device_mode":"manual","manual_devices":[" ov:GPU","","ov:GPU","fw:cpu","cuda:0"]}`,
			model.ASRDeviceModeManual, []string{"ov:GPU", "cuda:0"}},
		{"manual without devices", `{"model":"large-v2","device_mode":"manual","manual_devices":[]}`, model.ASRDeviceModeAuto, nil},
		{"manual with CPU only", `{"model":"large-v2","device_mode":"manual","manual_devices":["ov:CPU"]}`, model.ASRDeviceModeAuto, nil},
		{"auto drops a list", `{"model":"large-v2","device_mode":"auto","manual_devices":["cuda:0"]}`, model.ASRDeviceModeAuto, nil},
		{"missing", `{"model":"large-v2"}`, model.ASRDeviceModeAuto, nil},
		// a row that no longer exists stays in the list (the policy skips it)
		{"unknown id kept", `{"model":"large-v2","device_mode":"manual","manual_devices":["ov:XPU"]}`, model.ASRDeviceModeManual, []string{"ov:XPU"}},
	}
	for _, c := range cases {
		if err := st.SetKV(bg(), model.KVSettingsASR, c.stored); err != nil {
			t.Fatal(err)
		}
		if err := s.Load(bg()); err != nil {
			t.Fatal(err)
		}
		a := s.ASR()
		if a.DeviceMode != c.mode || !slices.Equal(a.ManualDevices, c.list) {
			t.Errorf("%s: device_mode %q manual_devices %q, want %q %q", c.name, a.DeviceMode, a.ManualDevices, c.mode, c.list)
		}
		// Reading never writes.
		if v, _, _ := st.GetKV(bg(), model.KVSettingsASR); v != c.stored {
			t.Errorf("%s: Load rewrote the stored value: %s", c.name, v)
		}
	}
}

func TestSetASRManualDevices(t *testing.T) {
	s, st := newService(t)
	s.SetDeviceSource(laptopDevices())
	base := s.ASR()
	set := func(mode string, list ...string) error {
		a := base
		a.DeviceMode, a.ManualDevices = mode, list
		return s.SetASR(bg(), a)
	}
	errCases := []struct {
		name   string
		mode   string
		list   []string
		code   string
		params map[string]any
	}{
		{"manual without devices", "manual", nil, CodeManualDevicesEmpty, nil},
		{"manual with blanks only", " manual ", []string{"", "  "}, CodeManualDevicesEmpty, nil},
		{"manual with a CPU device", "manual", []string{"ov:GPU", "fw:cpu"}, CodeManualDevicesCPU, map[string]any{"device": "CPU (faster-whisper)"}},
		{"CPU device without a label", "manual", []string{"ov:CPU"}, CodeManualDevicesCPU, map[string]any{"device": "ov:CPU"}},
		{"unknown device", "manual", []string{"cuda:0", " ov:XPU "}, CodeManualDevicesUnknown, map[string]any{"device": "ov:XPU"}},
		{"the first problem in the order sent", "manual", []string{"ov:XPU", "fw:cpu"}, CodeManualDevicesUnknown, map[string]any{"device": "ov:XPU"}},
		// an older client's single device id
		{"3b CPU device id", "fw:cpu", nil, CodeManualDevicesCPU, map[string]any{"device": "CPU (faster-whisper)"}},
		{"3b unknown id", "ov:XPU", nil, CodeDeviceModeInvalid, map[string]any{"value": "ov:XPU"}},
		{"MANUAL is not manual", "MANUAL", []string{"cuda:0"}, CodeDeviceModeInvalid, map[string]any{"value": "MANUAL"}},
	}
	before, _, _ := st.GetKV(bg(), model.KVSettingsASR)
	for _, c := range errCases {
		err := set(c.mode, c.list...)
		if !errors.Is(err, ErrInvalid) || !hasCode(err, c.code, c.params) {
			t.Errorf("%s: %v, want %s %v", c.name, err, c.code, c.params)
		}
	}
	if after, _, _ := st.GetKV(bg(), model.KVSettingsASR); after != before {
		t.Fatalf("a refused request changed the settings: %s", after)
	}

	// Accepted: stored normalized (trimmed, de-duplicated, order kept).
	if err := set(" manual ", " ov:GPU ", "cuda:0", "ov:GPU"); err != nil {
		t.Fatal(err)
	}
	if a := s.ASR(); a.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(a.ManualDevices, []string{"ov:GPU", "cuda:0"}) {
		t.Fatalf("manual %+v", a)
	}
	v, _, _ := st.GetKV(bg(), model.KVSettingsASR)
	if !strings.Contains(v, `"device_mode":"manual"`) || !strings.HasSuffix(v, `,"manual_devices":["ov:GPU","cuda:0"]}`) {
		t.Fatalf("stored %s", v)
	}
	// An older client's single id: manual [id].
	if err := set(model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	if a := s.ASR(); a.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(a.ManualDevices, []string{model.DeviceOVNPU}) {
		t.Fatalf("3b id %+v", a)
	}
	// auto drops the list: the JSON is the one of before power management.
	if err := set("auto", "cuda:0"); err != nil {
		t.Fatal(err)
	}
	v, _, _ = st.GetKV(bg(), model.KVSettingsASR)
	if strings.Contains(v, "manual_devices") || !strings.Contains(v, `"device_mode":"auto"`) || s.ASR().ManualDevices != nil {
		t.Fatalf("auto stored %s (%+v)", v, s.ASR())
	}
	// "" is auto as in 3b.
	if err := set("", "cuda:0"); err != nil || s.ASR().DeviceMode != model.ASRDeviceModeAuto {
		t.Fatalf("empty mode: %v %+v", err, s.ASR())
	}
}

// Without a registry the database rows decide (as device_mode in 3b).
func TestSetASRManualDevicesFromDatabase(t *testing.T) {
	s, st := newService(t)
	a := s.ASR()
	a.DeviceMode, a.ManualDevices = model.ASRDeviceModeManual, []string{model.DeviceOVGPU}
	if err := s.SetASR(bg(), a); !hasCode(err, CodeManualDevicesUnknown, map[string]any{"device": model.DeviceOVGPU}) {
		t.Fatalf("no row: %v", err)
	}
	// A CPU device is refused before its row is looked for (no row: the id).
	cpu := a
	cpu.ManualDevices = []string{model.DeviceOVCPU}
	if err := s.SetASR(bg(), cpu); !hasCode(err, CodeManualDevicesCPU, map[string]any{"device": model.DeviceOVCPU}) {
		t.Fatalf("cpu without a row: %v", err)
	}
	if err := st.InsertASRDevice(bg(), model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Graphics",
		Enabled: false, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 200}); err != nil {
		t.Fatal(err)
	}
	// A disabled row may be listed (the policy uses the usable members).
	if err := s.SetASR(bg(), a); err != nil || !slices.Equal(s.ASR().ManualDevices, []string{model.DeviceOVGPU}) {
		t.Fatalf("existing row: %v %+v", err, s.ASR())
	}
}

// ASR returns a copy of the list: decoding a request into it (lead ruling
// 15) never changes the cache, also when the request is then refused.
func TestASRManualDevicesIsACopy(t *testing.T) {
	s, _ := newService(t)
	s.SetDeviceSource(laptopDevices())
	a := s.ASR()
	a.DeviceMode, a.ManualDevices = model.ASRDeviceModeManual, []string{model.DeviceCUDA0, model.DeviceOVGPU}
	if err := s.SetASR(bg(), a); err != nil {
		t.Fatal(err)
	}
	in := s.ASR()
	if err := json.Unmarshal([]byte(`{"manual_devices":["fw:cpu"]}`), &in); err != nil {
		t.Fatal(err)
	}
	if err := s.SetASR(bg(), in); !hasCode(err, CodeManualDevicesCPU, map[string]any{"device": "CPU (faster-whisper)"}) {
		t.Fatalf("cpu: %v", err)
	}
	if got := s.ASR().ManualDevices; !slices.Equal(got, []string{model.DeviceCUDA0, model.DeviceOVGPU}) {
		t.Fatalf("cache changed through the returned value: %q", got)
	}
	got := s.ASR()
	got.ManualDevices[0] = "x"
	if s.ASR().ManualDevices[0] != model.DeviceCUDA0 {
		t.Fatal("ASR shares the cache's array")
	}
}

// Lead ruling 15 with the new fields: the merged JSON keeps the list when a
// request leaves it out; auto clears it.
func TestSetASRMergedManualJSON(t *testing.T) {
	s, st := newService(t)
	s.SetDeviceSource(laptopDevices())
	merge := func(body string) error {
		t.Helper()
		a := s.ASR()
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			t.Fatal(err)
		}
		return s.SetASR(bg(), a)
	}
	if err := merge(`{"device_mode":"manual"}`); !hasCode(err, CodeManualDevicesEmpty, nil) {
		t.Fatalf("manual without a list: %v", err)
	}
	if err := merge(`{"device_mode":"manual","manual_devices":["cuda:0","ov:GPU"]}`); err != nil {
		t.Fatal(err)
	}
	if err := merge(`{"multi_device":false}`); err != nil {
		t.Fatal(err)
	}
	v, _, _ := st.GetKV(bg(), model.KVSettingsASR)
	want := `{"model":"large-v3","compute_type":"auto","device":"cuda","language":"en","max_parallel":1,"vram_reserve_mb":1000,` +
		`"vram_per_job_mb":5000,"device_mode":"manual","multi_device":false,"battery_aware":true,"mem_reserve_mb":2048,` +
		`"manual_devices":["cuda:0","ov:GPU"]}`
	if v != want {
		t.Fatalf("stored\n got %s\nwant %s", v, want)
	}
	if err := merge(`{"manual_devices":["ov:NPU"]}`); err != nil || !slices.Equal(s.ASR().ManualDevices, []string{"ov:NPU"}) {
		t.Fatalf("new list: %v %+v", err, s.ASR())
	}
	if err := merge(`{"device_mode":"auto"}`); err != nil || s.ASR().ManualDevices != nil {
		t.Fatalf("auto: %v %+v", err, s.ASR())
	}
	// Back to manual without sending a list: the old one is gone.
	if err := merge(`{"device_mode":"manual"}`); !hasCode(err, CodeManualDevicesEmpty, nil) {
		t.Fatalf("manual after auto: %v", err)
	}
}
