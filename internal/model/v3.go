// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// v3 (SPEC-v3.md) constants frozen by the lead before the parallel work
// (.claude/v3-brief.md). Migration 009 adds the backends to the asr_device
// CHECK; the helpers of v2_intel.go (DeviceBackend, DeviceKind,
// ValidPrecision, DefaultDevicePriority) know them (v5-integration-a).

// ASR backends added by v3.
const (
	// BackendMLX is worker/asr_mlx.py on Apple Silicon (component asr-mlx, V3.1, V5).
	BackendMLX = "mlx"
	// BackendColab is remote transcription on Google Colab (V14).
	BackendColab = "colab"
)

// Device ids added by v3.
const (
	DeviceMLXGPU = "mlx:gpu"      // V3.1: the Apple GPU through MLX
	DeviceColab  = "colab:remote" // V14: the remote Colab runtime
)

// Device kinds added by v3 (DeviceKind).
const (
	DeviceKindApple  = "apple"
	DeviceKindRemote = "remote"
)

// waiting_for values of the Colab backend (V14.5, SPEC §9.5).
const (
	WaitRemoteQueue   = "remote_queue"   // waiting for Colab to assign the accelerator
	WaitRemoteWindow  = "remote_window"  // this borrowing session's fair-use window is used up
	WaitRemoteAuth    = "remote_auth"    // credential expired or missing: authorise again
	WaitRemoteBalance = "remote_balance" // not enough compute units
)

// kv keys of the desktop app settings (SPEC-v3 V1.1, V11; .claude/v3-brief.md §2).
const (
	KVAppCloseBehavior = "app.close_behavior" // ask | background | quit
	// KVAppPreventSleep is "1" | "0"; missing or unrecognised = on. When
	// off, the v2-power keep-awake request is not taken either.
	KVAppPreventSleep  = "app.prevent_sleep"
	KVAppUpdateChannel = "app.update_channel" // stable | beta
	KVOnboardingState  = "onboarding.state"   // OnboardingState JSON (V4.1)
)
