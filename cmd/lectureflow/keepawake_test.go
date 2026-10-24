// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// keepAwakeHook (V12.2 acceptance issue 6): the scheduler's KeepAwake —
// called for the asr runs and the device self-tests alike (sched
// TestSelfTestAloneKeepsAwake) — takes the power request only while
// app.prevent_sleep is on; a release always reaches the request.
func TestKeepAwakeHookPreventSleep(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	var calls []bool
	hook := keepAwakeHook(ctx, st, func(on bool) error { calls = append(calls, on); return nil })

	hook(true) // default: on
	hook(false)
	if !slices.Equal(calls, []bool{true, false}) {
		t.Fatalf("prevent_sleep default: %v", calls)
	}
	if err := st.SetKV(ctx, model.KVAppPreventSleep, "0"); err != nil {
		t.Fatal(err)
	}
	calls = nil
	hook(true)
	hook(false)
	if !slices.Equal(calls, []bool{false}) {
		t.Fatalf("prevent_sleep off: %v, want only the release", calls)
	}
	if err := st.SetKV(ctx, model.KVAppPreventSleep, "1"); err != nil {
		t.Fatal(err)
	}
	calls = nil
	hook(true)
	if !slices.Equal(calls, []bool{true}) {
		t.Fatalf("prevent_sleep on again: %v", calls)
	}
}
