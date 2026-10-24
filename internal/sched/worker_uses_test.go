// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// TestWorkerUses (v3 Gate round 1 item 5): admitted asr runs with the
// device they were bound to or their fixed snapshot, active self-tests,
// material extraction; remote runs, other stages and finished tests are
// left out.
func TestWorkerUses(t *testing.T) {
	snap := func(s model.ASRSnapshot) string { return model.MarshalSnapshot(s) }
	cuda := model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: "float16"}
	ov := model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: "int8"}
	colab := model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Model: "large-v3", Precision: "float16"}
	job := func(id, stage, cs string) model.Job {
		return model.Job{ID: id, Stage: stage, ConfigSnapshot: cs}
	}
	s := &Scheduler{
		running: map[string]*run{
			// auto snapshot, bound to cuda:0 at its start
			"j1": {job: job("j1", model.StageASR, snap(model.ASRSnapshot{Auto: true, Model: "large-v2", Language: "en"})), devRow: cuda, bind: true},
			// fixed snapshot on the OpenVINO GPU
			"j2": {job: job("j2", model.StageASR, snap(model.ASRSnapshot{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO,
				Model: "medium", Precision: "fp16", Language: "en"})), devRow: ov},
			// on Colab: nothing local
			"j3": {job: job("j3", model.StageASR, snap(model.ASRSnapshot{DeviceID: model.DeviceColab, Backend: model.BackendColab,
				Model: "large-v3", Precision: "float16"})), devRow: colab},
			"j4": {job: job("j4", model.StageMaterialExtract, model.EmptySnapshot)},
			"j5": {job: job("j5", model.StageTranslate, model.EmptySnapshot)},
		},
		tests: map[string]*devTestRun{
			model.DeviceOVNPU: {st: DeviceTestState{State: DeviceTestRunning},
				dev: model.ASRDevice{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "small", Precision: "int8"}},
			model.DeviceFWCPU: {st: DeviceTestState{State: DeviceTestWaiting},
				dev: model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Model: "small", Precision: "int8_float16"}},
			model.DeviceCUDA0: {st: DeviceTestState{State: "done"}, dev: cuda},
		},
	}
	got := s.WorkerUses()
	want := []WorkerUse{
		{What: UseASR, ID: "j1", Backend: model.BackendFasterWhisper, Model: "large-v3", Precision: "float16"},
		{What: UseASR, ID: "j2", Backend: model.BackendOpenVINO, Model: "medium", Precision: "fp16"},
		{What: UseDeviceTest, ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper},
		{What: UseDeviceTest, ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO},
		{What: UseMaterial, ID: "j4", Backend: "base"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("uses\n got %+v\nwant %+v", got, want)
	}
}
