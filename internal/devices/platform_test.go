// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

func init() {
	// The other tests of this package describe the rules of Windows (fw:cpu
	// always has a row); they hold on every host, a Mac included. The macOS
	// rules are tested below with pinGOOS.
	GOOS = "windows"
}

// pinGOOS makes the registries the test creates follow goos's rules.
func pinGOOS(t *testing.T, goos string) {
	t.Helper()
	old := GOOS
	GOOS = goos
	t.Cleanup(func() { GOOS = old })
}

// macWorld is an Apple Silicon Mac: no NVIDIA GPU, no OpenVINO
// environment, the Apple GPU found when mlx is set.
func macWorld(mlx *string) (*world, RegistryOptions) {
	w := newWorld()
	w.set(func(w *world) { w.cuda, w.probe, w.probeErr = "", nil, errors.New("python_openvino: no such file") })
	o := w.opts()
	o.MLX = func(context.Context) (string, bool) { return *mlx, *mlx != "" }
	return w, o
}

func macReg(t *testing.T, st db.Store, cfg *config.Config, o RegistryOptions) *Registry {
	t.Helper()
	r := NewRegistry(st, cfg, t.TempDir(), o)
	if err := r.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	return r
}

// mac-opti M5: on macOS without MLX the device table is empty — no fw:cpu
// row is made — and nothing can be launched on a device; with MLX the
// Apple GPU is the only device.
func TestMacHasNoFWCPU(t *testing.T) {
	pinGOOS(t, "darwin")
	st := regStore(t)
	mlx := ""
	_, o := macWorld(&mlx)
	r := macReg(t, st, config.Default(), o)
	if migrated, err := r.MigrateLegacy(tctx(), model.ASRSettings{Device: "cuda", Model: "large-v3"}); err != nil || migrated {
		t.Fatalf("migration on a Mac: %v %v", migrated, err)
	}
	if _, newIDs, err := r.Detect(tctx()); err != nil || len(newIDs) != 0 {
		t.Fatalf("detect: %v %v", newIDs, err)
	}
	if ids := devIDs(r.List()); len(ids) != 0 {
		t.Fatalf("devices without MLX: %v", ids)
	}
	if _, err := st.GetASRDevice(tctx(), model.DeviceFWCPU); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("fw:cpu row made on a Mac: %v", err)
	}
	if _, ok := r.Get(model.DeviceFWCPU); ok || r.Present(model.DeviceFWCPU) {
		t.Fatal("fw:cpu is a device on a Mac")
	}
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto}); ok {
		t.Fatal("a launch names a device without any")
	}
	if ok, why := r.Usable(model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "small",
		Precision: model.PrecisionAuto, Auto: true}); ok || why == nil || why.Code != CodeSnapshotDeviceUnavailable {
		t.Fatalf("the fallback snapshot is usable without a device: %v %+v", ok, why)
	}

	mlx = "Apple M6"
	if _, newIDs, err := r.Detect(tctx()); err != nil || !reflect.DeepEqual(newIDs, []string{model.DeviceMLXGPU}) {
		t.Fatalf("detect with MLX: %v %v", newIDs, err)
	}
	if ids := devIDs(r.List()); !reflect.DeepEqual(ids, []string{model.DeviceMLXGPU}) {
		t.Fatalf("devices with MLX: %v", ids)
	}
	if snap, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto}); !ok || snap.DeviceID != model.DeviceMLXGPU {
		t.Fatalf("launch snapshot %+v %v", snap, ok)
	}
}

// A database from 3.0.0 has an enabled fw:cpu row (the friend's Mac mini):
// it is hidden and never used, the other rows work as before, and the row
// itself stays (the order keeps it last).
func TestMacHidesEarlierFWCPURow(t *testing.T) {
	pinGOOS(t, "darwin")
	st := regStore(t)
	old := NewDeviceRow(model.DeviceFWCPU, LabelFWCPU)
	old.Enabled = true
	at := "2026-10-25T10:00:00Z"
	old.DetectedAt = &at
	if err := st.InsertASRDevice(tctx(), old); err != nil {
		t.Fatal(err)
	}
	mlx := ""
	_, o := macWorld(&mlx)
	r := macReg(t, st, config.Default(), o)
	// Before the first detection too (detected_at set).
	if ids := devIDs(r.List()); len(ids) != 0 || r.Present(model.DeviceFWCPU) {
		t.Fatalf("before the detection: %v present=%v", ids, r.Present(model.DeviceFWCPU))
	}
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if ids := devIDs(r.List()); len(ids) != 0 {
		t.Fatalf("devices: %v", ids)
	}
	if ps := r.Prompts(); len(ps) != 0 {
		t.Fatalf("prompts: %v", devIDs(ps))
	}
	fixed := model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "small", Precision: model.PrecisionAuto}
	if ok, why := r.Usable(fixed); ok || why == nil {
		t.Fatal("a fw:cpu snapshot is usable on a Mac")
	}
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeManual, ManualDevices: []string{model.DeviceFWCPU}}); ok {
		t.Fatal("a manual list of fw:cpu names it")
	}
	enabled := false
	if _, err := r.Update(tctx(), model.DeviceFWCPU, model.ASRDevicePatch{Enabled: &enabled}); err == nil {
		t.Fatal("fw:cpu can be changed on a Mac")
	} else if code, _ := regCode(err); code != CodeNotFound || !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("update fw:cpu: %v", err)
	}
	if err := r.DismissPrompt(tctx(), model.DeviceFWCPU); err == nil {
		t.Fatal("fw:cpu prompt dismissed on a Mac")
	}

	mlx = "Apple M6"
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if ids := devIDs(r.List()); !reflect.DeepEqual(ids, []string{model.DeviceMLXGPU}) {
		t.Fatalf("devices with MLX: %v", ids)
	}
	if ok, _ := r.Usable(model.ASRSnapshot{DeviceID: model.DeviceFWCPU, Auto: true}); !ok {
		t.Fatal("an auto snapshot is not usable with mlx:gpu present")
	}
	if snap, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto}); !ok || snap.DeviceID != model.DeviceMLXGPU {
		t.Fatalf("launch snapshot %+v %v", snap, ok)
	}
	// The order of the visible rows only; the hidden row keeps the last place.
	if vs, err := r.Reorder(tctx(), []string{model.DeviceMLXGPU}); err != nil || !reflect.DeepEqual(devIDs(vs), []string{model.DeviceMLXGPU}) {
		t.Fatalf("reorder: %v %v", devIDs(vs), err)
	}
	rows, err := st.ListASRDevices(tctx())
	if err != nil || !reflect.DeepEqual(devIDs(rows), []string{model.DeviceMLXGPU, model.DeviceFWCPU}) {
		t.Fatalf("rows in the database: %v %v", devIDs(rows), err)
	}
	if _, err := r.Update(tctx(), model.DeviceMLXGPU, model.ASRDevicePatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto}); ok {
		t.Fatal("the hidden fw:cpu is the launch's device once mlx:gpu is off")
	}
}

// The legacy settings of device cpu make no fw:cpu on a Mac.
func TestMacMigrateLegacyCPU(t *testing.T) {
	pinGOOS(t, "darwin")
	st := regStore(t)
	mlx := ""
	_, o := macWorld(&mlx)
	r := macReg(t, st, config.Default(), o)
	if migrated, err := r.MigrateLegacy(tctx(), model.ASRSettings{Device: "cpu", Model: "small", ComputeType: "auto", MaxParallel: 1}); err != nil || migrated {
		t.Fatalf("migration: %v %v", migrated, err)
	}
	if v, ok, err := st.GetKV(tctx(), model.KVASRDevicesMigrated); err != nil || !ok || v == "" {
		t.Fatalf("migration not marked done: %q %v %v", v, ok, err)
	}
	if rows, err := st.ListASRDevices(tctx()); err != nil || len(rows) != 0 {
		t.Fatalf("rows: %v %v", devIDs(rows), err)
	}
}

// Test mode (asr.fake_result) keeps the rules of the other systems on a
// Mac: fw:cpu has its row (the end-to-end tests run the same everywhere).
func TestMacTestModeKeepsFWCPU(t *testing.T) {
	pinGOOS(t, "darwin")
	st := regStore(t)
	mlx := ""
	_, o := macWorld(&mlx)
	cfg := config.Default()
	cfg.ASR.FakeResult = "fake_asr.json"
	r := macReg(t, st, cfg, o)
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if ids := devIDs(r.List()); !reflect.DeepEqual(ids, []string{model.DeviceFWCPU}) || !r.Present(model.DeviceFWCPU) {
		t.Fatalf("test mode devices: %v", ids)
	}
}

// On Windows fw:cpu is there, as before (the rest of this package's tests).
func TestFWCPUSupported(t *testing.T) {
	for goos, want := range map[string]bool{"windows": true, "linux": true, "darwin": false} {
		if got := FWCPUSupported(goos); got != want {
			t.Errorf("%s: %v", goos, got)
		}
	}
}
