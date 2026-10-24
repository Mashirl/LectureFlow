// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"testing"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// SPEC-v3 V5 (v5-integration-b1): mlx:gpu is online when the detection
// found the Apple GPU.
func TestPresentInMLX(t *testing.T) {
	row := model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX}
	det := devices.Detection{At: time.Now()}
	if presentIn(row, det, true) {
		t.Error("mlx:gpu without a detected Apple GPU")
	}
	det.MLX = "Apple M3 Pro"
	if !presentIn(row, det, true) {
		t.Error("mlx:gpu with the Apple GPU detected")
	}
	if presentIn(model.ASRDevice{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO}, det, true) {
		t.Error("the Apple GPU does not make an OpenVINO device present")
	}
}
