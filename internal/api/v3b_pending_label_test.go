// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// 3b Gate round 2, suggestion 8: the fallback auto snapshot of a launch
// that could not name a device is shown as "no device chosen yet"
// ("自动（等待可用设备） · en"; in the dialog an asr view without a device,
// model or precision) until the job starts and is bound; afterwards, and
// for any other snapshot, the usual "device · model · precision · language"
// label.
func TestASRPendingLabel(t *testing.T) {
	e := newEnv(t)
	fallback := model.MarshalSnapshot(settings.FallbackASRSnapshot("en"))
	bound := model.MarshalSnapshot(model.ASRSnapshot{DeviceID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper,
		Model: "large-v3", Precision: model.PrecisionAuto, Language: "en", Auto: true})
	asrView := func(status, raw string) *StageModel {
		t.Helper()
		jobs := []model.Job{{ID: "j-" + status, Scope: model.ScopeLecture, ScopeID: "L", Stage: model.StageASR, Status: status,
			IsCurrent: true, ConfigSnapshot: raw}}
		for _, v := range e.srv.stageViews(model.ScopeLecture, jobs, false, "") {
			if v.Stage == model.StageASR {
				return v.Model
			}
		}
		t.Fatal("no asr view")
		return nil
	}
	for _, status := range []string{model.StatusPending, model.StatusQueued} {
		if m := asrView(status, fallback); m == nil || m.Label != ASRPendingLabel+" · en" || m.Model != "" ||
			m.ASR == nil || !model.EqualASRSnapshot(*m.ASR, model.ASRSnapshot{Language: "en", Auto: true}) {
			t.Fatalf("%s with the fallback snapshot: %+v", status, m)
		}
		if m := asrView(status, bound); m == nil || m.Label != "cuda:0 · large-v3 · auto · en" || m.Model != "large-v3" ||
			m.ASR == nil || m.ASR.DeviceID != model.DeviceCUDA0 {
			t.Fatalf("%s with a device snapshot: %+v", status, m)
		}
	}
	// Once it ran (bound to fw:cpu with the same values) it is a real device.
	if m := asrView(model.StatusDone, fallback); m == nil || m.Label != "fw:cpu · small · auto · en" || m.Model != "small" ||
		m.ASR == nil || m.ASR.DeviceID != model.DeviceFWCPU {
		t.Fatalf("done: %+v", m)
	}

	// The "current settings" side of the rerun dialog.
	if v := e.srv.currentView(model.StageASR, fallback); v.Label != ASRPendingLabel+" · en" || v.Model != "" ||
		v.ASR == nil || !model.EqualASRSnapshot(*v.ASR, model.ASRSnapshot{Language: "en", Auto: true}) {
		t.Fatalf("current fallback: %+v (asr %+v)", v, v.ASR)
	}
	if v := e.srv.currentView(model.StageASR, bound); v.Label != "cuda:0 · large-v3 · auto · en" || v.Model != "large-v3" || v.ASR == nil || v.ASR.DeviceID != model.DeviceCUDA0 {
		t.Fatalf("current device: %+v", v)
	}
	if v, w := e.srv.currentView(model.StageSummary, "{}"), e.srv.snapshotView(model.StageSummary, "{}"); v.Label != w.Label {
		t.Fatalf("other stages unchanged: %+v vs %+v", v, w)
	}
}
