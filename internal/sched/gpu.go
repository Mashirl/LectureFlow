// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// GPUMem is one nvidia-smi reading in MB (first GPU).
type GPUMem struct {
	FreeMB  int
	TotalMB int
	UsedMB  int
}

// GPUQueryFunc reads the GPU memory. Injected in tests.
type GPUQueryFunc func(ctx context.Context) (GPUMem, error)

// NvidiaSMIQuery returns a GPUQueryFunc running
// `nvidia-smi --query-gpu=memory.free,memory.total,memory.used --format=csv,noheader,nounits`.
func NvidiaSMIQuery(exe string) GPUQueryFunc {
	return func(ctx context.Context) (GPUMem, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, exe, "--query-gpu=memory.free,memory.total,memory.used",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			return GPUMem{}, fmt.Errorf("nvidia-smi: %w", err)
		}
		return ParseNvidiaSMI(string(out))
	}
}

// ParseNvidiaSMI parses "free, total, used" from the first non-empty line.
func ParseNvidiaSMI(out string) (GPUMem, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			return GPUMem{}, fmt.Errorf("nvidia-smi: unexpected output %q", line)
		}
		var v [3]int
		for i := 0; i < 3; i++ {
			n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
			if err != nil {
				return GPUMem{}, fmt.Errorf("nvidia-smi: unexpected output %q", line)
			}
			v[i] = n
		}
		return GPUMem{FreeMB: v[0], TotalMB: v[1], UsedMB: v[2]}, nil
	}
	return GPUMem{}, fmt.Errorf("nvidia-smi: empty output")
}

func (s *Scheduler) fakeASR() bool { return s.cfg.ASR.FakeResult != "" }

// maxGPU is max_parallel of the current ASR settings (v1.1 A5.3, read on
// every call so a settings change applies to the next scan).
func (s *Scheduler) maxGPU() int {
	if s.fakeASR() {
		return 1 // SPEC §14: one free slot in fake mode
	}
	if n := s.set.ASR().MaxParallel; n >= 1 {
		return n
	}
	return 1
}

// gpuAdmit applies the GPU pool rules of SPEC §5 / §17.2 and fills r's GPU
// accounting when the job may start.
func (s *Scheduler) gpuAdmit(ctx context.Context, sc *scanState, r *run) string {
	s.mu.Lock()
	paused := s.paused
	n, reserved := 0, 0
	for _, x := range s.running {
		if x.res == ResourceGPU {
			n++
			if left := x.estimateMB - x.peakMB; left > 0 {
				reserved += left
			}
		}
	}
	s.mu.Unlock()
	if paused {
		return model.WaitGPUPaused
	}
	if sc.gpuFull || n >= s.maxGPU() {
		sc.gpuFull = true // a slot freed later in this scan goes to the next one (scanState)
		return model.WaitGPUSlot
	}
	if s.fakeASR() {
		return ""
	}
	if !sc.gpuRead {
		sc.gpu, sc.gpuErr = s.gpuQ(ctx)
		sc.gpuRead = true
		s.rememberGPU(sc.gpu, sc.gpuErr)
	}
	if sc.gpuErr != nil {
		// Reading failed: allow a single GPU task (SPEC §5).
		if n > 0 {
			sc.gpuFull = true
			return model.WaitGPUSlot
		}
		return ""
	}
	est, mdl, ct := s.estimate(ctx, sc.gpu.TotalMB, &r.job)
	// Jobs started recently have not reached their peak yet; keep the rest
	// of their estimate reserved so two jobs cannot claim the same memory.
	if sc.gpu.FreeMB-s.set.ASR().VRAMReserveMB-reserved < est {
		return model.WaitVRAM
	}
	r.estimateMB, r.baseUsedMB, r.sample, r.computeType, r.asrModel = est, sc.gpu.UsedMB, true, ct, mdl
	return ""
}

func (s *Scheduler) rememberGPU(m GPUMem, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastGPU.OK = err == nil
	s.lastGPU.Error = ""
	if err != nil {
		s.lastGPU.Error = err.Error()
		return
	}
	s.lastGPU.TotalMB, s.lastGPU.FreeMB, s.lastGPU.UsedMB = m.TotalMB, m.FreeMB, m.UsedMB
}

func vramKey(asrModel, computeType string) string {
	return model.KVVRAMObservedPrefix + asrModel + ":" + computeType
}

// asrTarget is the model, configured compute type and device an asr job
// runs with: its snapshot (decision 4), or the current ASR settings when the
// job has no valid snapshot (j nil: the current settings; with
// Options.Devices the cuda:0 row's model and precision).
func (s *Scheduler) asrTarget(j *model.Job) (asrModel, computeType, device string) {
	a := s.set.ASR()
	asrModel, computeType, device = a.Model, a.ComputeType, a.Device
	if j == nil {
		if s.devs != nil {
			if d, ok := s.devs.Get(model.DeviceCUDA0); ok {
				return d.Model, d.Precision, model.FWDevice(d.ID)
			}
		}
		return
	}
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		asrModel = snap.Model
		if snap.ComputeType != "" {
			computeType = snap.ComputeType
		}
		if snap.Device != "" {
			device = snap.Device
		}
	}
	return
}

// observedVRAM returns the calibrated peak for the model and compute type (kv, cached).
func (s *Scheduler) observedVRAM(ctx context.Context, asrModel, computeType string) int {
	key := vramKey(asrModel, computeType)
	s.mu.Lock()
	v, ok := s.vramObs[key]
	s.mu.Unlock()
	if ok {
		return v
	}
	str, found, err := s.st.GetKV(ctx, key)
	if err != nil {
		return 0
	}
	if found {
		v, _ = strconv.Atoi(str)
	}
	s.mu.Lock()
	s.vramObs[key] = v
	s.mu.Unlock()
	return v
}

// estimate = max(vram_per_job_mb, ceil(observed peak × 1.1)) (SPEC §17.2)
// for the model and resolved compute type job runs with (nil: the current
// ASR settings). vram_per_job_mb is read from the current settings.
func (s *Scheduler) estimate(ctx context.Context, totalMB int, j *model.Job) (est int, asrModel, computeType string) {
	asrModel, ct, device := s.asrTarget(j)
	computeType = config.ResolveComputeType(ct, device, totalMB)
	est = s.set.ASR().VRAMPerJobMB
	if obs := s.observedVRAM(ctx, asrModel, computeType); obs > 0 {
		if v := (obs*11 + 9) / 10; v > est { // ceil(obs × 1.1) in integers
			est = v
		}
	}
	return est, asrModel, computeType
}

// recordVRAM stores a successful run's observed peak in kv (only the
// maximum is kept). A run that overlapped with another GPU run is skipped:
// its increase over the pre-start baseline includes the other job's memory.
func (s *Scheduler) recordVRAM(r *run) {
	s.mu.Lock()
	sample, peak, ct, mdl, overlapped := r.sample, r.peakMB, r.computeType, r.asrModel, r.overlapped
	s.mu.Unlock()
	if mdl == "" {
		mdl = s.set.ASR().Model
	}
	if !sample || peak <= 0 {
		return
	}
	if overlapped {
		slog.Info("vram calibration skipped: another GPU job ran at the same time", "job", r.job.ID, "peak_increase_mb", peak)
		return
	}
	ctx := context.Background()
	key := vramKey(mdl, ct)
	if peak <= s.observedVRAM(ctx, mdl, ct) {
		return
	}
	if err := s.st.SetKV(ctx, key, strconv.Itoa(peak)); err != nil {
		slog.Error("record vram", "key", key, "err", err)
		return
	}
	s.mu.Lock()
	s.vramObs[key] = peak
	s.mu.Unlock()
}
