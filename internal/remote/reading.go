// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// Checking and converting the machine / sample events (sample.go): one
// implementation for every remote (Colab's colab_remote.py today, the SSH
// agent next). The caller has parsed the line into flat values; Read*
// checks each field against the tables and patterns of sample.go and turns
// the reading into the card's Machine and Usage (colab-obs-core,
// colab-telemetry.md §3.3).

import "math"

// MaxTempC bounds gpu_temp_c (°C; -1 = unknown).
const MaxTempC = 200

// MachineReading is a "machine" event as sent ("" and -1 = unknown).
type MachineReading struct {
	GPUName, GPUArch, ComputeCap, Driver, CUDA, Storage string
	GPUCount, VRAMTotalMB, VCPUs, MemTotalMB            int64
}

// SampleReading is a "sample" event as sent (-1 = unknown).
type SampleReading struct {
	CPUPct         float64
	MemTotalMB     int64
	MemAvailableMB int64
	// RAMDiskMB: the memory disk in use minus the job files (SegRAMDisk;
	// -1 without a memory disk). DataMB: the job files on the memory disk
	// (SegData; 0 on the disk). DataDiskMB: the job files on the disk (-1
	// in memory mode).
	RAMDiskMB, DataMB, DataDiskMB int64
	WhisperMB                     int64 // the worker and its children, RssAnon
	GPUUtilPct                    float64
	VRAMUsedMB                    int64
	VRAMReservedMB                int64
	VRAMWhisperMB                 int64
	GPUTempC                      float64
	GPUPowerW                     float64
}

// Getter returns an event's field: a string, a float64 (every JSON
// number) or anything else (a bool: always wrong here); ok false = absent.
type Getter func(key string) (v any, ok bool)

// ReadMachine checks a machine event's fields (MachineEventKeys, besides
// type and ts); bad is the first field that is missing or out of its
// pattern or range ("" = all fine).
func ReadMachine(get Getter) (m MachineReading, bad string) {
	strs := []struct {
		key string
		p   *string
		ok  func(string) bool
	}{
		{"gpu_name", &m.GPUName, GPUNamePattern.MatchString},
		{"gpu_arch", &m.GPUArch, GPUArchPattern.MatchString},
		{"compute_cap", &m.ComputeCap, VersionPattern.MatchString},
		{"driver", &m.Driver, DriverPattern.MatchString},
		{"cuda", &m.CUDA, VersionPattern.MatchString},
		{"storage", &m.Storage, StorageValuePattern.MatchString},
	}
	for _, f := range strs {
		v, ok := get(f.key)
		s, isStr := v.(string)
		if !ok || !isStr || !f.ok(s) {
			return m, f.key
		}
		*f.p = s
	}
	ints := []struct {
		key string
		p   *int64
		max int64
	}{
		{"gpu_count", &m.GPUCount, MaxGPUs}, {"vram_total_mb", &m.VRAMTotalMB, MaxMB},
		{"vcpus", &m.VCPUs, MaxVCPUs}, {"mem_total_mb", &m.MemTotalMB, MaxMB},
	}
	for _, f := range ints {
		n, ok := intField(get, f.key, f.max)
		if !ok {
			return m, f.key
		}
		*f.p = n
	}
	return m, ""
}

// ReadSample checks a sample event's fields (SampleEventKeys); bad as for
// ReadMachine.
func ReadSample(get Getter) (s SampleReading, bad string) {
	nums := []struct {
		key string
		p   *float64
		max float64
	}{
		{"cpu_pct", &s.CPUPct, MaxPct}, {"gpu_util_pct", &s.GPUUtilPct, MaxPct},
		{"gpu_temp_c", &s.GPUTempC, MaxTempC}, {"gpu_power_w", &s.GPUPowerW, MaxPowerW},
	}
	for _, f := range nums {
		v, ok := get(f.key)
		x, isNum := v.(float64)
		if !ok || !isNum || math.IsNaN(x) || x != -1 && (x < 0 || x > f.max) {
			return s, f.key
		}
		*f.p = x
	}
	ints := []struct {
		key string
		p   *int64
	}{
		{"mem_total_mb", &s.MemTotalMB}, {"mem_available_mb", &s.MemAvailableMB}, {"ramdisk_mb", &s.RAMDiskMB},
		{"data_mb", &s.DataMB}, {"data_disk_mb", &s.DataDiskMB}, {"whisper_mb", &s.WhisperMB},
		{"vram_used_mb", &s.VRAMUsedMB}, {"vram_reserved_mb", &s.VRAMReservedMB}, {"vram_whisper_mb", &s.VRAMWhisperMB},
	}
	for _, f := range ints {
		n, ok := intField(get, f.key, MaxMB)
		if !ok {
			return s, f.key
		}
		*f.p = n
	}
	return s, ""
}

// intField is an integer field in [-1, max].
func intField(get Getter, key string, max int64) (int64, bool) {
	v, ok := get(key)
	x, isNum := v.(float64)
	if !ok || !isNum || x != math.Trunc(x) || x < -1 || x > float64(max) {
		return 0, false
	}
	return int64(x), true
}

func known(n int64) *int {
	if n < 0 {
		return nil
	}
	v := int(n)
	return &v
}

func knownPct(x float64) *int {
	if x < 0 {
		return nil
	}
	v := int(math.Round(x))
	return &v
}

// Machine is the reading as the card's Machine (source: SourceNvidiaSMI
// for a worker's own reading).
func (m MachineReading) Machine(source string) Machine {
	return Machine{Source: source, GPUName: m.GPUName, GPUArch: m.GPUArch, ComputeCap: m.ComputeCap, Driver: m.Driver,
		CUDA: m.CUDA, GPUCount: known(m.GPUCount), VRAMTotalMB: known(m.VRAMTotalMB), VCPUs: known(m.VCPUs),
		MemTotalMB: known(m.MemTotalMB), Storage: m.Storage}
}

// Usage is the reading as the card's Usage without history (Source
// SourceWorker). The memory bar's segments (colab-telemetry.md §3.3) add up
// to MemTotalMB: what is in use (total − available) is handed out in the
// order memory disk, job files, Whisper, each at most what is left, and
// the rest is SegSystem; SegFree is the available memory. Without a total
// or an available reading there is no bar (RAM empty).
func (s SampleReading) Usage() Usage {
	u := Usage{Source: SourceWorker, CPUPct: knownPct(s.CPUPct), RAM: []RAMSegment{},
		GPUUtilPct: knownPct(s.GPUUtilPct), VRAMUsedMB: known(s.VRAMUsedMB), VRAMReservedMB: known(s.VRAMReservedMB),
		VRAMWhisperMB: known(s.VRAMWhisperMB), GPUTempC: knownPct(s.GPUTempC)}
	if s.GPUPowerW >= 0 {
		w := math.Round(s.GPUPowerW*10) / 10
		u.GPUPowerW = &w
	}
	if s.DataDiskMB >= 0 {
		u.DataDiskMB = known(s.DataDiskMB)
	}
	if s.MemTotalMB > 0 && s.MemAvailableMB >= 0 {
		u.RAM = Segments(s.MemTotalMB, s.MemAvailableMB, s.RAMDiskMB, s.DataMB, s.WhisperMB)
	}
	return u
}

// Segments splits total memory (MB) into the bar's parts; ramdisk, data
// and whisper < 0 are unknown (a memory disk of -1 is no segment at all;
// unknown data or Whisper count as system).
func Segments(total, available, ramdisk, data, whisper int64) []RAMSegment {
	available = min(max(available, 0), total)
	left := total - available
	var out []RAMSegment
	take := func(kind string, mb int64, always bool) {
		if mb < 0 && !always {
			return
		}
		n := min(max(mb, 0), left)
		left -= n
		out = append(out, RAMSegment{Kind: kind, MB: int(n)})
	}
	take(SegRAMDisk, ramdisk, false)
	take(SegData, data, true)
	take(SegWhisper, whisper, true)
	out = append(out, RAMSegment{Kind: SegSystem, MB: int(left)}, RAMSegment{Kind: SegFree, MB: int(available)})
	return out
}

// UsedFree is the bar of a reading that has only the total and the free
// memory (Colab's resources endpoint): SegUsed and SegFree.
func UsedFree(totalMB, freeMB int64) []RAMSegment {
	freeMB = min(max(freeMB, 0), totalMB)
	return []RAMSegment{{Kind: SegUsed, MB: int(totalMB - freeMB)}, {Kind: SegFree, MB: int(freeMB)}}
}
