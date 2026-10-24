// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// The device cards of the resource bar (SPEC-v2 I10, phase 3b task E): one
// card per enabled device in priority order, with its slots, tasks, pause
// switch, the battery state and, for the Intel GPU / NPU, the utilisation
// from the PDH counters (lead ruling 16; "--" when not readable). Power
// management (task P4): one entry per row, the policy's flags, the Intel
// devices' shared memory.

// engineRetry is how long the watcher waits before opening the PDH query
// again after it failed.
const engineRetry = time.Minute

// devicesEnabled reports whether the device cards are wired (any of the four
// Deps functions, or PowerStatus since power management; all nil = before
// 3b, no cards and no power).
func (s *Service) devicesEnabled() bool {
	return s.d.ASRDevices != nil || s.d.DeviceStatuses != nil || s.d.Detection != nil || s.d.Power != nil || s.d.PowerStatus != nil
}

// engineLUIDs returns the PDH keys of the OpenVINO devices that have one
// (ov:GPU, ov:NPU) from the last detection.
func engineLUIDs(det devices.Detection) map[string]string {
	out := map[string]string{}
	if det.OV == nil {
		return out
	}
	for _, d := range det.OV.Devices {
		id := model.OVDeviceID(d.Kind)
		if id != model.DeviceOVGPU && id != model.DeviceOVNPU {
			continue
		}
		if key, ok := LUIDKey(d.LUID); ok {
			out[id] = key
		}
	}
	return out
}

// sampleEngines reads the PDH counter when an Intel GPU / NPU with a LUID
// was detected. sampleMu held; s.mu is taken only to store the result.
func (s *Service) sampleEngines() {
	if s.d.Detection == nil {
		return
	}
	luids := engineLUIDs(s.d.Detection())
	now := s.d.Now()
	if len(luids) == 0 {
		s.mu.Lock()
		s.engUtil, s.engMem = nil, nil
		s.mu.Unlock()
		return
	}
	if s.engines == nil {
		if now.Before(s.engRetryAt) {
			return
		}
		r, err := s.newEngines()
		if err != nil {
			s.engRetryAt = now.Add(engineRetry)
			if msg := err.Error(); msg != s.engErr {
				slog.Info("resources: GPU engine counters unavailable, Intel device cards show --", "err", msg)
				s.engErr = msg
			}
			s.mu.Lock()
			s.engUtil, s.engMem = nil, nil
			s.mu.Unlock()
			return
		}
		s.engines, s.engErr = r, ""
	}
	ss, err := s.engines.Read()
	if err != nil {
		if msg := err.Error(); msg != s.engErr {
			slog.Info("resources: reading GPU engine counters failed", "err", msg)
			s.engErr = msg
		}
		s.engines.Close()
		s.engines, s.engRetryAt = nil, now.Add(engineRetry)
		s.mu.Lock()
		s.engUtil, s.engMem = nil, nil
		s.mu.Unlock()
		return
	}
	cur := map[string]*int{}
	if ss != nil { // nil: the rate counter is being primed, values unknown
		util := UtilByLUID(ss)
		for _, key := range luids {
			v := util[key] // an adapter no process uses is idle: 0
			cur[key] = &v
		}
	}
	// The shared memory of the same collection (an instantaneous counter:
	// known also while the rate counter is primed); an adapter without an
	// instance uses none.
	var mem map[string]*int
	if m, ok := s.engines.(adapterMemReader); ok {
		if sh := m.SharedUsage(); sh != nil {
			used := SharedByLUID(sh)
			mem = map[string]*int{}
			for _, key := range luids {
				v := used[key]
				mem[key] = &v
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engUtil, s.engMem = cur, mem
	for key, v := range cur {
		s.engHist[key] = append(s.engHist[key], histPoint{at: now, util: v})
	}
	for key, h := range s.engHist {
		if h = pruneHistory(h, now); len(h) == 0 {
			delete(s.engHist, key)
		} else {
			s.engHist[key] = h
		}
	}
}

// closeEngines releases the PDH queries (Stop): the device cards' and the
// upper area's.
func (s *Service) closeEngines() {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.engines != nil {
		s.engines.Close()
		s.engines = nil
	}
	s.closeSystemLocked()
}

// deviceInput is what build reads before taking s.mu.
type deviceInput struct {
	rows     []model.ASRDevice
	statuses map[string]sched.DeviceStatus
	det      devices.Detection
	hasDet   bool
	power    devices.Power
	hasPower bool
	// policy is the scheduler's power status (Deps.PowerStatus): the
	// Active / HoldsSuspended flags and Snapshot.Policy.
	policy    sched.PowerStatus
	hasPolicy bool
	titles    map[string]model.Lecture // lecture id → lecture (running tasks)
}

// readDevices collects the device inputs (outside s.mu: the store is read
// for the titles of running lectures).
func (s *Service) readDevices(ctx context.Context) deviceInput {
	in := deviceInput{statuses: map[string]sched.DeviceStatus{}, titles: map[string]model.Lecture{}}
	if s.d.ASRDevices != nil {
		for _, d := range s.d.ASRDevices() {
			if model.DeviceBackend(d.ID) == model.BackendColab {
				continue // SPEC-v3 V14.5: Colab has its own area (Snapshot.Remote)
			}
			in.rows = append(in.rows, d)
		}
	}
	if s.d.DeviceStatuses != nil {
		for _, st := range s.d.DeviceStatuses() {
			in.statuses[st.ID] = st
			for _, r := range st.Running {
				if _, ok := in.titles[r.LectureID]; ok || r.LectureID == "" || s.d.Store == nil {
					continue
				}
				l, err := s.d.Store.GetLecture(ctx, r.LectureID)
				if err != nil {
					l = model.Lecture{ID: r.LectureID}
				}
				in.titles[r.LectureID] = l
			}
		}
	}
	if s.d.Detection != nil {
		in.det, in.hasDet = s.d.Detection(), true
	}
	// One reading for the power area and the policy: the power status
	// carries the reading its view was computed from.
	if s.d.PowerStatus != nil {
		in.policy, in.hasPolicy = s.d.PowerStatus(), true
		in.power, in.hasPower = in.policy.Power, true
	} else if s.d.Power != nil {
		in.power, in.hasPower = s.d.Power(), true
	}
	return in
}

// presentIn is devices.Registry.Present from the detection the watcher
// sees: before the first detection a row with detected_at counts as
// present; afterwards cuda:0 when nvidia-smi gave a name, an OpenVINO
// device when the probe listed its kind, mlx:gpu when the detection found
// the Apple GPU (SPEC-v3 V5); fw:cpu always.
func presentIn(d model.ASRDevice, det devices.Detection, hasDet bool) bool {
	if d.ID == model.DeviceFWCPU {
		return true
	}
	if !hasDet || det.At.IsZero() {
		return d.DetectedAt != nil && *d.DetectedAt != ""
	}
	switch model.DeviceBackend(d.ID) {
	case model.BackendFasterWhisper:
		return model.DeviceKind(d.ID) == model.DeviceKindCUDA && det.CUDAName != ""
	case model.BackendOpenVINO:
		if det.OV == nil {
			return false
		}
		for _, o := range det.OV.Devices {
			if model.OVDeviceID(o.Kind) == d.ID {
				return true
			}
		}
	case model.BackendMLX:
		return d.ID == model.DeviceMLXGPU && det.MLX != ""
	}
	return false
}

// deviceCards builds Snapshot.Devices, Snapshot.Power and Snapshot.Policy.
// s.mu held (for the PDH values); g is the CUDA card's metrics.
//
// Power management (task P4): every row is listed, disabled ones too, in
// priority order (the bottom area's standby rows); Active follows the
// policy's view (Policy.Active), and without a power status (before the
// wiring) the enabled rows are the active cards, as the 3b cards were.
func (s *Service) deviceCards(in deviceInput, g GPU, asr model.ASRSettings) ([]Device, *Power, *Policy) {
	var pw *Power
	if in.hasPower {
		v := PowerOf(in.power, asr.BatteryAware)
		pw = &v
	}
	var pol *Policy
	active, holds := map[string]bool{}, map[string]bool{}
	if in.hasPolicy {
		pol = PolicyOf(in.policy)
		for _, id := range pol.Active {
			active[id] = true
		}
		for _, id := range in.policy.HoldsSuspended {
			holds[id] = true
		}
	}
	onBattery := in.power.Known && in.power.OnBattery
	luids := engineLUIDs(in.det)
	sharedTotal := SharedTotalMB(s.memTotalMB)
	var cards []Device
	for _, d := range in.rows {
		st, hasSt := in.statuses[d.ID]
		c := Device{
			ID: d.ID, Label: d.Label, Backend: d.Backend, Kind: model.DeviceKind(d.ID),
			Model: d.Model, Precision: d.Precision,
			SlotsMax: d.MaxParallel, Present: presentIn(d, in.det, in.hasDet),
			Tasks: []DeviceTask{}, UtilHistory: []int{}, BatteryMode: onBattery,
			Enabled:  d.Enabled,
			Untested: d.LastBenchmark == nil || strings.TrimSpace(*d.LastBenchmark) == "",
		}
		if in.hasPolicy {
			c.Active, c.HoldsSuspended = active[d.ID], holds[d.ID]
		} else {
			c.Active = d.Enabled
		}
		if c.Backend == "" {
			c.Backend = model.DeviceBackend(d.ID)
		}
		if c.Label == "" {
			c.Label = d.ID
		}
		if hasSt {
			c.SlotsUsed, c.Paused, c.BatteryHold = st.SlotsUsed, st.Paused, st.BatteryHold
			if st.SlotsMax > 0 {
				c.SlotsMax = st.SlotsMax
			}
			for _, r := range st.Running {
				l := in.titles[r.LectureID]
				c.Tasks = append(c.Tasks, DeviceTask{JobID: r.JobID, LectureID: r.LectureID, LectureTitle: l.Title,
					CourseID: l.CourseID, Progress: clamp01(r.Progress)})
			}
			if t := st.SelfTest; t != nil && t.State == sched.DeviceTestRunning {
				c.Tasks = append(c.Tasks, DeviceTask{SelfTest: true, Progress: clamp01(t.Progress)})
			}
		}
		switch c.Kind {
		case model.DeviceKindCUDA:
			if g.Available && g.UtilPct != nil {
				v := *g.UtilPct
				c.UtilPct = &v
			}
			if g.Available {
				c.UtilHistory = append(c.UtilHistory, g.UtilHistory...)
			}
		case model.DeviceKindIGPU, model.DeviceKindNPU:
			if key, ok := luids[d.ID]; ok {
				if v := s.engUtil[key]; v != nil {
					x := *v
					c.UtilPct = &x
				}
				for _, h := range pruneHistory(s.engHist[key], s.d.Now()) {
					if h.util != nil {
						c.UtilHistory = append(c.UtilHistory, *h.util)
					}
				}
				// The shared GPU memory (null when not readable).
				if v := s.engMem[key]; v != nil {
					c.MemUsedMB, c.MemTotalMB = cloneInt(v), cloneInt(sharedTotal)
				}
			}
		}
		cards = append(cards, c)
	}
	return cards, pw, pol
}

// cudaBackend is GPU.Backend from the cuda:0 row ("CUDA · float16"); ok is
// false without one (the legacy settings decide then).
func cudaBackend(rows []model.ASRDevice, totalMB int) (string, bool) {
	for _, d := range rows {
		if d.ID == model.DeviceCUDA0 {
			return "CUDA · " + resolveComputeType(d.Precision, totalMB), true
		}
	}
	return "", false
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}
