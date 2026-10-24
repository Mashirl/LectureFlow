// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package hwdetect detects the hardware relevant to transcription before any
// component is installed (SPEC-v3 V3.4) and recommends the components to
// install. Detection only uses what the operating system ships with
// (nvidia-smi from the NVIDIA driver, PowerShell/CIM, sysctl, pmset and
// system calls); it never depends on the Python components or on
// internal/devices, which need them.
//
// Every probe has its own timeout and a failing probe only leaves its own
// fields empty: Detect never returns an error for a probe that failed.
// All parsing is done by pure functions over the command output so that the
// Windows and macOS parsers can be unit-tested on any platform.
package hwdetect

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"lectureflow/internal/model"
)

// HW is the detected hardware (.claude/v3-brief.md §5).
type HW struct {
	OS   string `json:"os"`   // runtime.GOOS: "windows", "darwin", ...
	Arch string `json:"arch"` // runtime.GOARCH
	CPU  string `json:"cpu"`  // CPU name; "" = unknown

	RAMMB      int `json:"ram_mb"`       // total physical memory; 0 = unknown
	DiskFreeMB int `json:"disk_free_mb"` // free space of the data directory's volume; 0 = unknown

	NVIDIA []GPU `json:"nvidia"` // NVIDIA GPUs (Windows only)

	IntelGPU     bool   `json:"intel_gpu"`
	IntelNPU     bool   `json:"intel_npu"`
	IntelGPUName string `json:"intel_gpu_name"`
	IntelNPUName string `json:"intel_npu_name"`

	AppleSilicon bool   `json:"apple_silicon"`
	Chip         string `json:"chip"`           // e.g. "Apple M2 Pro"
	UnifiedMemMB int    `json:"unified_mem_mb"` // = RAMMB on Apple Silicon

	HasBattery bool `json:"has_battery"`
}

// GPU is one NVIDIA GPU.
type GPU struct {
	Name   string `json:"name"`
	VRAMMB int    `json:"vram_mb"` // 0 = unknown (the system device list could not tell)
	Driver string `json:"driver"`  // NVIDIA driver version, e.g. "592.00"; "" = unknown
	// Source is where the GPU was found: "nvidia-smi" or "wmi" (the
	// system device list, used when nvidia-smi is missing or fails).
	Source string `json:"source"`
}

// Recommendation is the component set suggested for the hardware.
type Recommendation struct {
	Components []string          `json:"components"`  // component ids, python and models included
	Reasons    []model.ErrorInfo `json:"reasons"`     // code + params, translated by the UI
	Notes      []model.ErrorInfo `json:"notes"`       // e.g. slow CPU transcription
	DownloadMB int               `json:"download_mb"` // 0 = computed from the component manifest (Plan)
}

// Runner runs a command and returns its standard output. Tests inject fakes.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options are the injectable parts of DetectWith.
type Options struct {
	OS, Arch string
	Run      Runner
	// DiskFreeMB returns the free space of the volume holding dir (an
	// existing directory). nil = unknown.
	DiskFreeMB func(dir string) (int, error)
	// TotalRAMMB is the fallback when the command-based probe gives no
	// memory size. nil = no fallback.
	TotalRAMMB func() (int, error)
	// Timeout of one probe command; 0 = defaultTimeout.
	Timeout time.Duration
}

const defaultTimeout = 5 * time.Second

// powershellTimeoutFactor: PowerShell's first start (CIM, .NET JIT) can
// take several seconds on a cold machine, so its probe gets 4x the budget
// (20 s by default).
const powershellTimeoutFactor = 4

// Detect probes the hardware with the platform's own tools. The error is
// always nil (a failed probe leaves its fields empty); it is kept for the
// frozen signature and future use.
func Detect(ctx context.Context, dataDir string) (HW, error) {
	return DetectWith(ctx, dataDir, platformOptions()), nil
}

// DetectWith is Detect with injectable commands and system calls.
func DetectWith(ctx context.Context, dataDir string, o Options) HW {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	hw := HW{OS: o.OS, Arch: o.Arch}
	switch o.OS {
	case "windows":
		detectWindows(ctx, &hw, o)
	case "darwin":
		detectDarwin(ctx, &hw, o)
	}
	if hw.RAMMB == 0 && o.TotalRAMMB != nil {
		if mb, err := o.TotalRAMMB(); err == nil && mb > 0 {
			hw.RAMMB = mb
		}
	}
	if hw.AppleSilicon {
		hw.UnifiedMemMB = hw.RAMMB
	}
	if hw.NVIDIA == nil {
		hw.NVIDIA = []GPU{} // a JSON array for the API, never null
	}
	if o.DiskFreeMB != nil && dataDir != "" {
		if dir := existingAncestor(dataDir); dir != "" {
			if mb, err := o.DiskFreeMB(dir); err == nil && mb > 0 {
				hw.DiskFreeMB = mb
			}
		}
	}
	return hw
}

// run runs one probe command with its own timeout.
func (o Options) run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if o.Run == nil {
		return nil, os.ErrNotExist
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return o.Run(cctx, name, args...)
}

// existingAncestor returns dir or its nearest existing ancestor (the data
// directory may not exist yet before onboarding creates it).
func existingAncestor(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		d = dir
	}
	for {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

const bytesPerMB = 1024 * 1024
