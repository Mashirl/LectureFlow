// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// SPEC-v3 V14 (v14-colab-integrate): colab:remote in the device table while
// Colab is enabled and authorised.

func colabRow(acc string) model.ASRDevice {
	return model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Label: "Google Colab · " + acc,
		Model: "large-v3", Precision: "float16", MaxParallel: 1}
}

func TestSetRemote(t *testing.T) {
	r, st, _, hooks := regFixture(t)
	before := hooks.Load()
	local := devIDs(r.List())
	if slices.Contains(local, model.DeviceColab) || r.Present(model.DeviceColab) {
		t.Fatalf("colab:remote before SetRemote: %v", local)
	}

	// Present: inserted (priority below the local accelerators, enabled,
	// prompt dismissed), listed, present, usable.
	if err := r.SetRemote(tctx(), true, colabRow("T4")); err != nil {
		t.Fatal(err)
	}
	if hooks.Load() == before {
		t.Fatal("no OnChange call")
	}
	// Priority 5 (v5-integration-a): after every local accelerator and
	// fw:cpu, before ov:CPU (the last resort, disabled by default).
	ids := devIDs(r.List())
	if want := []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceFWCPU, model.DeviceColab, model.DeviceOVCPU}; !slices.Equal(ids, want) || !r.Present(model.DeviceColab) {
		t.Fatalf("list %v present %v", ids, r.Present(model.DeviceColab))
	}
	d, ok := r.Get(model.DeviceColab)
	if !ok || !d.Enabled || d.Priority != model.DefaultDevicePriority(model.DeviceColab) || d.Label != "Google Colab · T4" ||
		d.Model != "large-v3" || d.Precision != "float16" || d.Backend != model.BackendColab {
		t.Fatalf("row = %+v", d)
	}
	if !promptDismissed(t, st, model.DeviceColab) {
		t.Fatal("the workbench prompt of colab:remote is not dismissed")
	}
	if ok, _ := r.Usable(model.ASRDeviceSnapshot(d, "en", false)); !ok {
		t.Fatal("a fixed colab snapshot is not usable while Colab is present")
	}

	// The user's switch stays; label, model and precision follow Colab.
	if _, err := r.Update(tctx(), model.DeviceColab, model.ASRDevicePatch{Enabled: new(bool)}); err != nil {
		t.Fatal(err)
	}
	row := colabRow("L4")
	row.Model, row.Precision = "large-v3-turbo", "int8_float16"
	if err := r.SetRemote(tctx(), true, row); err != nil {
		t.Fatal(err)
	}
	if d, _ := r.Get(model.DeviceColab); d.Enabled || d.Label != "Google Colab · L4" || d.Model != "large-v3-turbo" || d.Precision != "int8_float16" {
		t.Fatalf("row after the settings change = %+v", d)
	}
	// Model and precision are Colab's settings, not the row's.
	m := "small"
	_, err := r.Update(tctx(), model.DeviceColab, model.ASRDevicePatch{Model: &m})
	wantCode(t, "patch colab model", err, ErrInvalid, CodeRemoteManaged, nil)

	// Not present: hidden from the list (the row stays), not usable; a
	// reorder without it keeps it last.
	if err := r.SetRemote(tctx(), false, colabRow("L4")); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(devIDs(r.List()), model.DeviceColab) || r.Present(model.DeviceColab) {
		t.Fatalf("colab:remote still listed: %v", devIDs(r.List()))
	}
	if _, ok := r.Get(model.DeviceColab); !ok {
		t.Fatal("the row was deleted")
	}
	if ok, _ := r.Usable(model.ASRDeviceSnapshot(d, "en", false)); ok {
		t.Fatal("a fixed colab snapshot is usable while Colab is off")
	}
	order := devIDs(r.List())
	slices.Reverse(order)
	got, err := r.Reorder(tctx(), order)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(devIDs(got), order) {
		t.Fatalf("reorder = %v, want %v", devIDs(got), order)
	}
	// A detection leaves it alone.
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if r.Present(model.DeviceColab) {
		t.Fatal("a detection made colab:remote present")
	}
	// The auto snapshot never falls back to a hidden colab row.
	for _, x := range r.List() {
		if _, err := r.Update(tctx(), x.ID, model.ASRDevicePatch{Enabled: new(bool)}); err != nil {
			t.Fatal(err)
		}
	}
	on := true
	if _, err := r.Update(tctx(), model.DeviceColab, model.ASRDevicePatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{Language: "en"}); ok {
		t.Fatalf("current snapshot on the hidden colab row: %+v", s)
	}
}

// The power-aware policy never sees colab:remote (SPEC-v3 V14.4).
func TestPolicyIgnoresRemote(t *testing.T) {
	cuda := PolicyDevice{ID: model.DeviceCUDA0, Enabled: true, Present: true, Slots: 1}
	npu := PolicyDevice{ID: model.DeviceOVNPU, Enabled: true, Present: true, Slots: 1}
	remote := PolicyDevice{ID: model.DeviceColab, Enabled: true, Present: true, Slots: 1}
	run := PolicyRun{JobID: "j-remote", DeviceID: model.DeviceColab}
	desk := PolicyInput{Power: Power{Known: true}, Mode: model.ASRDeviceModeAuto, MultiDevice: false,
		Devices: []PolicyDevice{cuda, remote}, Running: []PolicyRun{run}}
	if p := Place(desk, PolicyJob{JobID: "j"}); !slices.Equal(p.Devices, []string{model.DeviceCUDA0}) {
		t.Fatalf("desktop placement = %+v", p)
	}
	if v := View(desk); !slices.Equal(v.Active, []string{model.DeviceCUDA0}) {
		t.Fatalf("desktop view = %+v", v)
	}
	batt := PolicyInput{Power: Power{Known: true, OnBattery: true, HasBattery: true}, Mode: model.ASRDeviceModeAuto,
		Devices: []PolicyDevice{cuda, npu, remote}, Running: []PolicyRun{run}}
	if p := Place(batt, PolicyJob{JobID: "j"}); !slices.Equal(p.Devices, []string{model.DeviceOVNPU}) {
		t.Fatalf("battery placement next to a remote run = %+v", p)
	}
	if d := SelfTest(batt, model.DeviceOVNPU); !d.Allowed || !d.Start {
		t.Fatalf("self-test next to a remote run = %+v", d)
	}
	prev := Power{Known: true, HasBattery: true}
	if a := OnPowerChange(prev, batt); len(a) != 1 || a[0].Action != PowerContinue {
		t.Fatalf("power change of a remote run = %+v", a)
	}
}
