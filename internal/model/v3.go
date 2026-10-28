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
	// WaitRemoteBusy (colabfix): the job waits for Colab, which runs another
	// lecture (or is being started or released), and no local device could
	// take it either (the local reason would name a missing or paused
	// device, e.g. power_no_device when only Colab transcribes).
	WaitRemoteBusy = "remote_busy"
	// WaitRemoteFallback (colabfix): Colab handed the lecture back
	// (stats.remote_fallback: it is never sent there automatically again)
	// and no local device can take it; the asr stage offers
	// ActionUseColab ("改用 Colab 处理").
	WaitRemoteFallback = "remote_fallback"
	// WaitRemoteHeld (colabfix2): the job could go to Colab, but the
	// automatic dispatch is held after "处理完当前这节后关机" / "立即关机"
	// (until no such lecture waits), and no local device can take it either
	// (or it may run on Colab only: a fixed colab:remote snapshot). The asr
	// stage offers ActionUseColab, which ends the hold.
	WaitRemoteHeld = "remote_held"
)

// ActionUseColab (colabfix) is offered in an asr stage's allowed_actions
// while the job waits with WaitRemoteFallback or WaitRemoteHeld: "改用 Colab
// 处理" (POST /api/lectures/{id}/use-colab, not the stage action endpoint).
const ActionUseColab = "use_colab"

// kv keys of the desktop app settings (SPEC-v3 V1.1, V11; .claude/v3-brief.md §2).
const (
	KVAppCloseBehavior = "app.close_behavior" // ask | background | quit
	// KVAppPreventSleep is "1" | "0"; missing or unrecognised = on. When
	// off, the v2-power keep-awake request is not taken either.
	KVAppPreventSleep  = "app.prevent_sleep"
	KVAppUpdateChannel = "app.update_channel" // stable | beta
	KVOnboardingState  = "onboarding.state"   // OnboardingState JSON (V4.1)
)
