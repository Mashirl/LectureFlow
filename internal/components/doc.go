// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package components is the component manager of SPEC-v3 V3 and V8: the
// components.json manifest (parsing, validation, compatibility with the
// application version, newer manifests from the network), install plans
// (dependency closure, download size, disk space), background downloads with
// HTTP Range resume, pause / resume / cancel, SHA-256 verification with one
// retry, mirror sources with automatic fallback to the default source,
// python-build-standalone runtimes, uv virtual environments from hash-pinned
// lock files, vendor tools (the Google Cloud CLI, tool.go), offline
// component archives, uninstall, and updates that keep the old version
// until the new one passed its self-test.
//
// Layout under {data_dir}/components (state.json stores only paths relative
// to this directory, so the whole data directory can move, V1.3):
//
//	state.json                         installed components (atomic writes)
//	manifest.json, manifest.d/         a newer manifest from the network and its lock files
//	downloads/{sha256}[.part]          downloads (content addressed: resumable across tasks and restarts)
//	.uvc/                              uv's cache while a task runs (removed when no task is active; ".uv-cache" before V12.2)
//	{dir}/{version}/installed.json     receipt, written last; {dir} = id with ":" replaced by "__"
//	{dir}/{version}/venv/              venv and python components
//	{dir}/{version}/runtime/           python component: the unpacked python-build-standalone archive
//	{dir}/{version}/model/             model components
//	{dir}/{version}/bin/               binary components
//	{dir}/{version}/tool/              tool components: the vendor's archive (ToolPath, ToolEnv)
//	{dir}/.s{n}/                       work in progress (n = 1, 2, …, created exclusively); renamed to {version}
//	                                   when complete (".staging-{version}-{task}" before V12.2; both are removed at start)
//	.imports/{task}.zip                ImportFile: the private copy of the archive (removed when the task ends and at start)
//
// A component is installed only when state.json lists it; Python, ModelDir
// and List never report a directory that state.json does not reference.
//
// The names are short because Windows limits paths (pathlen.go): {dir} stays
// the id (cmd/lectureflow maps directories back to ids), the version is the
// manifest's semver; the deepest files are a venv's site-packages, so on
// Windows Plan refuses an install whose deepest file (Build.MaxPath) would
// exceed MaxPathLen under the data directory (components.path_too_long).
//
// # Components in use (Options.InUse)
//
// Uninstall, and the switch at the end of an update (when state.json moves
// to the new version and the old directory is deleted), first ask
// Options.InUse(id); while it returns true they are refused with
// components.in_use_by_task {id, reason} (HTTP 409). Update asks once more
// when it is called, so most refusals come before any download. InUse is
// called without the manager's lock, so it may call Python and ModelDir.
// The caller answers true when any of these uses the component's
// interpreter (Python(backend) of a venv component; the python component's
// base venv and runtime) or its model directory (ModelDir):
//
//   - asr that is running, or queued and already dispatched to start, on a
//     backend whose venv is the component, or with the model that is the
//     component (also through the faster-whisper precision fallback of
//     ModelDir), or on any backend when the id is "python" (every venv
//     runs on its runtime) — reason "asr";
//   - the device self-test (V2) on that backend or model — "device_test";
//   - a component self-check (the import / worker check of the diagnostics
//     page) or a device-detection probe on that environment — "self_check";
//   - for "python": PDF extraction running with the base venv, of slides or
//     of a course syllabus — "material".
//
// The answer is a snapshot: a job that starts right after InUse returned
// false is not stopped, so callers that dispatch work should also avoid
// starting asr on a component that an active components task is updating
// (List reports that task in Status.Task).
package components
