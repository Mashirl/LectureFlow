// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package resources is the resource watcher of SPEC-v1.1 A10: GPU sampling
// through nvidia-smi (with 60 s of history), provider balances (DeepSeek
// balance API, cached 5 min) and per-provider token usage.
//
// The JSON types below are FROZEN by the lead (GET /api/resources and SSE
// "event: resources" carry a Snapshot). Everything else: resources-builder.
package resources

import (
	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// Snapshot is the body of GET /api/resources and of SSE "event: resources".
type Snapshot struct {
	// GPU stays the CUDA card's nvidia-smi metrics (I10: "CUDA 设备沿用现有
	// 指标"); the cuda:0 card of Devices refers to it.
	GPU GPU `json:"gpu"`
	// Devices (SPEC-v2 I10, phase 3b freeze): one card per enabled device,
	// priority order. Omitted when there is none (the front end treats a
	// missing list as empty; fixtures from before 3b have none).
	Devices []Device `json:"devices,omitempty"`
	// Power is the power state of the last scan (I8, I10); omitted before
	// 3b and when not reported.
	Power *Power `json:"power,omitempty"`
	// System and Policy (power management, v2-step20 freeze,
	// types_power.go; filled by task P4): the upper area "系统与电源" (CPU,
	// memory, CPU temperature) and the power policy's state (why these
	// devices are active, suspended jobs, notices). Omitted when not wired,
	// so snapshots without them keep their bytes.
	System    *System    `json:"system,omitempty"`
	Policy    *Policy    `json:"policy,omitempty"`
	Providers []Provider `json:"providers"`
	SampledAt string     `json:"sampled_at"` // UTC RFC3339
	// Remote is the Colab backend's state (SPEC-v3 V14.5, v3-brief §8):
	// omitted (null for the front end) while Colab is not configured or
	// switched off, so snapshots without it keep their bytes; with no
	// session its State is "none" ("远端 GPU：未使用"). Never a credential.
	Remote *colab.Status `json:"remote,omitempty"`
	// Remotes (colab-obs-core, lead freeze 2026-10-27): every remote
	// device as a remote.Card (Colab today, SSH servers next; remotes.go
	// builds them). Colab's card carries its colab.Status in Card.Colab;
	// Remote above stays for the front ends that read it. Omitted when there
	// is none.
	Remotes []remote.Card `json:"remotes,omitempty"`
}

// Device is one transcription device card of the resource bar (SPEC-v2
// I10): name, backend and precision, current tasks, slot usage. FROZEN by
// the lead for phase 3b; filled by task E.
//
// Power management (v2-step20 freeze, task P4): Devices lists EVERY row
// (disabled ones too, priority order) so the bottom area can show the
// standby rows ("Intel NPU · OpenVINO · 待命", "已停用"); Active marks the
// cards of the middle area (Policy.Active). A front end of 3b showed the
// list as cards of the enabled devices; the new one splits by Active.
type Device struct {
	ID        string `json:"id"`      // model.Device*
	Label     string `json:"label"`   // the device's own name (not translated)
	Backend   string `json:"backend"` // faster-whisper | openvino
	Kind      string `json:"kind"`    // cuda | igpu | npu | cpu
	Model     string `json:"model"`
	Precision string `json:"precision"`
	SlotsUsed int    `json:"slots_used"` // pipeline jobs + a running self-test
	SlotsMax  int    `json:"slots_max"`  // max_parallel
	Present   bool   `json:"present"`    // seen by the last detection
	Paused    bool   `json:"paused"`     // the device's own switch (the global one is GPU.Paused)
	// Tasks are what runs on the device now (never null).
	Tasks []DeviceTask `json:"tasks"`
	// UtilPct and UtilHistory: CUDA from nvidia-smi (= GPU.UtilPct /
	// UtilHistory); Intel GPU / NPU from the Windows PDH counter "\GPU
	// Engine(*)\Utilization Percentage" matched by LUID (lead ruling 16).
	// null / empty when not readable: the card shows "--" and only slots
	// and tasks.
	UtilPct     *int  `json:"util_pct"`
	UtilHistory []int `json:"util_history"` // last 60 s, oldest first, one every 2 s; never null
	// BatteryMode: running on battery (Power.OnBattery): the card says
	// "电池模式" (I10).
	BatteryMode bool `json:"battery_mode"`
	// BatteryHold: on battery with battery awareness on, this device starts
	// no new job (CUDA; in auto mode also the Intel devices that are not
	// the battery choice).
	BatteryHold bool `json:"battery_hold"`

	// Power management (v2-step20 freeze; task P4):
	// Enabled is the row's switch ("已停用" on a standby row when false).
	Enabled bool `json:"enabled"`
	// Active: one of the policy's active devices (devices.PolicyView
	// Active), shown as a card in the middle area; false = a standby row.
	Active bool `json:"active"`
	// Untested: the device has never run a self-test (last_benchmark null):
	// the standby row's "未测试" tag.
	Untested bool `json:"untested"`
	// HoldsSuspended: a job waiting power_wait_ac continues on this device
	// (sched.PowerStatus.HoldsSuspended): "有任务挂起".
	HoldsSuspended bool `json:"holds_suspended"`
	// MemUsedMB / MemTotalMB: an Intel device's shared GPU memory in use and
	// available (PDH "\GPU Adapter Memory(*)\Shared Usage" by LUID, like
	// UtilPct; no administrator rights), null when not readable and for
	// CUDA (its card reads GPU.MemUsedMB) and CPU devices. The card shows
	// "--" for null.
	MemUsedMB  *int `json:"mem_used_mb"`
	MemTotalMB *int `json:"mem_total_mb"`
}

// DeviceTask is one task on a device card: a pipeline asr job or a
// self-test.
type DeviceTask struct {
	JobID        string  `json:"job_id,omitempty"` // "" for a self-test
	LectureID    string  `json:"lecture_id,omitempty"`
	LectureTitle string  `json:"lecture_title,omitempty"`
	CourseID     string  `json:"course_id,omitempty"`
	SelfTest     bool    `json:"selftest,omitempty"`
	Progress     float64 `json:"progress"` // 0..1
}

// Power is the power state shown in the resource bar (devices.Power plus
// the setting).
type Power struct {
	Known        bool `json:"known"`
	OnBattery    bool `json:"on_battery"`
	Saver        bool `json:"saver"`
	BatteryPct   *int `json:"battery_pct"`
	BatteryAware bool `json:"battery_aware"` // settings battery_aware (retired by power management; still sent)

	// Power management (v2-step20 freeze; task P4): devices.Power's
	// HasBattery and Charging, and Source — "ac" (外接电源) or "battery"
	// (电池), "" while the state is unknown. USB-C PD and a power adapter
	// cannot be told apart reliably on Windows without administrator
	// rights (power-plan §4), so no other value is ever sent.
	HasBattery bool   `json:"has_battery"`
	Charging   bool   `json:"charging"`
	Source     string `json:"source"`
}

// GPU is A10.1 "转写设备". Pointer fields are null when nvidia-smi is not
// available or did not report the value (the UI shows "--").
type GPU struct {
	Available bool   `json:"available"` // nvidia-smi answered
	Name      string `json:"name"`      // e.g. "NVIDIA GeForce RTX 5070 Ti Laptop GPU"
	// Backend is "CUDA · float16" (device cuda + the compute type ASR would
	// use now) or "CPU" (device cpu) — from the current ASR settings.
	Backend     string   `json:"backend"`
	UtilPct     *int     `json:"util_pct"`
	MemUsedMB   *int     `json:"mem_used_mb"`
	MemTotalMB  *int     `json:"mem_total_mb"`
	TempC       *int     `json:"temp_c"`
	PowerW      *float64 `json:"power_w"`
	PowerLimitW *float64 `json:"power_limit_w"`
	// UtilHistory and TempHistory are the samples of the last 60 s, oldest
	// first, one every 2 s (at most 30). Missing samples are omitted.
	UtilHistory []int `json:"util_history"`
	TempHistory []int `json:"temp_history"`
	// TempWarn is true at TempC >= 85 (A10.1 warning colour).
	TempWarn bool `json:"temp_warn"`
	// ASR slot usage ("ASR 1/1") and the GPU pause switch (moved here from
	// the top bar, A10.1).
	ASRSlotsUsed int    `json:"asr_slots_used"`
	ASRSlotsMax  int    `json:"asr_slots_max"`
	Paused       bool   `json:"paused"`
	Error        string `json:"error,omitempty"` // why nvidia-smi is unavailable
}

// Provider is one row of A10.2.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Balance is the total balance text as returned by the balance API
	// (e.g. "110.00"), null when balance_api = none or the query failed.
	Balance         *string `json:"balance"`
	BalanceCurrency *string `json:"balance_currency"`
	// BalanceError explains a failed query (hover text); "" otherwise.
	BalanceError string `json:"balance_error,omitempty"`
	// BalanceErrorInfo is the coded form of BalanceError (v2 P14.3,
	// balance.* codes; Detail = BalanceError, never containing the key).
	BalanceErrorInfo *model.ErrorInfo `json:"balance_error_info,omitempty"`
	BalanceAt        string           `json:"balance_at,omitempty"` // when the balance was fetched (UTC RFC3339)

	Today Tokens `json:"today"`
	Total Tokens `json:"total"`
	// CacheHitRate* = cache_hit_tokens / prompt_tokens, null without prompt tokens.
	CacheHitRateToday *float64 `json:"cache_hit_rate_today"`
	CacheHitRateTotal *float64 `json:"cache_hit_rate_total"`
	// CostToday is the estimated cost of today's usage, null without prices.
	// v2 P12.5: the sum of today's per-request estimates (usage_daily.
	// estimated_cost), each priced in the period its request started in.
	// D3: also null when every request of today was unpriced (tokens only).
	CostToday *float64 `json:"cost_today"`
	Currency  *string  `json:"currency"`
	// CostUnpricedRequests is the number of today's requests that had no
	// price (D3: neither the model's tariff price nor a provider-level
	// price), not included in CostToday: with CostToday set, "部分请求未计价".
	// Omitted at 0 (added at the phase-2 gate, lead authorised).
	CostUnpricedRequests int64 `json:"cost_unpriced_requests,omitempty"`

	Inflight int `json:"inflight"` // current concurrency "3 / 8"
	Max      int `json:"max"`

	// TariffState is the provider's peak state (v2 P12.4); omitted for a
	// provider without a tariff with peak windows.
	TariffState *TariffState `json:"tariff_state,omitempty"`
}

// TariffState is the current peak state of a provider with a tariff (v2
// P12.4), refreshed with every sample (SSE "resources"). Same fields, types
// and JSON as api.TariffState, so api.TariffState(*ts) converts it. Times
// are UTC RFC3339.
type TariffState struct {
	Peak            bool        `json:"peak"`
	NextChange      string      `json:"next_change,omitempty"` // "" when the state never changes
	Timezone        string      `json:"timezone"`              // the tariff's zone, e.g. Asia/Shanghai
	MissingHolidays bool        `json:"missing_holidays"`      // "节假日数据缺失"
	TodayPeak       [][2]string `json:"today_peak"`            // UTC intervals overlapping the server's local day; never null
	OffpeakOnly     bool        `json:"offpeak_only"`
	Multiplier      float64     `json:"offpeak_multiplier"`
}

// Tokens is a usage total (input and output shown separately, A10.2).
type Tokens struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"` // includes cache hits
	CacheHitTokens   int64 `json:"cache_hit_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}
