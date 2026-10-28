// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sysprobe

import (
	"context"
	"io/fs"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Machine is the machine's identity (the remote.MachineEventKeys values).
type Machine struct {
	Source      string // "nvidia-smi" | "system_profiler" | ""
	GPUName     string
	GPUArch     string
	ComputeCap  string
	Driver      string
	CUDA        string
	GPUCount    int // -1 unknown
	VRAMTotalMB int
	VCPUs       int
	MemTotalMB  int
	// Metal: an Apple GPU (unified memory).
	Metal bool
	OS    string // "Linux 6.8.0", "macOS 15.1"
	CPU   string
}

// Usage is one reading (the remote.SampleEventKeys values; -1 unknown).
type Usage struct {
	CPUPct         int
	MemTotalMB     int
	MemAvailableMB int
	WhisperMB      int
	DataDiskMB     int
	GPUUtilPct     int
	VRAMUsedMB     int
	VRAMWhisperMB  int
	GPUTempC       int
	GPUPowerW      float64
}

// Sampler reads the machine; safe for one goroutine at a time.
type Sampler struct {
	mu      sync.Mutex
	smi     string
	smiOnce sync.Once
	cpu     cpuState
	machine *Machine
}

// NewSampler returns a sampler.
func NewSampler() *Sampler { return &Sampler{} }

func (s *Sampler) nvidia() string {
	s.smiOnce.Do(func() { s.smi = NvidiaSMI() })
	return s.smi
}

// Machine reads the identity once and caches it.
func (s *Sampler) Machine(ctx context.Context) Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.machine != nil {
		return *s.machine
	}
	m := Machine{GPUCount: -1, VRAMTotalMB: -1, VCPUs: runtime.NumCPU(), MemTotalMB: -1}
	if total, _, ok := memInfo(ctx); ok {
		m.MemTotalMB = total
	}
	m.OS, m.CPU = osInfo(ctx)
	if smi := s.nvidia(); smi != "" {
		if out, err := run(ctx, smi, MachineArgs...); err == nil {
			if g, err := ParseMachine(out); err == nil {
				m.Source, m.GPUName, m.Driver, m.ComputeCap = "nvidia-smi", g.Name, g.Driver, g.ComputeCap
				if g.MemTotalMB != nil {
					m.VRAMTotalMB = *g.MemTotalMB
				}
				if g.Count != nil {
					m.GPUCount = *g.Count
				}
			}
		}
	}
	if m.Source == "" {
		if name, ok := appleGPU(ctx); ok {
			m.Source, m.GPUName, m.Metal, m.GPUCount = "system_profiler", name, true, 1
		}
	}
	s.machine = &m
	return m
}

// Sample reads the current usage: CPU, memory, the worker processes
// (pids and their children), the job files under dataDir, the GPU.
func (s *Sampler) Sample(ctx context.Context, pids []int, dataDir string) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := Usage{CPUPct: -1, MemTotalMB: -1, MemAvailableMB: -1, WhisperMB: -1, DataDiskMB: -1, GPUUtilPct: -1,
		VRAMUsedMB: -1, VRAMWhisperMB: -1, GPUTempC: -1, GPUPowerW: -1}
	u.CPUPct = s.cpu.percent(ctx)
	if total, avail, ok := memInfo(ctx); ok {
		u.MemTotalMB, u.MemAvailableMB = total, avail
	}
	if len(pids) > 0 {
		u.WhisperMB = treeRSSMB(ctx, pids)
	} else {
		u.WhisperMB = 0
	}
	if dataDir != "" {
		u.DataDiskMB = dirMB(dataDir)
	}
	if smi := s.nvidia(); smi != "" {
		if out, err := run(ctx, smi, QueryArgs...); err == nil {
			if g, err := ParseQuery(out); err == nil {
				u.GPUUtilPct, u.VRAMUsedMB, u.GPUTempC = orNeg(g.UtilPct), orNeg(g.MemUsedMB), orNeg(g.TempC)
				if g.PowerW != nil {
					u.GPUPowerW = math.Round(*g.PowerW*10) / 10
				}
			}
		}
	} else if util, ok := appleGPUUtil(ctx); ok {
		u.GPUUtilPct = util
	}
	return u
}

func orNeg(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// dirMB is the size of the files below dir in MiB (symbolic links are not
// followed).
func dirMB(dir string) int {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return int(n >> 20)
}

// procTable is pid → (ppid, rss KiB) of every process.
type procEntry struct {
	ppid  int
	rssKB int64
}

// treeRSSMB sums the resident memory of pids and their descendants (MiB).
func treeRSSMB(ctx context.Context, pids []int) int {
	tab, ok := processTable(ctx)
	if !ok {
		return -1
	}
	children := map[int][]int{}
	for pid, e := range tab {
		children[e.ppid] = append(children[e.ppid], pid)
	}
	seen := map[int]bool{}
	var total int64
	var walk func(int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		if e, ok := tab[pid]; ok {
			total += e.rssKB
		}
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for _, p := range pids {
		walk(p)
	}
	return int(total >> 10)
}

// parsePSTable parses "pid ppid rss" lines (ps -A -o pid=,ppid=,rss=).
func parsePSTable(out string) map[int]procEntry {
	tab := map[int]procEntry{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.ParseInt(f[2], 10, 64)
		if e1 == nil && e2 == nil && e3 == nil {
			tab[pid] = procEntry{ppid: ppid, rssKB: rss}
		}
	}
	return tab
}
