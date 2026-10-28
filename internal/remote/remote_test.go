// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func getter(m map[string]any) Getter {
	return func(k string) (any, bool) {
		v, ok := m[k]
		if n, isInt := v.(int); isInt {
			return float64(n), ok
		}
		return v, ok
	}
}

func machineEvent() map[string]any {
	return map[string]any{"gpu_name": "Tesla T4", "gpu_arch": "Turing", "compute_cap": "7.5", "driver": "580.82.07",
		"cuda": "13.0", "gpu_count": 1, "vram_total_mb": 15360, "vcpus": 2, "mem_total_mb": 12975, "storage": "memory"}
}

func sampleEvent() map[string]any {
	return map[string]any{"cpu_pct": 37.5, "mem_total_mb": 12975, "mem_available_mb": 6100, "ramdisk_mb": 3400,
		"data_mb": 95, "data_disk_mb": -1, "whisper_mb": 3000, "gpu_util_pct": 88.0, "vram_used_mb": 4100,
		"vram_reserved_mb": 448, "vram_whisper_mb": 3650, "gpu_temp_c": 61.0, "gpu_power_w": 65.24}
}

func TestReadMachineAndSampleFields(t *testing.T) {
	m, bad := ReadMachine(getter(machineEvent()))
	if bad != "" || m.GPUName != "Tesla T4" || m.VRAMTotalMB != 15360 || m.Storage != "memory" {
		t.Fatalf("machine %+v bad %q", m, bad)
	}
	mc := m.Machine(SourceNvidiaSMI)
	if mc.Source != SourceNvidiaSMI || *mc.VCPUs != 2 || *mc.GPUCount != 1 || mc.CUDA != "13.0" {
		t.Errorf("Machine %+v", mc)
	}
	// Every field is required; unknowns are "" and -1.
	for _, k := range MachineEventKeys {
		ev := machineEvent()
		delete(ev, k)
		if _, bad := ReadMachine(getter(ev)); bad != k {
			t.Errorf("missing %s: bad %q", k, bad)
		}
	}
	unk := map[string]any{"gpu_name": "", "gpu_arch": "", "compute_cap": "", "driver": "", "cuda": "", "gpu_count": -1,
		"vram_total_mb": -1, "vcpus": -1, "mem_total_mb": -1, "storage": ""}
	if m, bad := ReadMachine(getter(unk)); bad != "" || m.Machine(SourceNvidiaSMI).VCPUs != nil {
		t.Errorf("unknowns: %+v %q", m, bad)
	}
	for k, v := range map[string]any{"gpu_name": "Tesla T4 <script>", "gpu_name ": "x", "driver": "580", "cuda": "13",
		"compute_cap": "7.5.1", "storage": "ramdisk", "gpu_arch": "Turing!", "gpu_count": 65, "vcpus": 1.5,
		"vram_total_mb": -2, "mem_total_mb": "12975"} {
		ev := machineEvent()
		ev[k] = v
		if _, bad := ReadMachine(getter(ev)); bad == "" && k != "gpu_name " {
			t.Errorf("%s = %v accepted", k, v)
		}
	}
	// A transcript-like name never passes.
	ev := machineEvent()
	ev["gpu_name"] = strings.Repeat("a", 65)
	if _, bad := ReadMachine(getter(ev)); bad != "gpu_name" {
		t.Errorf("long name: %q", bad)
	}

	s, bad := ReadSample(getter(sampleEvent()))
	if bad != "" || s.CPUPct != 37.5 || s.WhisperMB != 3000 || s.DataDiskMB != -1 {
		t.Fatalf("sample %+v bad %q", s, bad)
	}
	for _, k := range SampleEventKeys {
		ev := sampleEvent()
		delete(ev, k)
		if _, bad := ReadSample(getter(ev)); bad != k {
			t.Errorf("missing %s: bad %q", k, bad)
		}
	}
	for k, v := range map[string]any{"cpu_pct": 100.5, "gpu_util_pct": -0.5, "gpu_temp_c": 250.0, "gpu_power_w": 10001.0,
		"vram_used_mb": 1.5, "mem_total_mb": float64(MaxMB + 1), "whisper_mb": true, "data_mb": "95"} {
		ev := sampleEvent()
		ev[k] = v
		if _, bad := ReadSample(getter(ev)); bad != k {
			t.Errorf("%s = %v: bad %q", k, v, bad)
		}
	}
}

func TestSampleUsageSegments(t *testing.T) {
	s, _ := ReadSample(getter(sampleEvent()))
	u := s.Usage()
	want := []RAMSegment{{SegRAMDisk, 3400}, {SegData, 95}, {SegWhisper, 3000}, {SegSystem, 12975 - 6100 - 3400 - 95 - 3000}, {SegFree, 6100}}
	if !slices.Equal(u.RAM, want) {
		t.Errorf("segments %+v", u.RAM)
	}
	sum := 0
	for _, g := range u.RAM {
		sum += g.MB
	}
	if sum != 12975 || *u.CPUPct != 38 || *u.GPUPowerW != 65.2 || u.DataDiskMB != nil || u.Source != SourceWorker {
		t.Errorf("usage %+v (sum %d)", u, sum)
	}
	// Disk mode: no memory disk segment, the job files on the disk apart.
	ev := sampleEvent()
	ev["ramdisk_mb"], ev["data_mb"], ev["data_disk_mb"] = -1, 0, 120
	s, _ = ReadSample(getter(ev))
	u = s.Usage()
	if u.RAM[0].Kind != SegData || u.RAM[0].MB != 0 || u.DataDiskMB == nil || *u.DataDiskMB != 120 {
		t.Errorf("disk mode %+v", u)
	}
	// Readings that do not add up: the parts are capped, the sum stays the total.
	got := Segments(1000, 600, 300, 200, 500)
	if !slices.Equal(got, []RAMSegment{{SegRAMDisk, 300}, {SegData, 100}, {SegWhisper, 0}, {SegSystem, 0}, {SegFree, 600}}) {
		t.Errorf("capped %+v", got)
	}
	// No total: no bar.
	ev["mem_total_mb"] = -1
	s, _ = ReadSample(getter(ev))
	if u := s.Usage(); len(u.RAM) != 0 || u.RAM == nil {
		t.Errorf("no total: %+v", u.RAM)
	}
	if uf := UsedFree(13000, 12000); !slices.Equal(uf, []RAMSegment{{SegUsed, 1000}, {SegFree, 12000}}) {
		t.Errorf("used/free %+v", uf)
	}
}

func TestSamplesHistory(t *testing.T) {
	var r Samples
	t0 := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	if r.Usage(t0) != nil || r.Machine() != nil {
		t.Fatal("empty")
	}
	n := func(v int) *int { return &v }
	// 2 s steps; one step without a reading; two readings in one step.
	r.Add(t0, Usage{Source: SourceWorker, CPUPct: n(10), GPUUtilPct: n(80), VRAMUsedMB: n(4000)})
	r.Add(t0.Add(2*time.Second), Usage{Source: SourceWorker, CPUPct: n(20), GPUUtilPct: n(81), VRAMUsedMB: n(4001)})
	r.Add(t0.Add(6*time.Second), Usage{Source: SourceWorker, CPUPct: n(30), GPUUtilPct: n(82), VRAMUsedMB: n(4002)})
	r.Add(t0.Add(7*time.Second), Usage{Source: SourceColabResources, GPUUtilPct: n(90), VRAMUsedMB: n(4100)})
	now := t0.Add(8 * time.Second)
	u := r.Usage(now)
	if len(u.CPUHistory) != HistoryLen || len(u.GPUHistory) != HistoryLen || u.Source != SourceColabResources || u.AgeSec != 1 {
		t.Fatalf("usage %+v", u)
	}
	val := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}
	last := func(h []*int, k int) []any {
		var out []any
		for _, p := range h[len(h)-k:] {
			out = append(out, val(p))
		}
		return out
	}
	// steps t0, +2, +4 (none), +6 (worker then resources: CPU kept), +8 (now, none)
	if got := last(u.CPUHistory, 5); !slices.Equal(got, []any{10, 20, nil, 30, nil}) {
		t.Errorf("cpu %v", got)
	}
	if got := last(u.GPUHistory, 5); !slices.Equal(got, []any{80, 81, nil, 90, nil}) {
		t.Errorf("gpu %v", got)
	}
	if r.LastFrom(SourceWorker) != t0.Add(6*time.Second) || r.LastFrom(SourceColabResources) != t0.Add(7*time.Second) {
		t.Errorf("last from")
	}
	// A minute later the old points are out of the window.
	r.Add(t0.Add(70*time.Second), Usage{Source: SourceWorker, CPUPct: n(5)})
	u = r.Usage(t0.Add(70 * time.Second))
	cnt := 0
	for _, p := range u.CPUHistory {
		if p != nil {
			cnt++
		}
	}
	if cnt != 1 || *u.CPUHistory[HistoryLen-1] != 5 {
		t.Errorf("history after a minute: %d points", cnt)
	}
	b, _ := json.Marshal(u)
	if !strings.Contains(string(b), `"cpu_history":[null,`) {
		t.Errorf("JSON %s", b)
	}
	// The worker's machine is not replaced by the resources endpoint's.
	r.SetMachine(Machine{Source: SourceNvidiaSMI, GPUName: "Tesla T4"})
	r.SetMachine(Machine{Source: SourceColabResources, GPUName: "T4"})
	if m := r.Machine(); m.GPUName != "Tesla T4" {
		t.Errorf("machine %+v", m)
	}
	r.Reset()
	if r.Usage(now) != nil || r.Machine() != nil || !r.LastFrom(SourceWorker).IsZero() {
		t.Error("reset")
	}
}
