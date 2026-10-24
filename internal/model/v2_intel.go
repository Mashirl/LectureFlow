// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import "strings"

// ---------------------------------------------------------------------------
// SPEC-v2 part 3, I6-I10: transcription devices (Intel NPU / iGPU through
// OpenVINO next to faster-whisper). FROZEN by the lead for phase 3b
// (.claude/v3b-brief.md, NOTES.md "v3b 冻结（core-builder）"); changes only
// through the lead.
// ---------------------------------------------------------------------------

// ASRDevice is one row of asr_device (migrations/008_asr_devices.sql, I6).
type ASRDevice struct {
	ID          string `json:"id"`      // DeviceCUDA0 | DeviceOVGPU | DeviceOVNPU | DeviceOVCPU | DeviceFWCPU
	Backend     string `json:"backend"` // BackendFasterWhisper | BackendOpenVINO
	Label       string `json:"label"`   // the device's own name, refreshed by every detection (never translated)
	Enabled     bool   `json:"enabled"`
	Model       string `json:"model"`     // e.g. large-v3, large-v3-turbo
	Precision   string `json:"precision"` // ValidPrecision(Backend, ·)
	MaxParallel int    `json:"max_parallel"`
	Priority    int    `json:"priority"` // auto mode tries higher values first
	// DetectedAt is when a detection last saw the device (UTC RFC3339); nil
	// = never seen (e.g. cuda:0 migrated while nvidia-smi was not answering).
	DetectedAt *string `json:"detected_at"`
	// LastBenchmark is the raw JSON of the last self-test (DeviceBenchmark,
	// I9); nil = never tested. Written only by Store.SetASRDeviceBenchmark.
	LastBenchmark *string `json:"last_benchmark"`
}

// ASR backends (asr_device.backend CHECK).
const (
	BackendFasterWhisper = "faster-whisper" // worker/asr.py with config python (.venv)
	BackendOpenVINO      = "openvino"       // worker/asr_openvino.py with config python_openvino (.venv-ov)
)

// Device ids. A device id names a kind of device, not an enumeration slot:
// ov:GPU is the Intel GPU (the worker resolves it by vendor 0x8086, since
// OpenVINO may list an NVIDIA card as GPU.1; lead ruling 10: several Intel
// GPUs are out of scope).
const (
	DeviceCUDA0 = "cuda:0" // faster-whisper on the first NVIDIA GPU
	DeviceOVGPU = "ov:GPU" // OpenVINO on the Intel GPU (Xe iGPU)
	DeviceOVNPU = "ov:NPU" // OpenVINO on the Intel NPU (AI Boost)
	DeviceOVCPU = "ov:CPU" // OpenVINO on the CPU (disabled by default, last resort)
	// DeviceFWCPU is faster-whisper on the CPU: created only by the one-time
	// migration of legacy settings with device = cpu.
	DeviceFWCPU = "fw:cpu"
)

// ASRDeviceModeAuto is ASRSettings.DeviceMode "自动" (the default): every
// enabled device may be chosen, in priority order (I8). Any other value is
// the id of the one device to use.
const ASRDeviceModeAuto = "auto"

// Device kinds (DeviceKind), used for the recommendation data, the
// candidate lists of the self-test, the battery rule and the UI.
const (
	DeviceKindCUDA = "cuda"
	DeviceKindIGPU = "igpu"
	DeviceKindNPU  = "npu"
	DeviceKindCPU  = "cpu"
)

// Precisions. faster-whisper keeps the v1.1 compute types (auto is resolved
// per run, SPEC §17.2); OpenVINO uses the names of the converted model
// directories (whisper-{model}-{precision}, the 3a benchmark naming).
const (
	PrecisionAuto        = "auto"
	PrecisionFloat16     = "float16"
	PrecisionInt8Float16 = "int8_float16"

	PrecisionFP16 = "fp16"
	PrecisionINT8 = "int8"
	PrecisionINT4 = "int4"
)

// DeviceBackend returns the backend of a device id: faster-whisper for
// cuda:* and fw:*, openvino for ov:*, mlx for mlx:* and colab for colab:*
// (SPEC-v3 V3.1, V14), "" otherwise.
func DeviceBackend(id string) string {
	switch {
	case strings.HasPrefix(id, "cuda:"), strings.HasPrefix(id, "fw:"):
		return BackendFasterWhisper
	case strings.HasPrefix(id, "ov:"):
		return BackendOpenVINO
	case strings.HasPrefix(id, "mlx:"):
		return BackendMLX
	case strings.HasPrefix(id, "colab:"):
		return BackendColab
	}
	return ""
}

// DeviceKind returns the kind of a device id ("" when unknown).
func DeviceKind(id string) string {
	switch {
	case strings.HasPrefix(id, "cuda:"):
		return DeviceKindCUDA
	case id == DeviceOVGPU:
		return DeviceKindIGPU
	case id == DeviceOVNPU:
		return DeviceKindNPU
	case id == DeviceOVCPU, id == DeviceFWCPU:
		return DeviceKindCPU
	case id == DeviceMLXGPU:
		return DeviceKindApple // SPEC-v3 V5
	case id == DeviceColab:
		return DeviceKindRemote // SPEC-v3 V14
	}
	return ""
}

// OVDeviceName is the device field of the OpenVINO worker's job.json:
// "GPU", "NPU" or "CPU" for ov:GPU, ov:NPU, ov:CPU; "" for other ids.
func OVDeviceName(id string) string {
	switch id {
	case DeviceOVGPU, DeviceOVNPU, DeviceOVCPU:
		return strings.TrimPrefix(id, "ov:")
	}
	return ""
}

// OVDeviceID is the device id of an OpenVINO device kind (igpu, npu, cpu);
// "" for other kinds.
func OVDeviceID(kind string) string {
	switch kind {
	case DeviceKindIGPU:
		return DeviceOVGPU
	case DeviceKindNPU:
		return DeviceOVNPU
	case DeviceKindCPU:
		return DeviceOVCPU
	}
	return ""
}

// FWDevice is the device field of the faster-whisper worker's job.json:
// "cuda" for cuda:*, "cpu" for fw:cpu; "" for other ids.
func FWDevice(id string) string {
	switch {
	case strings.HasPrefix(id, "cuda:"):
		return "cuda"
	case id == DeviceFWCPU:
		return "cpu"
	}
	return ""
}

// DefaultDevicePriority is the priority a newly created row gets: CUDA >
// Apple GPU (MLX) > Intel GPU > NPU > CPU (I8 default order; mlx:gpu never
// shares a machine with the others), fw:cpu above Colab above ov:CPU (the
// remote backend is a supplement, local devices first; SPEC-v3 V14.1).
func DefaultDevicePriority(id string) int {
	switch DeviceKind(id) {
	case DeviceKindCUDA:
		return 300
	case DeviceKindApple:
		return 250
	case DeviceKindIGPU:
		return 200
	case DeviceKindNPU:
		return 100
	case DeviceKindRemote:
		return 5
	}
	if id == DeviceFWCPU {
		return 10
	}
	return 0
}

// ValidPrecision reports whether p is a precision of backend:
// faster-whisper and colab (faster-whisper on the remote GPU)
// auto|float16|int8_float16, openvino and mlx fp16|int8|int4 (mlx: the
// converted model directory's quantisation, worker/asr_mlx.py).
func ValidPrecision(backend, p string) bool {
	switch backend {
	case BackendFasterWhisper, BackendColab:
		return p == PrecisionAuto || p == PrecisionFloat16 || p == PrecisionInt8Float16
	case BackendOpenVINO, BackendMLX:
		return p == PrecisionFP16 || p == PrecisionINT8 || p == PrecisionINT4
	}
	return false
}

// ASRDeviceSnapshot is the asr snapshot of a run on device d (I6:
// {device_id, backend, model, precision, language}); auto marks a snapshot
// written in auto mode, whose device the scheduler binds again every time
// the job starts (lead ruling 1).
func ASRDeviceSnapshot(d ASRDevice, language string, auto bool) ASRSnapshot {
	return ASRSnapshot{DeviceID: d.ID, Backend: d.Backend, Model: d.Model, Precision: d.Precision,
		Language: language, Auto: auto}
}

// ASRDevicePatch is a change of one device row (PATCH /api/asr/devices/{id});
// nil = unchanged. Label, priority (Reorder), detected_at and last_benchmark
// are not patched.
type ASRDevicePatch struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	Model       *string `json:"model,omitempty"`
	Precision   *string `json:"precision,omitempty"`
	MaxParallel *int    `json:"max_parallel,omitempty"`
}

// ---------- self-test results (I9), asr_device.last_benchmark ----------

// Benchmark statuses.
const (
	BenchmarkDone      = "done"
	BenchmarkFailed    = "failed"
	BenchmarkCancelled = "cancelled"
)

// Pick rules (I9, the I5 rule).
const (
	PickRuleBest       = "wer_rtf_le_0_5" // lowest WER among RTF <= 0.5 (lowest RTF breaks a tie, or the earlier candidate without a reference)
	PickRuleLowestRTF  = "lowest_rtf"     // no candidate reached RTF <= 0.5
	PickMaxRTF         = 0.5
	BenchmarkVersion   = 1
	BenchmarkSourceLec = "lecture" // the clip comes from a repaired lecture
	// BenchmarkSourceBundled: the bundled public-domain sample lecture
	// (I9, SPEC-v3 V4.3: config SampleDir, devices.BundledClip), used whole
	// when no lecture gives a clip: start_ms 0, end_ms its duration, no
	// lecture_id.
	BenchmarkSourceBundled = "bundled"
)

// DeviceBenchmark is the JSON of asr_device.last_benchmark.
type DeviceBenchmark struct {
	Version    int               `json:"version"`     // BenchmarkVersion
	Status     string            `json:"status"`      // BenchmarkDone | BenchmarkFailed | BenchmarkCancelled
	StartedAt  string            `json:"started_at"`  // UTC RFC3339
	FinishedAt string            `json:"finished_at"` // UTC RFC3339
	Audio      BenchmarkAudio    `json:"audio"`
	Results    []BenchmarkResult `json:"results"` // in the order tried; never null
	// Pick is the recommended combination (I9); nil when no candidate ran.
	Pick *BenchmarkPick `json:"pick,omitempty"`
	// AppliedAt is set when the user applied Pick (or another result) to
	// the device row ("应用").
	AppliedAt *string    `json:"applied_at,omitempty"`
	Error     *ErrorInfo `json:"error,omitempty"` // why the test failed as a whole (e.g. selftest.no_audio)
}

// BenchmarkAudio is the clip a self-test transcribed.
type BenchmarkAudio struct {
	Source    string `json:"source"`               // BenchmarkSourceLec | BenchmarkSourceBundled
	LectureID string `json:"lecture_id,omitempty"` // source lecture
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
	// ReferenceWords is the number of words of the reference text (the
	// repaired sentence.text inside the clip); 0 = no reference, no WER.
	ReferenceWords int `json:"reference_words"`
}

// BenchmarkResult is one candidate's run.
type BenchmarkResult struct {
	Model       string   `json:"model"`
	Precision   string   `json:"precision"`
	OK          bool     `json:"ok"`
	LoadS       float64  `json:"load_s"`       // model load (+ compile) seconds
	TranscribeS float64  `json:"transcribe_s"` // transcription seconds
	RTF         float64  `json:"rtf"`          // TranscribeS / clip seconds
	WER         *float64 `json:"wer"`          // null without a reference or when the run failed
	// FirstCompile: the run compiled the model for this device (no cache
	// yet); its LoadS is the one-time cost.
	FirstCompile bool `json:"first_compile"`
	// Prepared: the run downloaded and/or converted the model first.
	Prepared  bool       `json:"prepared"`
	PeakMemMB int        `json:"peak_mem_mb"` // worker peak working set (0 = not reported)
	Words     bool       `json:"words"`       // word timestamps came back
	Error     *ErrorInfo `json:"error,omitempty"`
}

// BenchmarkPick is the combination the self-test recommends.
type BenchmarkPick struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
	Rule      string `json:"rule"` // PickRuleBest | PickRuleLowestRTF
}

// ---------- scheduling (I8) ----------

// waiting_for values added by I8 (SPEC §9.5). Kept from v1: WaitGPUPaused
// ("gpu_paused", the global switch: every device paused), WaitGPUSlot
// ("gpu_slot": every device the job may use is full) and WaitVRAM ("vram":
// CUDA memory).
const (
	// WaitOnBattery: on battery with battery awareness on, and the job may
	// only use devices that wait on battery (CUDA). [spec]
	// Superseded by WaitPowerWaitAC (power management, v2_power.go): the
	// scheduler stops producing it with task P1; the front end keeps its
	// text for old data.
	WaitOnBattery = "on_battery"
	// WaitDevicePausedPrefix: device_paused:{id}, the job's devices are
	// paused one by one (the one device of its snapshot, or every
	// candidate in auto mode: the highest-priority one is named).
	WaitDevicePausedPrefix = "device_paused:"
	// WaitMemory: an Intel device's estimate does not fit the available
	// system memory minus mem_reserve_mb.
	WaitMemory = "memory"
	// WaitNoASRDevice: no device the job may use exists, is enabled and is
	// present (also: the fixed device of its snapshot is missing or
	// disabled, or the first detection has not finished).
	WaitNoASRDevice = "no_asr_device"
	// WaitDeviceSelfTestPrefix: device_selftest:{id}, the device the job
	// would use runs a self-test that has not yielded yet.
	WaitDeviceSelfTestPrefix = "device_selftest:"
)

func WaitDevicePaused(id string) string   { return WaitDevicePausedPrefix + id }
func WaitDeviceSelfTest(id string) string { return WaitDeviceSelfTestPrefix + id }

// Transient asr statuses of JobStats.Status (the worker's STATUS lines):
// shown on the stage card until the first PROGRESS line.
const (
	ASRStatusDownloadingModel = "downloading_model" // faster-whisper downloads its model (v1)
	// ASRStatusPreparingModel: the OpenVINO worker downloads and converts
	// the model ("首次使用，正在准备模型", I7).
	ASRStatusPreparingModel = "preparing_model"
	// ASRStatusCompilingModel: first use of the model on this GPU / NPU,
	// compiling into the cache (NPU about 4 minutes, once; lead ruling 3).
	ASRStatusCompilingModel = "compiling_model"
)

// ASRTiming is job.stats.asr_timing of an asr run (the worker's timing
// block): the hover card and the I12 RTF.
type ASRTiming struct {
	LoadMS       int64 `json:"load_ms"`       // model load (+ compile)
	TranscribeMS int64 `json:"transcribe_ms"` // transcription only
	AudioMS      int64 `json:"audio_ms"`      // audio duration
	FirstCompile bool  `json:"first_compile,omitempty"`
	PeakMemMB    int   `json:"peak_mem_mb,omitempty"` // worker peak working set
}

// KV keys of phase 3b.
const (
	// KVASRDevicePausedPrefix: asr_device_paused:{id} = "1" while the
	// device's own pause switch is on (I8; the global switch stays
	// KVGPUPaused).
	KVASRDevicePausedPrefix = "asr_device_paused:"
	// KVASRDevicesMigrated = "1" once the legacy ASR settings were turned
	// into device rows (cuda:0 or fw:cpu), one time.
	KVASRDevicesMigrated = "v2.asr_devices_migrated"
	// KVASRDevicePromptDismissedPrefix: asr_device_prompt_dismissed:{id} =
	// "1" after the user closed the workbench prompt of a new device (I9).
	KVASRDevicePromptDismissedPrefix = "asr_device_prompt_dismissed:"
	// KVMemObservedPrefix: mem_observed:{device}:{model}:{precision} = the
	// largest observed worker peak in MB (Intel devices' memory admission,
	// like vram_observed for CUDA).
	KVMemObservedPrefix = "mem_observed:"
)

// MemObservedKey is the kv key of an observed memory peak.
func MemObservedKey(deviceID, mdl, precision string) string {
	return KVMemObservedPrefix + deviceID + ":" + mdl + ":" + precision
}
