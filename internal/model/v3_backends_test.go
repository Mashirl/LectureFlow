// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import "testing"

// SPEC-v3 V3.1, V5, V14: the device helpers know mlx:gpu and colab:remote.
func TestV3DeviceHelpers(t *testing.T) {
	cases := []struct {
		id, backend, kind string
		prio              int
	}{
		{DeviceMLXGPU, BackendMLX, DeviceKindApple, 250},
		{DeviceColab, BackendColab, DeviceKindRemote, 5},
		{DeviceCUDA0, BackendFasterWhisper, DeviceKindCUDA, 300},
		{DeviceOVGPU, BackendOpenVINO, DeviceKindIGPU, 200},
		{"metal:x", "", "", 0},
	}
	for _, c := range cases {
		if b := DeviceBackend(c.id); b != c.backend {
			t.Errorf("%s: backend %q", c.id, b)
		}
		if k := DeviceKind(c.id); k != c.kind {
			t.Errorf("%s: kind %q", c.id, k)
		}
		if p := DefaultDevicePriority(c.id); p != c.prio {
			t.Errorf("%s: priority %d", c.id, p)
		}
	}
	// local devices before Colab, Colab before the OpenVINO CPU
	if !(DefaultDevicePriority(DeviceFWCPU) > DefaultDevicePriority(DeviceColab) && DefaultDevicePriority(DeviceColab) > DefaultDevicePriority(DeviceOVCPU)) {
		t.Error("colab priority order")
	}
	for _, p := range []string{PrecisionFP16, PrecisionINT8, PrecisionINT4} {
		if !ValidPrecision(BackendMLX, p) {
			t.Errorf("mlx %s", p)
		}
	}
	if ValidPrecision(BackendMLX, PrecisionAuto) || ValidPrecision(BackendMLX, PrecisionFloat16) {
		t.Error("mlx takes the converted model's names")
	}
	if !ValidPrecision(BackendColab, PrecisionFloat16) || ValidPrecision(BackendColab, PrecisionFP16) {
		t.Error("colab takes faster-whisper's names")
	}
	if FWDevice(DeviceMLXGPU) != "" || OVDeviceName(DeviceColab) != "" {
		t.Error("v3 ids are no faster-whisper / OpenVINO devices")
	}
}
