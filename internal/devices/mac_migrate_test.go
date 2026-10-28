// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// mac-fix: a Mac has no NVIDIA GPU, so the migration of the legacy
// settings does not run nvidia-smi there (it logged "nvidia-smi did not
// answer during the migration" on every Apple silicon Mac). Windows and
// test mode on a Mac still ask it.
func TestMigrateLegacySkipsNvidiaSMIOnMac(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	run := func(goos string, fake bool) int32 {
		t.Helper()
		pinGOOS(t, goos)
		mlx := ""
		_, o := macWorld(&mlx)
		var calls atomic.Int32
		inner := o.CUDAName
		o.CUDAName = func(ctx context.Context) (string, error) {
			calls.Add(1)
			return inner(ctx)
		}
		cfg := config.Default()
		if fake {
			cfg.ASR.FakeResult = "fake_asr.json"
		}
		r := macReg(t, regStore(t), cfg, o)
		if _, err := r.MigrateLegacy(tctx(), model.ASRSettings{Device: "cuda", Model: "large-v3"}); err != nil {
			t.Fatal(err)
		}
		return calls.Load()
	}

	if n := run("darwin", false); n != 0 {
		t.Fatalf("nvidia-smi ran %d times during the migration on a Mac", n)
	}
	if strings.Contains(buf.String(), "nvidia-smi") {
		t.Fatalf("the migration on a Mac logged about nvidia-smi:\n%s", buf.String())
	}
	if n := run("darwin", true); n != 1 {
		t.Fatalf("test mode on a Mac: nvidia-smi asked %d times, want 1", n)
	}
	buf.Reset()
	if n := run("windows", false); n != 1 {
		t.Fatalf("Windows: nvidia-smi asked %d times, want 1", n)
	}
	if !strings.Contains(buf.String(), "nvidia-smi did not answer during the migration") {
		t.Fatalf("Windows without an answer logs it:\n%s", buf.String())
	}
}
