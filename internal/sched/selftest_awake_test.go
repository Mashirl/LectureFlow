// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"slices"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// V12.2 acceptance issue 6 (NOTES "决定（v3 acc-fix-core）"): a device
// self-test on its own — no pipeline job — holds the keep-awake request
// from its start to its end, as a transcription does; a cancelled one
// releases it too.
func TestSelfTestAloneKeepsAwake(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	ft := newFakeTest(2)
	rec := &awakeRec{}
	de := newDevEnv(t, []model.ASRDevice{cuda}, func(o *Options) {
		o.DeviceTest = ft.run
		o.KeepAwake = rec.set
	})
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceCUDA0); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceCUDA0, DeviceTestRunning)
	<-ft.started
	rec.holdsOnly(t, "self-test running")
	ft.step <- struct{}{} // between two candidates: still held
	rec.holdsOnly(t, "between candidates")
	ft.step <- struct{}{}
	de.waitTest(model.DeviceCUDA0, DeviceTestDone)
	rec.heldThenReleased(de, "self-test done")

	// a cancelled self-test
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceCUDA0); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceCUDA0, DeviceTestRunning)
	<-ft.started
	de.waitFor("held again", 5*time.Second, func() bool { return slices.Equal(rec.get(), []bool{true, false, true}) })
	if _, err := de.s.CancelDeviceTest(model.DeviceCUDA0); err != nil {
		t.Fatal(err)
	}
	de.waitFor("released after cancel", 5*time.Second, func() bool { return slices.Equal(rec.get(), []bool{true, false, true, false}) })
}
