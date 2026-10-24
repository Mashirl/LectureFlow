// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// Installed models only (V12.2 acceptance, issue 3; NOTES "决定（v3
// acc-fix-core）"). With the component manager, a worker that runs from its
// backend's component environment must not fetch a model by itself: the
// faster-whisper worker would load a model name from the global Hugging
// Face cache, downloading it from huggingface.co outside the data
// directory and past the configured mirror; the OpenVINO worker would
// download and convert one. Such a run uses installed models only, and a
// missing one is an error that points to "Components". Development (no
// component manager, or the backend's configured interpreter such as
// .venv) keeps the earlier behaviour.

// ComponentModelsOnly reports whether runs of backend may use only models
// already on this computer: cfg has a component manager and the backend's
// worker runs from its component environment (config.Config.WorkerPython).
func ComponentModelsOnly(cfg *config.Config, backend string) bool {
	if cfg == nil || cfg.Components == nil {
		return false
	}
	switch backend {
	case model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX:
	default:
		return false
	}
	_, _, component := cfg.WorkerPython(backend)
	return component
}

// ModelAvailable reports whether a run of backend / mdl / precision finds
// its model without downloading anything. Always true unless
// ComponentModelsOnly; then faster-whisper needs the model component (any
// installed precision, as FWModel), OpenVINO the component or a model
// already converted into ModelDir (OVModelReady), MLX the model files at
// MLXModelPath. dataDir should be absolute (as the asr stage passes it).
func ModelAvailable(cfg *config.Config, dataDir, backend, mdl, precision string) bool {
	if !ComponentModelsOnly(cfg, backend) {
		return true
	}
	switch backend {
	case model.BackendFasterWhisper:
		_, ok := cfg.ComponentModelDir(backend, mdl, precision)
		return ok
	case model.BackendOpenVINO:
		return OVModelReady(cfg, dataDir, mdl, precision)
	case model.BackendMLX:
		return MLXModelFilesReady(MLXModelPath(cfg, dataDir, mdl, precision))
	}
	return true
}

// InstalledCandidates is Candidates(dev.ID) without the combinations whose
// model is not ModelAvailable (the device self-test runs these). Without
// ComponentModelsOnly it is Candidates unchanged.
func InstalledCandidates(cfg *config.Config, dataDir string, dev model.ASRDevice) []Candidate {
	all := Candidates(dev.ID)
	backend := dev.Backend
	if backend == "" {
		backend = model.DeviceBackend(dev.ID)
	}
	if !ComponentModelsOnly(cfg, backend) {
		return all
	}
	out := make([]Candidate, 0, len(all))
	for _, c := range all {
		if ModelAvailable(cfg, dataDir, backend, c.Model, c.Precision) {
			out = append(out, c)
		}
	}
	return out
}

// CandidateModels lists the distinct model names of the device's main
// (non-fallback) candidates in order: the models to install for its
// self-test (the models param of CodeSelfTestNoModel).
func CandidateModels(deviceID string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range Candidates(deviceID) {
		if !c.Fallback && !seen[c.Model] {
			seen[c.Model] = true
			out = append(out, c.Model)
		}
	}
	return out
}
