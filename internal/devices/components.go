// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// Model locations with the component manager (SPEC-v3 V3, step 5;
// v5-integration-b1): an installed model component
// (components/model__{backend}__{model}__{precision}/{version}/model/,
// config.Config.ComponentModelDir) takes precedence over the data
// directory's own locations, which stay the fallback (development, and
// OpenVINO models the worker converted itself). cfg may be nil (no
// components).

// OVModelPath is the OpenVINO model directory a run of mdl / precision
// uses: the model component when installed (its directory holds every
// ModelFiles entry), else ModelDir (where the worker converts a missing
// model).
func OVModelPath(cfg *config.Config, dataDir, mdl, precision string) string {
	if dir, ok := cfg.ComponentModelDir(model.BackendOpenVINO, mdl, precision); ok {
		return dir
	}
	return ModelDir(dataDir, mdl, precision)
}

// OVModelReady is ModelReady for OVModelPath: false means the first run
// prepares (converts) the model.
func OVModelReady(cfg *config.Config, dataDir, mdl, precision string) bool {
	return ModelFilesReady(OVModelPath(cfg, dataDir, mdl, precision))
}

// MLXModelPath is the MLX model directory a run of mdl / precision uses:
// the model component when installed, else MLXModelDir (a model placed
// there by hand; the worker cannot convert one).
func MLXModelPath(cfg *config.Config, dataDir, mdl, precision string) string {
	if dir, ok := cfg.ComponentModelDir(model.BackendMLX, mdl, precision); ok {
		return dir
	}
	return MLXModelDir(dataDir, mdl)
}

// FWModel is the job.json "model" of the faster-whisper worker: the model
// component's directory when installed (WhisperModel loads a local
// CTranslate2 directory; any installed precision serves, the files are the
// same), else the model name (the worker's Hugging Face cache, as before).
// The asr snapshot and its input hash keep the name (SPEC-v2 I6).
func FWModel(cfg *config.Config, mdl, precision string) string {
	if dir, ok := cfg.ComponentModelDir(model.BackendFasterWhisper, mdl, precision); ok {
		return dir
	}
	return mdl
}

// MLXModelFilesReady reports whether dir holds an MLX whisper model as
// worker/asr_mlx.py requires: a non-empty config.json and
// weights.safetensors or weights.npz.
func MLXModelFilesReady(dir string) bool {
	if !nonEmptyFile(filepath.Join(dir, "config.json")) {
		return false
	}
	return nonEmptyFile(filepath.Join(dir, "weights.safetensors")) || nonEmptyFile(filepath.Join(dir, "weights.npz"))
}

// FWModelFilesReady reports whether dir holds a CTranslate2 whisper model
// (a non-empty model.bin, as worker/asr.py checks).
func FWModelFilesReady(dir string) bool {
	return nonEmptyFile(filepath.Join(dir, "model.bin"))
}

func nonEmptyFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}
