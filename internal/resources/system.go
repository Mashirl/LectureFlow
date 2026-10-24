// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"log/slog"
	"math"
)

// The upper area of the resource bar, "系统与电源" (power management,
// v2-step20 task P4; types_power.go): CPU utilisation, physical memory and
// the CPU temperature, sampled with the GPU. Windows reads them without
// administrator rights:
//
//   - CPU: the PDH rate counter CPUCounter ("% Processor Utility", what Task
//     Manager shows since Windows 8), or CPUCounterFallback ("% Processor
//     Time") when the first does not exist; null on the first collection;
//   - memory: GlobalMemoryStatusEx (ullTotalPhys − ullAvailPhys,
//     ullTotalPhys);
//   - temperature: the PDH counter ThermalCounter of every ACPI thermal
//     zone (Kelvin), the hottest one in °C; null when no zone reports a
//     plausible value (common on desktops and some laptops: "--").
//
// Everything in this file is pure or OS-independent; system_windows.go reads
// the counters and the memory status, system_other.go reports them unknown.

// PDH counter paths of the readings (English names, PdhAddEnglishCounterW).
const (
	CPUCounter         = `\Processor Information(_Total)\% Processor Utility`
	CPUCounterFallback = `\Processor Information(_Total)\% Processor Time`
	ThermalCounter     = `\Thermal Zone Information(*)\Temperature`
	// AdapterMemCounter is the shared GPU memory in use per adapter
	// instance ("luid_0x…_0x…_phys_0"), in bytes: Device.MemUsedMB of the
	// Intel GPU / NPU cards, matched by LUID like EngineCounter.
	AdapterMemCounter = `\GPU Adapter Memory(*)\Shared Usage`
)

// Plausible CPU temperatures in °C: a thermal zone reporting a value
// outside them (0 K, a sensor that is not connected) is ignored.
const (
	minTempC = 1
	maxTempC = 150
)

// CPUPctOf is System.CPUPct for a formatted counter value: nil when unknown
// (the rate counter is being primed, the counter is missing, NaN or
// negative), else the value rounded and capped at 100 ("% Processor
// Utility" exceeds 100 when the cores run above their base frequency).
func CPUPctOf(v *float64) *int {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return nil
	}
	p := int(math.Round(*v))
	if p > 100 {
		p = 100
	}
	return &p
}

// KelvinToC converts a thermal zone's Kelvin reading to whole °C; ok is
// false outside the plausible range.
func KelvinToC(k float64) (int, bool) {
	if math.IsNaN(k) || math.IsInf(k, 0) {
		return 0, false
	}
	c := int(math.Round(k - 273.15))
	if c < minTempC || c > maxTempC {
		return 0, false
	}
	return c, true
}

// MaxTempC is System.CPUTempC: the hottest plausible zone of a reading of
// ThermalCounter (Kelvin per instance), nil when there is none.
func MaxTempC(zones []EngineSample) *int {
	var best *int
	for _, z := range zones {
		c, ok := KelvinToC(z.Value)
		if !ok {
			continue
		}
		if best == nil || c > *best {
			v := c
			best = &v
		}
	}
	return best
}

// MemOf is System.MemUsedMB / MemTotalMB from GlobalMemoryStatusEx's
// ullTotalPhys and ullAvailPhys (bytes); both nil for a zero total.
func MemOf(totalPhys, availPhys uint64) (used, total *int) {
	if totalPhys == 0 {
		return nil, nil
	}
	if availPhys > totalPhys {
		availPhys = totalPhys
	}
	t := int(totalPhys >> 20)
	u := int((totalPhys - availPhys) >> 20)
	return &u, &t
}

// SharedTotalMB is Device.MemTotalMB of an Intel GPU / NPU: the shared
// system memory WDDM lets an adapter use, half of the physical memory (the
// "共享 GPU 内存" total Task Manager shows; DXGI reports the same value as
// SharedSystemMemory but does not enumerate the NPU). nil when the physical
// memory is unknown.
func SharedTotalMB(memTotalMB *int) *int {
	if memTotalMB == nil || *memTotalMB <= 0 {
		return nil
	}
	v := *memTotalMB / 2
	return &v
}

// SharedByLUID is the shared GPU memory in use per adapter (LUIDKey → MB,
// rounded) from a reading of AdapterMemCounter (bytes per instance; the
// instances of one adapter are summed). Instances without a LUID and
// negative or NaN values are ignored.
func SharedByLUID(samples []EngineSample) map[string]int {
	sums := map[string]float64{}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value < 0 {
			continue
		}
		luid, _, ok := ParseEngineInstance(s.Instance)
		if !ok {
			continue
		}
		sums[luid] += s.Value
	}
	out := make(map[string]int, len(sums))
	for l, v := range sums {
		out[l] = int(math.Round(v / (1 << 20)))
	}
	return out
}

// sysReading is one collection of the PDH counters of the upper area.
type sysReading struct {
	// CPU is the formatted CPU counter, nil while it is primed (the first
	// collection) or when it could not be added.
	CPU *float64
	// Zones are the ThermalCounter instances (Kelvin); nil when the counter
	// set does not exist.
	Zones []EngineSample
}

// systemReader reads the PDH counters of the upper area (system_windows.go;
// elsewhere opening fails). Read is called with sampleMu held.
type systemReader interface {
	Read() (sysReading, error)
	Close()
}

// memStatusFunc reads GlobalMemoryStatusEx: ullTotalPhys and ullAvailPhys
// in bytes (replaced in tests).
type memStatusFunc func() (totalPhys, availPhys uint64, err error)

// systemEnabled reports whether the snapshot carries System: an injected
// SystemStats, or the device cards are wired (the cmd wiring since 3b;
// before it, and in the tests of the other areas, snapshots keep their
// bytes).
func (s *Service) systemEnabled() bool {
	return s.d.SystemStats != nil || s.devicesEnabled()
}

// sampleSystem takes the upper area's readings. sampleMu held; s.mu is
// taken only to store the result.
func (s *Service) sampleSystem() {
	if !s.systemEnabled() {
		return
	}
	var v System
	if s.d.SystemStats != nil {
		v = s.d.SystemStats()
	} else {
		v = s.readSystem()
	}
	s.mu.Lock()
	s.sys = v
	if v.MemTotalMB != nil {
		t := *v.MemTotalMB
		s.memTotalMB = &t
	}
	s.mu.Unlock()
}

// readSystem reads memory and the PDH counters; a failing PDH query is
// retried after engineRetry (logged once per distinct problem), the values
// it gives are null meanwhile.
func (s *Service) readSystem() System {
	var v System
	if total, avail, err := s.memStatus(); err == nil {
		v.MemUsedMB, v.MemTotalMB = MemOf(total, avail)
	} else if msg := err.Error(); msg != s.memErr {
		slog.Info("resources: memory status unavailable, the resource bar shows --", "err", msg)
		s.memErr = msg
	}
	now := s.d.Now()
	if s.sysR == nil {
		if now.Before(s.sysRetryAt) {
			return v
		}
		r, err := s.newSystem()
		if err != nil {
			s.sysRetryAt = now.Add(engineRetry)
			if msg := err.Error(); msg != s.sysErr {
				slog.Info("resources: CPU counters unavailable, the resource bar shows --", "err", msg)
				s.sysErr = msg
			}
			return v
		}
		s.sysR, s.sysErr = r, ""
	}
	rd, err := s.sysR.Read()
	if err != nil {
		if msg := err.Error(); msg != s.sysErr {
			slog.Info("resources: reading CPU counters failed", "err", msg)
			s.sysErr = msg
		}
		s.sysR.Close()
		s.sysR, s.sysRetryAt = nil, now.Add(engineRetry)
		return v
	}
	v.CPUPct, v.CPUTempC = CPUPctOf(rd.CPU), MaxTempC(rd.Zones)
	return v
}

// closeSystemLocked releases the PDH query (Stop). sampleMu held.
func (s *Service) closeSystemLocked() {
	if s.sysR != nil {
		s.sysR.Close()
		s.sysR = nil
	}
}

// systemOf is the snapshot's System (a copy of the last sample). s.mu held.
func (s *Service) systemOf() *System {
	v := System{CPUPct: cloneInt(s.sys.CPUPct), MemUsedMB: cloneInt(s.sys.MemUsedMB),
		MemTotalMB: cloneInt(s.sys.MemTotalMB), CPUTempC: cloneInt(s.sys.CPUTempC)}
	return &v
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
