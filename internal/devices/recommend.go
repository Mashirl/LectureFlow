// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"lectureflow/internal/model"
)

// Recommendation data and self-test candidates (SPEC-v2 I5, I6, I9).
// FROZEN by the lead for phase 3b; written at the freeze (not a stub).

//go:embed recommendation.json
var recommendationJSON []byte

// Candidate is a model + precision combination of a device.
type Candidate struct {
	Model     string
	Precision string
	// Fallback: the self-test tries it only when no earlier candidate
	// reached RTF <= model.PickMaxRTF (a smaller model is never expected to
	// be more accurate; it is there for slow devices).
	Fallback bool
}

// RecommendedEntry is one device of recommendation.json (the 3a figures,
// measured on the reference laptop; for display only).
type RecommendedEntry struct {
	Kind             string   `json:"kind"`
	Model            string   `json:"model"`
	Precision        string   `json:"precision"`
	RTF              float64  `json:"rtf"`
	WER              float64  `json:"wer"`
	Words            bool     `json:"words"`
	FirstLoadS       float64  `json:"first_load_s"`
	CachedLoadS      *float64 `json:"cached_load_s"`
	ModelMB          float64  `json:"model_mb"`
	PeakWorkingSetMB float64  `json:"peak_working_set_mb"`
	Alternative      *struct {
		Model            string  `json:"model"`
		Precision        string  `json:"precision"`
		RTF              float64 `json:"rtf"`
		WER              float64 `json:"wer"`
		ModelMB          float64 `json:"model_mb"`
		PeakWorkingSetMB float64 `json:"peak_working_set_mb"`
	} `json:"alternative,omitempty"`
}

type recommendationFile struct {
	OpenVINO string                      `json:"openvino_version"`
	GenAI    string                      `json:"openvino_genai_version"`
	Rule     string                      `json:"rule"`
	Devices  map[string]RecommendedEntry `json:"devices"` // keyed by OpenVINO device name: GPU, NPU, CPU
}

var loadRecommendation = sync.OnceValue(func() recommendationFile {
	var f recommendationFile
	if err := json.Unmarshal(recommendationJSON, &f); err != nil {
		panic(fmt.Sprintf("devices: recommendation.json: %v", err)) // embedded: a broken file fails every test
	}
	return f
})

// RecommendedInfo returns the recommendation.json entry of an OpenVINO
// device id (ov:GPU, ov:NPU, ov:CPU).
func RecommendedInfo(deviceID string) (RecommendedEntry, bool) {
	name := model.OVDeviceName(deviceID)
	if name == "" {
		return RecommendedEntry{}, false
	}
	e, ok := loadRecommendation().Devices[name]
	return e, ok
}

// Recommended is the model and precision a newly created row of the device
// gets (I6): recommendation.json for OpenVINO devices (the rule's pick:
// large-v3-turbo fp16 on all three; lead ruling 14 keeps fp16 on the Intel
// GPU), the product default large-v3 / auto for CUDA (SPEC §2, §17.2: the
// default of the settings before 3b). ok is false for anything else; the
// caller then uses Fallback (I6: small + int8).
func Recommended(deviceID string) (Candidate, bool) {
	switch model.DeviceKind(deviceID) {
	case model.DeviceKindCUDA:
		return Candidate{Model: "large-v3", Precision: model.PrecisionAuto}, true
	case model.DeviceKindApple: // SPEC-v3 V5 (no measurements yet: the I5 pick of the Intel GPU)
		return Candidate{Model: "large-v3-turbo", Precision: model.PrecisionFP16}, true
	}
	if e, ok := RecommendedInfo(deviceID); ok && e.Model != "" && model.ValidPrecision(model.BackendOpenVINO, e.Precision) {
		return Candidate{Model: e.Model, Precision: e.Precision}, true
	}
	return Candidate{}, false
}

// Fallback is I6's "没有推荐数据时取 small + int8" in the backend's
// precision names: small / int8 for OpenVINO, small / auto for
// faster-whisper and Colab (auto resolves to int8 on the CPU, SPEC §17.2),
// small / fp16 for MLX (the unquantised conversion).
func Fallback(backend string) Candidate {
	switch backend {
	case model.BackendFasterWhisper, model.BackendColab:
		return Candidate{Model: "small", Precision: model.PrecisionAuto}
	case model.BackendMLX:
		return Candidate{Model: "small", Precision: model.PrecisionFP16}
	}
	return Candidate{Model: "small", Precision: model.PrecisionINT8}
}

// Candidates is the list the self-test tries on a device, in order (I9).
// The NPU never tries int4 (lead ruling 3: turbo int4 did not compile within
// 15 minutes in 3a). The first entry is Recommended(id) where there is one.
func Candidates(deviceID string) []Candidate {
	switch deviceID {
	case model.DeviceOVGPU:
		return []Candidate{{Model: "large-v3-turbo", Precision: model.PrecisionFP16}, {Model: "large-v3-turbo", Precision: model.PrecisionINT8},
			{Model: "small", Precision: model.PrecisionINT8, Fallback: true}}
	case model.DeviceOVNPU:
		return []Candidate{{Model: "large-v3-turbo", Precision: model.PrecisionFP16}, {Model: "large-v3-turbo", Precision: model.PrecisionINT8},
			{Model: "small", Precision: model.PrecisionFP16, Fallback: true}}
	case model.DeviceOVCPU:
		return []Candidate{{Model: "large-v3-turbo", Precision: model.PrecisionINT8}, {Model: "small", Precision: model.PrecisionINT8, Fallback: true}}
	case model.DeviceFWCPU:
		return []Candidate{{Model: "small", Precision: model.PrecisionAuto}, {Model: "base", Precision: model.PrecisionAuto, Fallback: true}}
	case model.DeviceMLXGPU: // SPEC-v3 V5
		return []Candidate{{Model: "large-v3-turbo", Precision: model.PrecisionFP16}, {Model: "small", Precision: model.PrecisionFP16, Fallback: true}}
	}
	if model.DeviceKind(deviceID) == model.DeviceKindCUDA {
		return []Candidate{{Model: "large-v3", Precision: model.PrecisionAuto}, {Model: "large-v3-turbo", Precision: model.PrecisionAuto}}
	}
	return nil
}

// ModelChoices are the models the settings page offers for a backend
// (DeviceOptions.models). For OpenVINO only these can be prepared (each is
// converted from HFID(model)); faster-whisper accepts any non-empty name as
// before 3b, the list is a suggestion.
func ModelChoices(backend string) []string {
	switch backend {
	case model.BackendOpenVINO, model.BackendMLX:
		return []string{"large-v3-turbo", "large-v3", "medium", "small", "base"}
	case model.BackendFasterWhisper, model.BackendColab:
		return []string{"large-v3", "large-v2", "large-v3-turbo", "medium", "small", "base"}
	}
	return nil
}

// PrecisionChoices are the precisions of a backend (model.ValidPrecision).
func PrecisionChoices(backend string) []string {
	switch backend {
	case model.BackendOpenVINO, model.BackendMLX:
		return []string{model.PrecisionFP16, model.PrecisionINT8, model.PrecisionINT4}
	case model.BackendFasterWhisper, model.BackendColab:
		return []string{model.PrecisionAuto, model.PrecisionFloat16, model.PrecisionInt8Float16}
	}
	return nil
}

// ValidOVModel reports whether an OpenVINO device may use the model (one of
// ModelChoices(openvino), compared exactly).
func ValidOVModel(mdl string) bool {
	for _, m := range ModelChoices(model.BackendOpenVINO) {
		if m == mdl {
			return true
		}
	}
	return false
}

// ovModelMB are the sizes of the converted OpenVINO models on disk (3a,
// docs/benchmarks/intel-openvino.md §3, "体积 MB").
var ovModelMB = map[string]int{
	"base-fp16": 146, "base-int8": 79, "base-int4": 59,
	"small-fp16": 469, "small-int8": 243, "small-int4": 153,
	"medium-fp16": 1466, "medium-int8": 746, "medium-int4": 424,
	"large-v3-turbo-fp16": 1551, "large-v3-turbo-int8": 784, "large-v3-turbo-int4": 450,
	"large-v3-fp16": 2954, "large-v3-int8": 1489, "large-v3-int4": 818,
}

// ModelSizeMB is the size of a converted OpenVINO model measured in 3a; ok
// is false for combinations 3a did not convert. MemEstimateMB uses twice
// this value when nothing was observed yet (I8).
func ModelSizeMB(mdl, precision string) (int, bool) {
	v, ok := ovModelMB[strings.TrimSpace(mdl)+"-"+strings.TrimSpace(precision)]
	return v, ok
}
