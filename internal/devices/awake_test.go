// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"errors"
	"reflect"
	"testing"
)

// fakeAwake replaces the platform calls (the unit tests never hold a real
// power request and never touch the display: no SC_MONITORPOWER).
type fakeAwake struct {
	calls   []string
	failSet error
}

func installFakeAwake(t *testing.T) *fakeAwake {
	t.Helper()
	f := &fakeAwake{}
	oc, os, ocl, ocls := awakeCreate, awakeSet, awakeClear, awakeClose
	awakeCreate = func(reason string) (uintptr, []uint16, error) {
		f.calls = append(f.calls, "create:"+reason)
		return 42, []uint16{'x', 0}, nil
	}
	awakeSet = func(h uintptr) error {
		f.calls = append(f.calls, "set")
		if h != 42 {
			t.Errorf("set on handle %d", h)
		}
		return f.failSet
	}
	awakeClear = func(h uintptr) error {
		f.calls = append(f.calls, "clear")
		return nil
	}
	awakeClose = func(h uintptr) error {
		f.calls = append(f.calls, "close")
		return nil
	}
	t.Cleanup(func() { awakeCreate, awakeSet, awakeClear, awakeClose = oc, os, ocl, ocls })
	return f
}

func TestKeepAwakeSequence(t *testing.T) {
	f := installFakeAwake(t)
	k, err := NewKeepAwake(KeepAwakeReason)
	if err != nil {
		t.Fatal(err)
	}
	if k.On() {
		t.Fatal("held right after creation")
	}
	for _, on := range []bool{true, true, false, false, true} {
		if err := k.Set(on); err != nil {
			t.Fatal(err)
		}
		if k.On() != on {
			t.Fatalf("On() = %v after Set(%v)", k.On(), on)
		}
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if k.On() {
		t.Fatal("held after Close")
	}
	// After Close nothing happens any more.
	if err := k.Set(true); err != nil || k.On() {
		t.Fatalf("Set after Close: %v %v", err, k.On())
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"create:" + KeepAwakeReason, "set", "clear", "set", "clear", "close"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}

	// A failed Set leaves the request released, and the next Set tries again.
	f2 := installFakeAwake(t)
	k2, _ := NewKeepAwake("r")
	f2.failSet = errors.New("denied")
	if err := k2.Set(true); err == nil || k2.On() {
		t.Fatalf("failed set: %v %v", err, k2.On())
	}
	f2.failSet = nil
	if err := k2.Set(true); err != nil || !k2.On() {
		t.Fatalf("retry: %v %v", err, k2.On())
	}
	// Close of a request that was never held only closes the handle.
	f3 := installFakeAwake(t)
	k3, _ := NewKeepAwake("r")
	k3.Close()
	if !reflect.DeepEqual(f3.calls, []string{"create:r", "close"}) {
		t.Fatalf("calls %v", f3.calls)
	}
	// A nil keep-awake (creation failed in cmd) is safe.
	var nk *KeepAwake
	if nk.Set(true) != nil || nk.On() || nk.Close() != nil {
		t.Fatal("nil KeepAwake")
	}
}

func TestKeepAwakeCreateError(t *testing.T) {
	installFakeAwake(t)
	awakeCreate = func(string) (uintptr, []uint16, error) { return 0, nil, errors.New("no request") }
	if k, err := NewKeepAwake("r"); err == nil || k != nil {
		t.Fatalf("create error: %v %v", k, err)
	}
}
