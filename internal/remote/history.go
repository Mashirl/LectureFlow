// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"slices"
	"sync"
	"time"
)

// Samples keeps what one remote reported about itself: its Machine and its
// latest Usage with HistoryLen points of history, one per HistoryStep, by
// the time this machine received each reading (remote clocks are not
// trusted). Readings that land in the same step: the later one wins, but a
// value it lacks (Colab's resources endpoint has no CPU) keeps the
// earlier one's. Safe for concurrent use; the zero value is ready.
type Samples struct {
	mu      sync.Mutex
	machine *Machine
	last    *Usage
	lastAt  time.Time
	from    map[string]time.Time // source → when its last reading arrived
	points  map[int64]point      // step index → the step's values
}

type point struct{ cpu, gpu, vram *int }

func step(t time.Time) int64 { return t.UnixNano() / int64(HistoryStep) }

// SetMachine records the remote's machine. The worker's own reading
// (nvidia-smi) is never replaced by one from Colab's resources endpoint.
func (r *Samples) SetMachine(m Machine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.machine != nil && r.machine.Source != SourceColabResources && m.Source == SourceColabResources {
		return
	}
	c := m
	r.machine = &c
}

// Machine is the recorded machine (nil: none yet).
func (r *Samples) Machine() *Machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.machine == nil {
		return nil
	}
	c := *r.machine
	return &c
}

// Add records a reading received at at (u.Source names its source; At,
// AgeSec and the histories of u are ignored).
func (r *Samples) Add(at time.Time, u Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := u
	c.RAM = slices.Clone(u.RAM)
	c.CPUHistory, c.GPUHistory, c.VRAMHistory = nil, nil, nil
	if r.last == nil || !at.Before(r.lastAt) { // a late older reading only gets its point
		r.last, r.lastAt = &c, at
	}
	if r.from == nil {
		r.from = map[string]time.Time{}
		r.points = map[int64]point{}
	}
	if at.After(r.from[u.Source]) {
		r.from[u.Source] = at
	}
	k := step(at)
	p := r.points[k]
	if u.CPUPct != nil {
		p.cpu = ptr(*u.CPUPct)
	}
	if u.GPUUtilPct != nil {
		p.gpu = ptr(*u.GPUUtilPct)
	}
	if u.VRAMUsedMB != nil {
		p.vram = ptr(*u.VRAMUsedMB)
	}
	r.points[k] = p
	newest := step(r.lastAt)
	for i := range r.points {
		if i <= newest-HistoryLen {
			delete(r.points, i)
		}
	}
}

func ptr(v int) *int { return &v }

// LastFrom is when the last reading of source arrived (zero: never).
func (r *Samples) LastFrom(source string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.from[source]
}

// Usage is the latest reading with its age and the histories of the
// HistoryLen steps ending at now (oldest first, nil where there was no
// reading); nil before the first reading.
func (r *Samples) Usage(now time.Time) *Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return nil
	}
	u := *r.last
	u.RAM = slices.Clone(r.last.RAM)
	u.At = r.lastAt.UTC()
	u.AgeSec = max(0, int(now.Sub(r.lastAt).Seconds()))
	u.CPUHistory = make([]*int, HistoryLen)
	u.GPUHistory = make([]*int, HistoryLen)
	u.VRAMHistory = make([]*int, HistoryLen)
	end := step(now)
	for i := 0; i < HistoryLen; i++ {
		p, ok := r.points[end-HistoryLen+1+int64(i)]
		if !ok {
			continue
		}
		u.CPUHistory[i], u.GPUHistory[i], u.VRAMHistory[i] = p.cpu, p.gpu, p.vram
	}
	return &u
}

// Reset forgets everything (the session was released).
func (r *Samples) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.machine, r.last, r.lastAt, r.from, r.points = nil, nil, time.Time{}, nil, nil
}
