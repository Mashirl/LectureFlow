// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"strconv"

	"lectureflow/internal/model"
)

// mac-opti M6: the resource bar on macOS. The upper area reads, without
// cgo and without administrator rights (devices.ReadMacCPUTicks,
// devices.ReadMacMemory: the same measure the scheduler's memory admission
// uses, internal/devices/macmem.go):
//
//   - CPU: host_statistics(HOST_CPU_LOAD_INFO), the ticks of all cores; the
//     busy share between two samples (devices.MacCPUPct: Activity Monitor's
//     "CPU 负载");
//   - memory: hw.memsize (= hwdetect's RAMMB / UnifiedMemMB) and Activity
//     Monitor's "已使用内存" (app memory + wired + compressed). On Apple
//     silicon this is the unified memory the GPU uses too:
//     System.UnifiedMemory, the front end names it "统一内存";
//   - temperature: not readable without private interfaces or root
//     (powermetrics): System.Unsupported lists it, the front end leaves the
//     tile out instead of showing "--".
//
// The card of mlx:gpu (the Apple GPU) reads `ioreg -r -d 1 -w 0 -c
// IOAccelerator` each sample while the GPU is detected: "Device
// Utilization %" (Activity Monitor's GPU history) and "In use system
// memory" (the unified memory the GPU's allocations hold; part of the
// upper area's "in use"; the card shows it against the physical memory).
// system_darwin.go wires the readers; this file is OS-independent, so it
// is tested everywhere.

// platformMarks is what a new Service takes as its platform (the
// platform's platformReadings: system_windows.go, system_darwin.go,
// system_other.go); the package's tests pin it so their expectations hold
// on every host.
var platformMarks = platformReadings

// Readings of the upper area (System.Unsupported).
const (
	ReadingCPU     = "cpu_pct"
	ReadingMem     = "mem"
	ReadingCPUTemp = "cpu_temp"
)

// IORegArgs are the arguments of the ioreg command of the Apple GPU card:
// the IOAccelerator services with their properties, one level, no line
// wrapping.
var IORegArgs = []string{"-r", "-d", "1", "-w", "0", "-c", "IOAccelerator"}

// ioregExe is ioreg (/usr/sbin, always in the PATH of macOS).
const ioregExe = "ioreg"

var (
	ioregUtilRe  = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)
	ioregInUseRe = regexp.MustCompile(`"In use system memory"\s*=\s*(\d+)`)
)

// ParseIOAccelerator reads the ioreg output of IORegArgs: the GPU's
// utilization in percent (capped at 100) and the system memory it uses in
// MB, of the first accelerator that reports them; nil when absent.
func ParseIOAccelerator(out string) (utilPct, inUseMB *int) {
	if m := ioregUtilRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil {
			v = min(v, 100)
			utilPct = &v
		}
	}
	if m := ioregInUseRe.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			mb := int(math.Round(float64(v) / (1 << 20)))
			inUseMB = &mb
		}
	}
	return utilPct, inUseMB
}

// sampleApple reads the Apple GPU's utilization and memory while the
// detection found it (mlx:gpu). sampleMu held; s.mu is taken only to store
// the result.
func (s *Service) sampleApple(ctx context.Context) {
	if s.d.Detection == nil || s.d.Detection().MLX == "" {
		s.mu.Lock()
		s.appleUtil, s.appleMem, s.appleHist = nil, nil, nil
		s.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	out, err := s.run(cctx, ioregExe, IORegArgs...)
	cancel()
	if ctx.Err() != nil {
		return
	}
	var util, mem *int
	if err != nil {
		if msg := err.Error(); msg != s.appleErr {
			slog.Info("resources: ioreg unavailable, the Apple GPU card shows --", "err", msg)
			s.appleErr = msg
		}
	} else {
		s.appleErr = ""
		util, mem = ParseIOAccelerator(out)
	}
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appleUtil, s.appleMem = util, mem
	if util != nil {
		s.appleHist = append(s.appleHist, histPoint{at: now, util: cloneInt(util)})
	}
	s.appleHist = pruneHistory(s.appleHist, now)
}

// appleCard fills the Apple GPU card's utilization, history and memory
// (the GPU's share of the unified memory, against the physical memory).
// s.mu held.
func (s *Service) appleCard(c *Device) {
	if c.ID != model.DeviceMLXGPU || !c.Present {
		return
	}
	if s.appleUtil != nil {
		c.UtilPct = cloneInt(s.appleUtil)
	}
	for _, h := range pruneHistory(s.appleHist, s.d.Now()) {
		if h.util != nil {
			c.UtilHistory = append(c.UtilHistory, *h.util)
		}
	}
	if s.appleMem != nil {
		c.MemUsedMB, c.MemTotalMB = cloneInt(s.appleMem), cloneInt(s.memTotalMB)
	}
}
