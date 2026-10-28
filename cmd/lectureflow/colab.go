// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/sched"
)

// colabRemote is the Colab backend as the scheduler sees it (SPEC-v3 V14):
// until Startup has run (credential lifecycle, settings, measured rates, a
// leftover session released — possibly over the network, so it runs in the
// background) it reports no credential and takes no lecture, so colab:remote
// stays out of the device table and nothing is dispatched on defaults.
type colabRemote struct {
	*colab.Backend
	ready atomic.Bool
}

var _ sched.RemoteBackend = (*colabRemote)(nil)

func (c *colabRemote) Credential(ctx context.Context) colab.CredentialInfo {
	if !c.ready.Load() {
		return colab.CredentialInfo{RemainingSec: -1}
	}
	return c.Backend.Credential(ctx)
}

func (c *colabRemote) CanDispatch(ctx context.Context, lectureSec float64) (bool, string) {
	if !c.ready.Load() {
		return false, ""
	}
	return c.Backend.CanDispatch(ctx, lectureSec)
}

// newColab builds the Colab backend of cfg (both modes): kv in the
// database, the worker scripts and ffmpeg of cfg. nil (logged) when it
// cannot be built; the service runs without it (the endpoints answer 503).
// onChange (SSE changed colab) hears about background account readings;
// the five-minute reading of Run follows the scheduler's switch
// (colab-obs-core).
func newColab(cfg *config.Config, st db.Store, onChange func(), adjust func(*colab.Options)) *colabRemote {
	o := colab.Options{KV: st, WorkerDir: cfg.WorkerDir, FFmpeg: cfg.FFmpeg, Logger: slog.Default(), OnChange: onChange,
		Enabled: func(ctx context.Context) bool {
			p, err := sched.ReadRemotePrefs(ctx, st)
			return err == nil && p.Enabled
		}}
	if adjust != nil {
		adjust(&o)
	}
	b, err := colab.New(o)
	if err != nil {
		slog.Warn("colab: the backend is not available", "err", err)
		return nil
	}
	return &colabRemote{Backend: b}
}

// startColab runs Startup, then the backend's Tick loop until ctx ends;
// done is closed when both have returned.
func (c *colabRemote) start(ctx context.Context, s *sched.Scheduler, done chan<- struct{}) {
	go func() {
		defer close(done)
		if err := c.Startup(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("colab: startup", "code", colab.Code(err), "err", err)
		}
		c.ready.Store(true)
		s.RefreshRemote(ctx) // the device table and the scheduler see Colab now
		c.Run(ctx)
	}()
}

// stopColab is the Colab part of the graceful stop: the Tick loop ends, the
// runtime is released and a credential with the "session" lifecycle is
// deleted (Close), within wait.
func (c *colabRemote) stop(cancel context.CancelFunc, done <-chan struct{}, wait time.Duration) {
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(wait):
			slog.Warn("graceful stop: the Colab backend did not stop its loop in time")
		}
	}
	ctx, ccancel := context.WithTimeout(context.Background(), wait)
	defer ccancel()
	finished := make(chan error, 1)
	go func() { finished <- c.Close(ctx) }()
	select {
	case err := <-finished:
		if err != nil {
			slog.Warn("graceful stop: releasing the Colab runtime", "code", colab.Code(err))
		}
	case <-time.After(wait):
		slog.Warn("graceful stop: the Colab runtime was not released in time; the next start releases it")
	}
}
