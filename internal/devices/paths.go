// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"strings"
)

// Locations of the OpenVINO models (SPEC-v2 I1, I7: data/models/openvino/,
// the same names as the 3a benchmark so its converted models and NPU
// compile caches are reused; lead ruling 3). FROZEN by the lead for phase
// 3b; written at the freeze (not a stub). Every function is pure apart from
// ModelReady / CompileCached, which only stat files.

// ModelRoot is {data_dir}/models/openvino.
func ModelRoot(dataDir string) string { return filepath.Join(dataDir, "models", "openvino") }

// ModelDir is the converted model: {root}/whisper-{model}-{precision}
// (precision fp16 | int8 | int4). The worker converts into a temporary
// sibling directory and renames it here (lead ruling 11).
func ModelDir(dataDir, mdl, precision string) string {
	return filepath.Join(ModelRoot(dataDir), "whisper-"+mdl+"-"+precision)
}

// CacheDir is the compile cache of one device and model:
// {root}/cache/{GPU|NPU|CPU}-{model}-{precision} (ovDev = model.OVDeviceName).
// The worker passes it as CACHE_DIR for GPU and NPU; CPU runs without one
// (the job's cache_dir is "" there).
func CacheDir(dataDir, ovDev, mdl, precision string) string {
	return filepath.Join(ModelRoot(dataDir), "cache", ovDev+"-"+mdl+"-"+precision)
}

// HFHome is the Hugging Face cache the worker downloads into (HF_HOME,
// lead ruling 4): {root}/hf_cache.
func HFHome(dataDir string) string { return filepath.Join(ModelRoot(dataDir), "hf_cache") }

// HFID is the Hugging Face model a whisper model is converted from.
func HFID(mdl string) string { return "openai/whisper-" + mdl }

// PrepareMinFreeBytes is the free space the worker requires on the drive of
// ModelRoot before it downloads and converts a model (lead ruling 4, the
// same 10 GB as SPEC-v2 D6); less → asr.ov_no_space. Sent as the job's
// min_free_bytes.
const PrepareMinFreeBytes int64 = 10 << 30

// ModelFiles are the files of a converted model that WhisperPipeline loads
// (optimum-intel's export with the tokenizers, as in 3a). A directory with
// all of them is ready.
var ModelFiles = []string{
	"config.json", "generation_config.json", "preprocessor_config.json",
	"openvino_encoder_model.xml", "openvino_encoder_model.bin",
	"openvino_decoder_model.xml", "openvino_decoder_model.bin",
	"openvino_tokenizer.xml", "openvino_tokenizer.bin",
	"openvino_detokenizer.xml", "openvino_detokenizer.bin",
}

// ModelReady reports whether ModelDir holds a converted model (every
// ModelFiles entry is a non-empty file). false means the first run on any
// OpenVINO device prepares the model ("首次使用需要准备模型").
func ModelReady(dataDir, mdl, precision string) bool {
	return ModelFilesReady(ModelDir(dataDir, mdl, precision))
}

// ModelFilesReady reports whether dir holds every ModelFiles entry as a
// non-empty file (a converted OpenVINO model: the data directory's own,
// ModelDir, or a model component's, OVModelPath).
func ModelFilesReady(dir string) bool {
	for _, f := range ModelFiles {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// CompileCached reports whether CacheDir holds a compiled blob for the
// device (ovDev GPU or NPU): false means the first run compiles (NPU about
// 4 minutes for large-v3-turbo). Always true for CPU, which needs none.
func CompileCached(dataDir, ovDev, mdl, precision string) bool {
	if ovDev == "CPU" {
		return true
	}
	entries, err := os.ReadDir(CacheDir(dataDir, ovDev, mdl, precision))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".blob") {
			if fi, err := e.Info(); err == nil && fi.Size() > 0 {
				return true
			}
		}
	}
	return false
}
