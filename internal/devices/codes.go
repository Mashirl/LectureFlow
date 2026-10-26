// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

// Error codes of this package (SPEC-v2 P14.3; phase 3b freeze). The Chinese
// texts with their {param} placeholders are in
// testdata/api/error_codes_backend.json, the zh-CN / en texts in
// web/i18n (errors.{code}). params named device hold the device's label
// (its own name, not translated) or, when there is no row, its id.
const (
	// Registry.Update / Reorder / ApplyBenchmark (HTTP 400, 404)
	CodeNotFound           = "devices.not_found"            // device
	CodeModelMissing       = "devices.model_missing"        //
	CodeModelInvalid       = "devices.model_invalid"        // value (an OpenVINO model outside ModelChoices)
	CodePrecisionInvalid   = "devices.precision_invalid"    // value (model.ValidPrecision of the backend)
	CodeMaxParallelInvalid = "devices.max_parallel_invalid" // min, max (1..8)
	CodeOrderInvalid       = "devices.order_invalid"        // the ids are not every device exactly once
	CodeApplyNotTested     = "devices.apply_not_tested"     // model, precision: not an OK result of the last self-test
	// CodeRemoteManaged: colab:remote's model and precision are Colab's
	// settings (PUT /api/colab/settings), not the device row's (SPEC-v3 V14).
	CodeRemoteManaged = "devices.remote_managed"

	// Detection.OVErr: why the OpenVINO devices are not available
	CodeOVPythonMissing = "devices.ov_python_missing" // path (config python_openvino); also the asr stage's job error
	CodeOVImportFailed  = "devices.ov_import_failed"  // error (worker --probe exit 2: openvino cannot be imported)
	CodeProbeFailed     = "devices.probe_failed"      // error (the probe failed otherwise: timeout, bad output)

	// The asr stage's job error when the MLX worker's interpreter is
	// missing (SPEC-v3 V5; the asr-mlx component is not installed and
	// config python_mlx does not exist).
	CodeMLXPythonMissing = "devices.mlx_python_missing" // path

	// No interpreter at all (3.1.0): in the desktop application without
	// the asr-openvino / asr-mlx component and without the key in
	// config.toml (config.Config.WorkerPython returns ""). Detection.OVErr
	// and the asr stage's job error, pointing to the Components page.
	CodeOVNotInstalled  = "devices.ov_not_installed"  //
	CodeMLXNotInstalled = "devices.mlx_not_installed" //

	// Installed models only (installed.go, V12.2 acceptance issue 3):
	// CodeModelNotInstalled is the asr stage's job error when the run's
	// model is not ModelAvailable; CodeSelfTestNoModel the self-test's
	// error when none of the device's candidates is (models: the
	// CandidateModels joined with " / ").
	CodeModelNotInstalled = "devices.model_not_installed" // model, precision
	CodeSelfTestNoModel   = "selftest.no_model"           // device, models

	// Registry.Usable: why "沿用原模型" is not available for an asr
	// snapshot (A4.3; the same area as settings' snapshot.unusable.*)
	CodeSnapshotDeviceUnavailable = "snapshot.unusable.device_unavailable" // device
)

// ErrorCodes lists the codes this package defines (cmd's
// TestBackendErrorCodesCatalog checks them against the catalog).
var ErrorCodes = []string{
	CodeNotFound, CodeModelMissing, CodeModelInvalid, CodePrecisionInvalid, CodeMaxParallelInvalid,
	CodeOrderInvalid, CodeApplyNotTested, CodeRemoteManaged,
	CodeOVPythonMissing, CodeOVImportFailed, CodeProbeFailed, CodeMLXPythonMissing,
	CodeOVNotInstalled, CodeMLXNotInstalled,
	CodeModelNotInstalled, CodeSelfTestNoModel,
	CodeSnapshotDeviceUnavailable,
}
