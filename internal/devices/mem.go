// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Memory estimates of the Intel devices' admission (SPEC-v2 I8: "预估值来自
// 自测结果，没有时按模型大小的 2 倍估计"; lead ruling 8). FROZEN
// signatures; bodies: task B.

// DefaultMemModelMB is the model size MemEstimateMB doubles when nothing
// else is known (large-v3-turbo fp16, 3a).
const DefaultMemModelMB = 1551

// MemEstimateMB is the system memory a run of d's configuration is expected
// to take, in MB: ceil(1.1 × kv mem_observed:{id}:{model}:{precision}) when
// observed; else the peak_mem_mb of an OK result for the same model and
// precision in d's last_benchmark; else, for OpenVINO, 2 × the converted
// model's size (ModelSizeMB, or the size of ModelDir on disk), else 2 ×
// 1551 (the large-v3-turbo fp16 size); for faster-whisper (cuda:0, fw:cpu;
// power management: the worker memory budget counts every backend, the
// CUDA worker's host memory too) FWHostMemMB(model).
//
// A database error while reading the observation is treated as "not
// observed" (the estimate never fails; the scheduler calls it per scan).
func MemEstimateMB(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int {
	return MemEstimateMBFor(ctx, st, nil, dataDir, d)
}

// MemEstimateMBFor is MemEstimateMB with the model locations of cfg
// (SPEC-v3 step 5): the size on disk of an installed model component
// (OVModelPath, MLXModelPath) counts like the data directory's own model.
// cfg nil = the data directory's locations only.
func MemEstimateMBFor(ctx context.Context, st db.Store, cfg *config.Config, dataDir string, d model.ASRDevice) int {
	if st != nil {
		if mb, ok := observedMem(ctx, st, d.ID, d.Model, d.Precision); ok {
			return int(math.Ceil(1.1 * float64(mb)))
		}
	}
	if b, ok := ParseBenchmark(d.LastBenchmark); ok {
		for _, r := range b.Results {
			if r.OK && r.Model == d.Model && r.Precision == d.Precision && r.PeakMemMB > 0 {
				return r.PeakMemMB
			}
		}
	}
	switch model.DeviceBackend(d.ID) {
	case model.BackendFasterWhisper:
		return FWHostMemMB(d.Model)
	case model.BackendMLX:
		// Unified memory (SPEC-v3 V5): 2 × the converted model on disk,
		// else the OpenVINO size of the same model and precision below.
		if dataDir != "" && d.Model != "" {
			if bytes := dirSize(MLXModelPath(cfg, dataDir, d.Model, d.Precision)); bytes > 0 {
				return 2 * int(math.Ceil(float64(bytes)/(1<<20)))
			}
		}
	}
	if mb, ok := ModelSizeMB(d.Model, d.Precision); ok && mb > 0 {
		return 2 * mb
	}
	if dataDir != "" && d.Model != "" && d.Precision != "" {
		if bytes := dirSize(OVModelPath(cfg, dataDir, d.Model, d.Precision)); bytes > 0 {
			return 2 * int(math.Ceil(float64(bytes)/(1<<20)))
		}
	}
	return 2 * DefaultMemModelMB
}

// FWHostMemMB is the host memory a faster-whisper worker is assumed to take
// before one was observed (power management, P1; NOTES "决定（电源管理
// P1）"): I12 measured a peak working set of 3346 MB for the CUDA worker
// with large-v3 float16 (docs/benchmarks/intel-openvino.md §11.2), so a
// large model (large-v1/v2/v3, large-v3-turbo, distil-large-*) counts
// 3400 MB, every other model 2000 MB.
func FWHostMemMB(mdl string) int {
	if strings.Contains(strings.ToLower(mdl), "large") {
		return FWHostMemLargeMB
	}
	return FWHostMemOtherMB
}

// The FWHostMemMB values.
const (
	FWHostMemLargeMB = 3400
	FWHostMemOtherMB = 2000
)

// observedMem reads kv mem_observed:{device}:{model}:{precision}.
func observedMem(ctx context.Context, st db.Store, deviceID, mdl, precision string) (int, bool) {
	v, ok, err := st.GetKV(ctx, model.MemObservedKey(deviceID, mdl, precision))
	if err != nil || !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// dirSize is the total size of the regular files under dir (0 when it does
// not exist).
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// RecordObservedMem keeps the largest observed worker peak of a
// configuration (kv model.MemObservedKey; only a larger value is written).
// The asr stage calls it after a successful OpenVINO run. mb <= 0 (not
// reported) writes nothing.
func RecordObservedMem(ctx context.Context, st db.Store, deviceID, mdl, precision string, mb int) error {
	if mb <= 0 {
		return nil
	}
	key := model.MemObservedKey(deviceID, mdl, precision)
	return st.Tx(ctx, func(tx db.Store) error {
		v, ok, err := tx.GetKV(ctx, key)
		if err != nil {
			return err
		}
		if old, err := strconv.Atoi(strings.TrimSpace(v)); ok && err == nil && old >= mb {
			return nil
		}
		return tx.SetKV(ctx, key, strconv.Itoa(mb))
	})
}
