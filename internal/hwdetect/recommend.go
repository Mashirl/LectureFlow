// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"math"
	"strings"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// Component ids (.claude/v3-brief.md §4).
const (
	ComponentPython      = "python"
	ComponentASRCUDA     = "asr-cuda"
	ComponentASROpenVINO = "asr-openvino"
	ComponentASRMLX      = "asr-mlx"
)

// Model backends of the model component ids.
const (
	ModelBackendFasterWhisper = "faster-whisper"
	ModelBackendOpenVINO      = "openvino"
	ModelBackendMLX           = "mlx"
)

// PrecisionMLXFP16 is the precision name used for MLX models (the
// mlx-community Whisper conversions are float16). model.ValidPrecision does
// not know the mlx backend yet (step 7).
const PrecisionMLXFP16 = "fp16"

// ModelID is the component id of a Whisper model:
// "model:{backend}:{name}:{precision}".
func ModelID(backend, name, precision string) string {
	return "model:" + backend + ":" + name + ":" + precision
}

// VRAM tiers of the NVIDIA rule (V3.4). Cards are sold in GB but report
// slightly less in MiB (a "6 GB" card may report 6 0xx MiB or a little
// under 6144), so the tiers are decimal thousands of MiB: a 6 GB card
// always reaches 6000, a 4 GB card 4000.
const (
	vramLargeMB = 6000 // >= : large-v3
	vramTurboMB = 4000 // >= : large-v3-turbo; below: medium
)

// Unified memory tiers of the Apple Silicon rule.
const (
	unifiedTurboMB = 8 * 1024 // >= : large-v3-turbo; below: small
)

// lowRAMMB: below this total memory a note suggests closing other programs.
const lowRAMMB = 8 * 1000

// Reason and note codes (translated by web/i18n/_parts/v3-hw.*.json).
const (
	ReasonNVIDIA            = "hw.reason.nvidia"              // {name, vram_gb}
	ReasonNVIDIAVRAMUnknown = "hw.reason.nvidia_vram_unknown" // {name}
	ReasonNVIDIASmallVRAM   = "hw.reason.nvidia_small_vram"   // {vram_gb, model}
	ReasonNPUBattery        = "hw.reason.npu_battery"         // {name}
	ReasonIntelGPU          = "hw.reason.intel_gpu"           // {name}
	ReasonIntelNPU          = "hw.reason.intel_npu"           // {name}
	ReasonApple             = "hw.reason.apple"               // {chip}
	ReasonAppleSmallMem     = "hw.reason.apple_small_mem"     // {mem_gb, model}
	ReasonCPUOnly           = "hw.reason.cpu_only"            // {cpu}

	NoteCPUSlow     = "hw.note.cpu_slow"
	NoteVRAMUnknown = "hw.note.vram_unknown"
	NoteLowRAM      = "hw.note.low_ram"     // {ram_gb}
	NoteUnsupported = "hw.note.unsupported" // {os, arch}
)

// Recommend returns the components to install for hw (V3.4). It is a pure
// function; testdata/recommend.golden.json covers every rule and boundary.
//
//   - NVIDIA (Windows): asr-cuda + large-v3 (VRAM >= 6 GB, precision by
//     config.ResolveComputeType: float16 from 10 GB, else int8_float16);
//     4-6 GB large-v3-turbo; < 4 GB medium; unknown VRAM large-v3-turbo.
//     With an Intel NPU as well: asr-openvino + large-v3-turbo fp16 for
//     battery mode.
//   - Intel iGPU / NPU only (Windows): asr-openvino + large-v3-turbo fp16
//     (SPEC-v2 I5 recommendation, lead ruling 14: fp16 on iGPU and NPU).
//   - Apple Silicon: asr-mlx + large-v3-turbo (>= 8 GB unified memory),
//     else small.
//   - None of these (Windows): asr-openvino on the CPU + small int8, with
//     the "slow, use overnight processing" note.
//   - Other platforms (Intel Mac, Linux): python only, with a note.
//
// python is always the first component.
func Recommend(hw HW) Recommendation {
	r := Recommendation{Components: []string{ComponentPython}, Reasons: []model.ErrorInfo{}, Notes: []model.ErrorInfo{}}
	add := func(ids ...string) { r.Components = append(r.Components, ids...) }
	reason := func(code string, params map[string]any) {
		r.Reasons = append(r.Reasons, model.ErrorInfo{Code: code, Params: params})
	}
	note := func(code string, params map[string]any) {
		r.Notes = append(r.Notes, model.ErrorInfo{Code: code, Params: params})
	}
	ovTurbo := ModelID(ModelBackendOpenVINO, "large-v3-turbo", model.PrecisionFP16)

	switch {
	case hw.OS == "windows" && len(hw.NVIDIA) > 0:
		g := bestGPU(hw.NVIDIA)
		var name, precision string
		switch {
		case g.VRAMMB <= 0:
			name, precision = "large-v3-turbo", model.PrecisionInt8Float16
			reason(ReasonNVIDIAVRAMUnknown, map[string]any{"name": g.Name})
			note(NoteVRAMUnknown, nil)
		case g.VRAMMB >= vramLargeMB:
			name, precision = "large-v3", config.ResolveComputeType("auto", "cuda", g.VRAMMB)
			reason(ReasonNVIDIA, map[string]any{"name": g.Name, "vram_gb": gb(g.VRAMMB)})
		default:
			name = "medium"
			if g.VRAMMB >= vramTurboMB {
				name = "large-v3-turbo"
			}
			precision = config.ResolveComputeType("auto", "cuda", g.VRAMMB)
			reason(ReasonNVIDIA, map[string]any{"name": g.Name, "vram_gb": gb(g.VRAMMB)})
			reason(ReasonNVIDIASmallVRAM, map[string]any{"vram_gb": gb(g.VRAMMB), "model": name})
		}
		add(ComponentASRCUDA, ModelID(ModelBackendFasterWhisper, name, precision))
		if hw.IntelNPU {
			add(ComponentASROpenVINO, ovTurbo)
			reason(ReasonNPUBattery, map[string]any{"name": orDefault(hw.IntelNPUName, "Intel NPU")})
		}

	case hw.OS == "windows" && (hw.IntelGPU || hw.IntelNPU):
		add(ComponentASROpenVINO, ovTurbo)
		if hw.IntelGPU {
			reason(ReasonIntelGPU, map[string]any{"name": orDefault(hw.IntelGPUName, "Intel GPU")})
		}
		if hw.IntelNPU {
			reason(ReasonIntelNPU, map[string]any{"name": orDefault(hw.IntelNPUName, "Intel NPU")})
		}

	case hw.OS == "darwin" && hw.AppleSilicon:
		mem := hw.UnifiedMemMB
		if mem <= 0 {
			mem = hw.RAMMB
		}
		name := "large-v3-turbo"
		// Unknown memory (0) keeps the turbo model: every Apple Silicon Mac
		// has at least 8 GB.
		if mem > 0 && mem < unifiedTurboMB {
			name = "small"
		}
		add(ComponentASRMLX, ModelID(ModelBackendMLX, name, PrecisionMLXFP16))
		reason(ReasonApple, map[string]any{"chip": orDefault(hw.Chip, "Apple Silicon")})
		if name == "small" {
			reason(ReasonAppleSmallMem, map[string]any{"mem_gb": gb(mem), "model": name})
		}

	case hw.OS == "windows":
		add(ComponentASROpenVINO, ModelID(ModelBackendOpenVINO, "small", model.PrecisionINT8))
		reason(ReasonCPUOnly, map[string]any{"cpu": orDefault(hw.CPU, "CPU")})
		note(NoteCPUSlow, nil)

	default:
		note(NoteUnsupported, map[string]any{"os": hw.OS, "arch": hw.Arch})
	}

	if hw.RAMMB > 0 && hw.RAMMB < lowRAMMB && (hw.OS == "windows" || hw.OS == "darwin") {
		note(NoteLowRAM, map[string]any{"ram_gb": gb(hw.RAMMB)})
	}
	return r
}

// bestGPU is the NVIDIA GPU with the most known VRAM (the first one on ties).
func bestGPU(gpus []GPU) GPU {
	best := gpus[0]
	for _, g := range gpus[1:] {
		if g.VRAMMB > best.VRAMMB {
			best = g
		}
	}
	return best
}

// gb rounds MiB to whole GiB for display (12227 MiB -> 12).
func gb(mb int) int { return int(math.Round(float64(mb) / 1024)) }

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
