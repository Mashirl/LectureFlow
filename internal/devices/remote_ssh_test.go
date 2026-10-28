// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// ssh-mvp: SSH servers' rows in the device table — inserted and updated by
// internal/sshremote, present only while the server is ready (never by a
// detection), removed with the server.

import (
	"errors"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

func TestSSHRows(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	r, changes := newReg(t, st, w)
	r.EnableSSH()
	row := model.ASRDevice{ID: "ssh:mac", Backend: model.BackendMLX, Label: "Mac", Model: "large-v3-turbo",
		Precision: model.PrecisionFP16, Priority: 400}
	if err := r.UpsertSSH(tctx(), row); err != nil {
		t.Fatal(err)
	}
	d, ok := r.Get("ssh:mac")
	if !ok || !d.Enabled || d.MaxParallel != 1 || d.Priority != 400 || d.Backend != model.BackendMLX {
		t.Fatalf("row %+v %v", d, ok)
	}
	if !slices.Contains(devIDs(r.List()), "ssh:mac") {
		t.Fatal("not listed")
	}
	// Its workbench prompt is dismissed (the server was added on purpose).
	for _, p := range r.Prompts() {
		if p.ID == "ssh:mac" {
			t.Fatal("prompted")
		}
	}
	// Not present until the server is ready; a detection does not change that.
	if r.Present("ssh:mac") {
		t.Fatal("present before ready")
	}
	r.SetSSHPresent("ssh:mac", true)
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if !r.Present("ssh:mac") {
		t.Fatal("a detection reset the server's presence")
	}
	// The user's switch and priority survive an update; label, backend,
	// model, precision follow.
	if _, err := r.Update(tctx(), "ssh:mac", model.ASRDevicePatch{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	row.Label, row.Model = "Mac mini", "medium"
	row.Priority = 1
	if err := r.UpsertSSH(tctx(), row); err != nil {
		t.Fatal(err)
	}
	d, _ = r.Get("ssh:mac")
	if d.Enabled || d.Priority != 400 || d.Label != "Mac mini" || d.Model != "medium" {
		t.Fatalf("after update %+v", d)
	}
	before := changes.Load()
	if err := r.RemoveSSH(tctx(), "ssh:mac"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("ssh:mac"); ok || r.Present("ssh:mac") || changes.Load() == before {
		t.Fatal("not removed")
	}
	// Only ssh ids, only the engines a server runs.
	for _, bad := range []model.ASRDevice{
		{ID: "cuda:1", Backend: model.BackendFasterWhisper},
		{ID: "ssh:x", Backend: model.BackendOpenVINO},
		{ID: "ssh:Bad", Backend: model.BackendMLX},
	} {
		if err := r.UpsertSSH(tctx(), bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// With the SSH servers off (security review 3, L6: the default) an ssh row
// that is in the database from an earlier run is not in the table, not in
// the snapshots and never present; it comes back when they are switched on.
func TestSSHRowsHiddenWhileOff(t *testing.T) {
	st := regStore(t)
	row := model.ASRDevice{ID: "ssh:mac", Backend: model.BackendMLX, Label: "Mac", Enabled: true, Model: "small",
		Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 900}
	if err := st.InsertASRDevice(tctx(), row); err != nil {
		t.Fatal(err)
	}
	r, _ := newReg(t, st, newWorld())
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(devIDs(r.List()), "ssh:mac") {
		t.Fatal("an ssh row is listed while the SSH servers are off")
	}
	if snap, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto, Language: "en"}); ok && snap.DeviceID == "ssh:mac" {
		t.Fatal("the auto snapshot names an ssh row while the SSH servers are off")
	}
	r.SetSSHPresent("ssh:mac", true)
	if r.Present("ssh:mac") {
		t.Fatal("present while off")
	}
	if _, err := st.GetASRDevice(tctx(), "ssh:mac"); err != nil {
		t.Fatal("the row is gone from the database")
	}
	r.EnableSSH()
	if !slices.Contains(devIDs(r.List()), "ssh:mac") {
		t.Fatal("not listed once on")
	}
}

func ptr[T any](v T) *T { return &v }
