// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// Whether a local device can start a transcription now (fresh-cuda; NOTES
// "决定（fresh-cuda：core-builder，2026-10-27）"). The detection sees an
// NVIDIA card through nvidia-smi alone, so cuda:0 (and the built-in fw:cpu)
// is enabled and present on a fresh install before the asr-cuda component
// is installed (v5-integration-b1: "没装 asr-cuda 时 cuda:0 照样出现"); a
// lecture the scheduler sent there failed for good with
// worker.fw_not_installed, also when Colab could have taken it, and one
// sent while the model was still downloading failed with
// devices.model_not_installed. With the component manager the scheduler
// therefore admits a local device only when its worker would start
// (WorkerState): the backend's interpreter exists and the run's model is
// ModelAvailable. Otherwise the job waits for that device with one of the
// codes below, and Colab's overflow takes it as for a computer without a
// local device (missing) or with a busy one (installing). Nothing changes
// once the components are installed, without a component manager, or in
// test mode.

// waiting_for codes of a job whose devices lack a component (the device
// id after the prefix, as model.WaitDevicePaused).
const (
	// WaitComponentMissingPrefix: component_missing:{id}, the environment
	// of the device's backend (asr-cuda, asr-openvino, asr-mlx) is not
	// installed and no task installs it.
	WaitComponentMissingPrefix = "component_missing:"
	// WaitModelMissingPrefix: model_missing:{id}, the backend is installed
	// but the model the run needs (the device's, or a fixed snapshot's) is
	// not, and no task installs it.
	WaitModelMissingPrefix = "model_missing:"
	// WaitComponentInstallingPrefix: component_installing:{id}, an
	// unfinished task (queued, running or paused) installs what is missing:
	// the job starts on the device once it is done.
	WaitComponentInstallingPrefix = "component_installing:"
)

func WaitComponentMissing(id string) string    { return WaitComponentMissingPrefix + id }
func WaitModelMissing(id string) string        { return WaitModelMissingPrefix + id }
func WaitComponentInstalling(id string) string { return WaitComponentInstallingPrefix + id }

// WorkerState values.
const (
	WorkerReady       = ""             // the worker starts (or this is not decided here)
	WorkerNoComponent = "no_component" // the backend's interpreter is missing
	WorkerNoModel     = "no_model"     // the run's model is missing
	WorkerInstalling  = "installing"   // what is missing is being installed
)

// ComponentTasks is what the component manager tells about its unfinished
// install and update tasks (*components.Manager implements it; a
// config.ComponentLocator without it has none: missing is then never
// "being installed").
type ComponentTasks interface {
	// PythonInstalling: an unfinished task installs the environment of
	// backend (its venv component is an item not done yet).
	PythonInstalling(backend string) bool
	// ModelInstalling: an unfinished task installs the model
	// (faster-whisper: any precision of name, as Manager.ModelDir).
	ModelInstalling(backend, name, precision string) bool
}

// WorkerState reports whether a run of backend / mdl / precision on a local
// device would start now: WorkerReady, or why not — the backend's worker
// interpreter (config.Config.WorkerPython: the component's, else a
// configured one that exists) is missing (WorkerNoComponent), or its model
// is not ModelAvailable (WorkerNoModel); WorkerInstalling instead when an
// unfinished task of the component manager installs the missing part.
// Always WorkerReady without a component manager (the earlier behaviour:
// the stage reports a missing interpreter), in test mode (cfg.ASR.FakeResult:
// no worker runs) and for a backend without a local worker (colab, ssh).
// It may stat files and take the manager's lock: never call it with a lock
// of the caller held. dataDir as ModelAvailable.
func WorkerState(cfg *config.Config, dataDir, backend, mdl, precision string) string {
	if cfg == nil || cfg.Components == nil || cfg.ASR.FakeResult != "" {
		return WorkerReady
	}
	switch backend {
	case model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX:
	default:
		return WorkerReady
	}
	tasks, _ := cfg.Components.(ComponentTasks)
	if py, _, component := cfg.WorkerPython(backend); !component && !executableExists(py) {
		if tasks != nil && tasks.PythonInstalling(backend) {
			return WorkerInstalling
		}
		return WorkerNoComponent
	}
	if !ModelAvailable(cfg, dataDir, backend, mdl, precision) {
		if tasks != nil && tasks.ModelInstalling(backend, mdl, precision) {
			return WorkerInstalling
		}
		return WorkerNoModel
	}
	return WorkerReady
}

// WorkerWait is the waiting_for of a job that WorkerState kept off device
// id ("" for WorkerReady).
func WorkerWait(state, id string) string {
	switch state {
	case WorkerNoComponent:
		return WaitComponentMissing(id)
	case WorkerNoModel:
		return WaitModelMissing(id)
	case WorkerInstalling:
		return WaitComponentInstalling(id)
	}
	return ""
}
