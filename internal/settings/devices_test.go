// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// Phase 3b freeze: without a DeviceSource, CurrentASR is the asr entry of
// CurrentSnapshots (the legacy snapshot, byte for byte), also while an LLM
// role is not configured.
func TestCurrentASRLegacy(t *testing.T) {
	s, _ := newService(t)
	s.SetASR(bg(), model.ASRSettings{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en", MaxParallel: 1, VRAMReserveMB: 1000})
	want := `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}`
	if got := s.CurrentASR(); got != want {
		t.Fatalf("CurrentASR = %s, want %s", got, want)
	}
	if _, err := s.CurrentSnapshots(); err == nil {
		t.Fatal("roles are not configured: CurrentSnapshots should fail")
	}
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	for _, r := range []string{model.RoleSummary, model.RoleRepair, model.RoleTranslate} {
		if err := s.SetRole(bg(), r, p.ID, "deepseek-chat", model.RoleParams{}); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err := s.CurrentSnapshots()
	if err != nil || snaps[model.StageASR] != s.CurrentASR() {
		t.Fatalf("CurrentSnapshots asr = %q (%v), CurrentASR = %q", snaps[model.StageASR], err, s.CurrentASR())
	}
	s.SetDeviceSource(nil) // nil is allowed and changes nothing
	if s.CurrentASR() != want {
		t.Fatal("SetDeviceSource(nil) changed the snapshot")
	}
}

// fakeDevices is a DeviceSource with fixed rows and a fixed answer.
type fakeDevices struct {
	snap  model.ASRSnapshot
	ok    bool
	rows  map[string]model.ASRDevice
	calls int
}

func (f *fakeDevices) CurrentASRSnapshot(a model.ASRSettings) (model.ASRSnapshot, bool) {
	f.calls++
	s := f.snap
	s.Language = a.Language
	return s, f.ok
}

func (f *fakeDevices) Get(id string) (model.ASRDevice, bool) {
	d, ok := f.rows[id]
	return d, ok
}

// Phase 3b: the defaults of the four new fields, and how a stored value
// without them (written before 3b) or with odd values loads.
func TestASRDefaultsPhase3b(t *testing.T) {
	d := DefaultASR()
	if d.DeviceMode != model.ASRDeviceModeAuto || !d.MultiDevice || !d.BatteryAware || d.MemReserveMB != 2048 {
		t.Fatalf("defaults %+v", d)
	}
	s, st := newService(t)
	if a := s.ASR(); !model.EqualASRSettings(a, DefaultASR()) {
		t.Fatalf("empty database %+v", a)
	}
	// A value written before 3b (no new keys): the defaults of the new fields.
	st.SetKV(bg(), model.KVSettingsASR, `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en","max_parallel":2,"vram_reserve_mb":1000,"vram_per_job_mb":5000}`)
	s.Load(bg())
	want := DefaultASR()
	want.Model, want.MaxParallel = "large-v2", 2
	if a := s.ASR(); !model.EqualASRSettings(a, want) {
		t.Fatalf("pre-3b value\n got %+v\nwant %+v", a, want)
	}
	// Stored false / 0 stay; an empty device_mode is auto; an out-of-range
	// reserve is the default.
	st.SetKV(bg(), model.KVSettingsASR, `{"model":"large-v2","device_mode":" ","multi_device":false,"battery_aware":false,"mem_reserve_mb":0}`)
	s.Load(bg())
	if a := s.ASR(); a.DeviceMode != "auto" || a.MultiDevice || a.BatteryAware || a.MemReserveMB != 0 {
		t.Fatalf("explicit values %+v", a)
	}
	// Power management (power-brief §3.2): the 3b single device id reads as
	// manual [that id].
	for _, v := range []string{"-1", "65537"} {
		st.SetKV(bg(), model.KVSettingsASR, `{"model":"large-v2","mem_reserve_mb":`+v+`,"device_mode":"ov:NPU"}`)
		s.Load(bg())
		if a := s.ASR(); a.MemReserveMB != 2048 || a.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(a.ManualDevices, []string{"ov:NPU"}) {
			t.Fatalf("mem_reserve_mb %s: %+v", v, a)
		}
	}
}

func TestSetASRDeviceFields(t *testing.T) {
	s, st := newService(t)
	base := model.ASRSettings{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en", MaxParallel: 1, VRAMReserveMB: 1000}
	// An older client's struct without the new fields: device_mode "" is
	// stored as auto (the API merges first, so false / 0 are explicit).
	if err := s.SetASR(bg(), base); err != nil {
		t.Fatal(err)
	}
	if a := s.ASR(); a.DeviceMode != model.ASRDeviceModeAuto || a.MultiDevice || a.BatteryAware || a.MemReserveMB != 0 {
		t.Fatalf("stored %+v", a)
	}
	for _, mb := range []int{-1, 65537} {
		a := base
		a.MemReserveMB = mb
		if err := s.SetASR(bg(), a); !errors.Is(err, ErrInvalid) || !hasCode(err, CodeMemReserveInvalid, map[string]any{"min": 0, "max": 65536}) {
			t.Fatalf("mem_reserve_mb %d: %v", mb, err)
		}
	}
	// device_mode must be auto or an existing device row. Without a
	// registry the database decides.
	a := base
	a.DeviceMode = " ov:NPU "
	if err := s.SetASR(bg(), a); !errors.Is(err, ErrInvalid) || !hasCode(err, CodeDeviceModeInvalid, map[string]any{"value": "ov:NPU"}) {
		t.Fatalf("unknown device: %v", err)
	}
	if err := st.InsertASRDevice(bg(), model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "NPU",
		Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100}); err != nil {
		t.Fatal(err)
	}
	// Power management: stored as manual [ov:NPU] (power-brief §3.2).
	if err := s.SetASR(bg(), a); err != nil || s.ASR().DeviceMode != model.ASRDeviceModeManual ||
		!slices.Equal(s.ASR().ManualDevices, []string{model.DeviceOVNPU}) {
		t.Fatalf("existing row: %v %+v", err, s.ASR())
	}
	// With a registry, its rows decide (not the database).
	f := &fakeDevices{rows: map[string]model.ASRDevice{model.DeviceOVGPU: {ID: model.DeviceOVGPU}}}
	s.SetDeviceSource(f)
	if err := s.SetASR(bg(), a); !hasCode(err, CodeDeviceModeInvalid, map[string]any{"value": "ov:NPU"}) {
		t.Fatalf("registry without the row: %v", err)
	}
	a.DeviceMode = model.DeviceOVGPU
	a.MultiDevice, a.BatteryAware, a.MemReserveMB = true, false, 4096
	if err := s.SetASR(bg(), a); err != nil {
		t.Fatal(err)
	}
	// Persisted with every field.
	s2, _ := Open(bg(), st, nil)
	if got := s2.ASR(); got.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(got.ManualDevices, []string{model.DeviceOVGPU}) ||
		!got.MultiDevice || got.BatteryAware || got.MemReserveMB != 4096 {
		t.Fatalf("reloaded %+v", got)
	}
	a.DeviceMode = "AUTO" // ids and auto are compared exactly
	if err := s.SetASR(bg(), a); !hasCode(err, CodeDeviceModeInvalid, map[string]any{"value": "AUTO"}) {
		t.Fatalf("AUTO: %v", err)
	}
}

// Lead ruling 15: PUT /api/settings/asr merges the body into ASR() before
// SetASR; the stored JSON then keeps the fields the body did not send.
func TestSetASRMergedJSON(t *testing.T) {
	s, st := newService(t)
	a := s.ASR()
	if err := json.Unmarshal([]byte(`{"model":"large-v2","max_parallel":2}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := s.SetASR(bg(), a); err != nil {
		t.Fatal(err)
	}
	v, _, _ := st.GetKV(bg(), model.KVSettingsASR)
	want := `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en","max_parallel":2,"vram_reserve_mb":1000,` +
		`"vram_per_job_mb":5000,"device_mode":"auto","multi_device":true,"battery_aware":true,"mem_reserve_mb":2048}`
	if v != want {
		t.Fatalf("stored\n got %s\nwant %s", v, want)
	}
	a = s.ASR()
	json.Unmarshal([]byte(`{"multi_device":false}`), &a)
	s.SetASR(bg(), a)
	if got := s.ASR(); got.MultiDevice || !got.BatteryAware || got.MemReserveMB != 2048 || got.MaxParallel != 2 {
		t.Fatalf("second merge %+v", got)
	}
}

func TestCurrentASRWithDeviceSource(t *testing.T) {
	s, _ := newService(t)
	s.SetASR(bg(), model.ASRSettings{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "de", MaxParallel: 1, VRAMReserveMB: 1000})
	legacy := `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"de"}`
	f := &fakeDevices{snap: model.ASRSnapshot{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo",
		Precision: "fp16", Auto: true}, ok: true}
	s.SetDeviceSource(f)
	want := `{"device_id":"ov:NPU","backend":"openvino","model":"large-v3-turbo","precision":"fp16","language":"de","auto":true}`
	if got := s.CurrentASR(); got != want || f.calls != 1 {
		t.Fatalf("CurrentASR = %s (calls %d), want %s", got, f.calls, want)
	}
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	for _, r := range []string{model.RoleSummary, model.RoleRepair, model.RoleTranslate} {
		s.SetRole(bg(), r, p.ID, "deepseek-chat", model.RoleParams{})
	}
	if snaps, err := s.CurrentSnapshots(); err != nil || snaps[model.StageASR] != want {
		t.Fatalf("CurrentSnapshots asr = %s (%v)", snaps[model.StageASR], err)
	}
	// ok false (no enabled row / no row for the mode): an auto snapshot of
	// the fallback (3b Gate round 1, item 2), no longer the legacy one.
	f.ok = false
	if got := s.CurrentASR(); got != `{"device_id":"fw:cpu","backend":"faster-whisper","model":"small","precision":"auto","language":"de","auto":true}` {
		t.Fatalf("ok false: %s", got)
	}
	s.SetDeviceSource(nil)
	if got := s.CurrentASR(); got != legacy {
		t.Fatalf("no source: %s", got)
	}
}
