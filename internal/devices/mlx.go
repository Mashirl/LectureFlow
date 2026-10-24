// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"path/filepath"

	"lectureflow/internal/model"
)

// SPEC-v3 V5: the Apple GPU through MLX (worker/asr_mlx.py, device
// mlx:gpu, backend mlx). The platform part of the detection is
// mlx_darwin.go / mlx_other.go.

// MLXLabelDefault is the label of mlx:gpu when the chip name is unknown.
const MLXLabelDefault = "Apple GPU (MLX)"

// MLXModelDir is the converted MLX model of a whisper model placed in the
// data directory by hand: {data_dir}/models/mlx/{model} (config.json +
// weights.safetensors or weights.npz). An installed model component takes
// precedence (MLXModelPath).
func MLXModelDir(dataDir, mdl string) string {
	return filepath.Join(dataDir, "models", "mlx", mdl)
}

// detectMLXFn is the platform's detectMLX (tests record its argument).
var detectMLXFn = detectMLX

// mlx runs o.MLX or the platform default (never in test mode) with the
// asr-mlx component's interpreter, else config python_mlx. While it runs
// the interpreter is reported by RunningPythons.
func (r *Registry) mlx(ctx context.Context) (string, bool) {
	if r.o.MLX != nil {
		return r.o.MLX(ctx)
	}
	if r.testMode() {
		return "", false
	}
	py, _, _ := r.cfg.WorkerPython(model.BackendMLX)
	defer r.trackPython(py)()
	return detectMLXFn(ctx, py)
}
