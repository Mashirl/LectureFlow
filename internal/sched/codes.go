// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Codes of the scheduler's job errors and report notes (v2 P14.3); the
// Chinese texts are in testdata/api/error_codes_backend.json.
const (
	// CodeProviderDeleted: the provider of an API request was deleted (or
	// never existed) while the request waited for the pool. params provider.
	CodeProviderDeleted = "llm.provider_deleted"
	// CodeNoteReviewIncomplete: the janitor report note of the G2 fallback
	// after a glossary review that did not complete. params status (failed
	// or cancelled).
	CodeNoteReviewIncomplete = "janitor.fallback_incomplete"
	// CodeSelfTestRunning: a self-test of the device is already waiting or
	// running (SPEC-v2 I9; ErrSelfTestRunning, HTTP 409). No params.
	CodeSelfTestRunning = "selftest.running"
	// CodeSelfTestFailed: a self-test failed with an error that has no code
	// of its own (DeviceTestState.Error; the original text is the detail).
	// No params (3b Gate round 1, item 7).
	CodeSelfTestFailed = "selftest.failed"

	// Power management (v2-step20 freeze, power.go).
	// CodeSelfTestPowerBattery: on battery only the battery device may be
	// tested (devices.SelfTest Allowed false): StartDeviceTest refuses
	// (HTTP 409) and a waiting or running test is cancelled with it as the
	// state's error. params device (the tested device's label).
	CodeSelfTestPowerBattery = "selftest.power_battery"
	// CodeSelfTestPowerCPU: on a machine with a battery a CPU device is
	// never tested, in any power state (it is never used for transcription
	// there; backend Gate round 1, fix 7): StartDeviceTest refuses (HTTP 409,
	// like CodeSelfTestPowerBattery) and a waiting or running test is
	// cancelled with it as the state's error. params device (label).
	CodeSelfTestPowerCPU = "selftest.power_cpu"
	// Notices of the power policy (kind note; PowerNotice.Info and the
	// stage card's note of job.stats.power_event). params lecture (title),
	// device (label).
	CodePowerSwitched  = "power.switched"  // lecture, device
	CodePowerSuspended = "power.suspended" // lecture
	CodePowerResumed   = "power.resumed"   // lecture, device

	// SPEC-v3 V14 (Colab remote transcription, remote.go).
	// CodeRemoteDisabled: "改用 Colab 处理" while Colab is off (settings) or
	// its device row is disabled (HTTP 409). No params.
	CodeRemoteDisabled = "remote.disabled"
	// CodeRemoteUnavailable: "改用 Colab 处理" while Colab cannot take a
	// lecture (HTTP 409). params waiting_for (model.WaitRemote*: remote_auth,
	// remote_window, remote_balance; the front end translates it).
	CodeRemoteUnavailable = "remote.unavailable"
	// CodeRemoteBackToLocal (note): Colab handed the lecture back and it
	// waits for the local devices (stage card of a queued or running asr
	// job). params cause (the colab.* error code).
	CodeRemoteBackToLocal = "remote.back_to_local"
	// CodeRemoteADCSignedOut (note): "断开连接" also signed gcloud's own
	// application-default login out (DELETE /api/colab/auth). No params.
	CodeRemoteADCSignedOut = "remote.gcloud_signed_out"
	// CodeSelfTestRemote: colab:remote has no device self-test (HTTP 409).
	// params device (label).
	CodeSelfTestRemote = "selftest.remote"
)

// ErrorCodes lists the codes this package defines.
var ErrorCodes = []string{CodeProviderDeleted, CodeNoteReviewIncomplete, CodeSelfTestRunning, CodeSelfTestFailed,
	CodeSelfTestPowerBattery, CodeSelfTestPowerCPU, CodePowerSwitched, CodePowerSuspended, CodePowerResumed,
	CodeRemoteDisabled, CodeRemoteUnavailable, CodeRemoteBackToLocal, CodeRemoteADCSignedOut, CodeSelfTestRemote}
