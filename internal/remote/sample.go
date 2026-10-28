// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import "regexp"

// The two events a remote worker sends about its machine
// (colab-telemetry.md §3.6): "machine" once per attach, then "sample"
// every 2 s. Flat scalars only; -1 is unknown (a field is never left
// out); strings come from nvidia-smi and the like, never from audio, and
// must match the patterns below. Colab's colab_remote.py and the SSH
// agent send exactly these fields; a malformed line is a contract
// violation like any other event.
const (
	EventMachine = "machine"
	EventSample  = "sample"
)

// MachineEventKeys and SampleEventKeys are the events' fields besides
// "type" and "ts" (the tables colab_remote.py's EVENT_KEYS mirrors).
var (
	MachineEventKeys = []string{"gpu_name", "gpu_arch", "compute_cap", "driver", "cuda", "gpu_count", "vram_total_mb", "vcpus", "mem_total_mb", "storage"}
	SampleEventKeys  = []string{"cpu_pct", "mem_total_mb", "mem_available_mb", "ramdisk_mb", "data_mb", "data_disk_mb", "whisper_mb", "gpu_util_pct", "vram_used_mb", "vram_reserved_mb", "vram_whisper_mb", "gpu_temp_c", "gpu_power_w"}
)

// String fields ("" = unknown).
var (
	GPUNamePattern      = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9 ._()/+-]{0,63})?$`)
	GPUArchPattern      = regexp.MustCompile(`^[A-Za-z0-9 ]{0,32}$`)
	DriverPattern       = regexp.MustCompile(`^([0-9]{1,4}(\.[0-9]{1,4}){1,3})?$`)
	VersionPattern      = regexp.MustCompile(`^([0-9]{1,2}\.[0-9]{1,2})?$`) // cuda, compute_cap
	StorageValuePattern = regexp.MustCompile(`^(memory|disk|disk_fallback)?$`)
)

// Number ranges (inclusive; -1 = unknown is always allowed).
const (
	MaxPct    = 100
	MaxMB     = 1 << 40
	MaxPowerW = 10000
	MaxGPUs   = 64
	MaxVCPUs  = 4096
)
