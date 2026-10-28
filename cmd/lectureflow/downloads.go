// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/fetch"
	"lectureflow/internal/selfupdate"
)

// The browser extension's download queue (SPEC-ext 下载队列,
// .claude/ext-brief.md §6.2). Wiring, in buildApp / App:
//
//	q, err := newDownloadQueue(cfg, st, hub)      // before api.New
//	deps.Downloads = q                             // only when q != nil (an interface)
//	srv := api.New(deps)
//	startDownloads(ctx, q, srv)                    // before the bridge serves; marks
//	                                               // unfinished tasks needs_refresh
//	downloadsFor = func(a *App) Downloads { return q } // q also has LocateTask
//	stopDownloads(q, remain(shutdownReserve))      // in shutdown, alongside the scheduler
//
// The queue keeps cookies and signed URLs in memory only; Close forgets
// them and stops aria2c.

// downloadsStopTimeout bounds the wait for running downloads at shutdown.
const downloadsStopTimeout = 5 * time.Second

// newDownloadQueue builds the queue on the application's database (which
// implements fetch.Store), the data directory and the bundled aria2c and
// ffmpeg; task changes reach the pages as SSE changed {kind: "downloads",
// id: "<task id>"} (throttled per task by the queue).
func newDownloadQueue(cfg *config.Config, st db.Store, hub *api.Hub) (*fetch.Queue, error) {
	fs, ok := st.(fetch.Store)
	if !ok {
		return nil, errors.New("downloads: the database does not keep download tasks")
	}
	o := fetch.Options{Store: fs, DataDir: cfg.DataDir, Aria2c: cfg.Aria2c, FFmpeg: cfg.FFmpeg}
	if hub != nil {
		o.OnChange = func(id int64) { hub.Changed("downloads", strconv.FormatInt(id, 10)) }
	}
	return fetch.New(o), nil
}

// startDownloads connects the queue to the lecture import of srv and
// starts it.
func startDownloads(ctx context.Context, q *fetch.Queue, srv *api.Server) error {
	q.SetImport(srv.ImportDownload)
	return q.Start(ctx)
}

// stopDownloads stops the running downloads where they are (the next start
// marks them needs_refresh) and aria2c.
func stopDownloads(q *fetch.Queue, timeout time.Duration) {
	if q == nil {
		return
	}
	q.Close(min(timeout, downloadsStopTimeout))
}

// updateDownloader is the in-app update's downloader: the queue's aria2c
// (same process manager; falls back to selfupdate's net/http downloader by
// itself when aria2c cannot start). nil (no queue) keeps selfupdate's own
// net/http downloader.
func updateDownloader(q *fetch.Queue) selfupdate.Downloader {
	if q == nil {
		return nil
	}
	return q.UpdateDownloader()
}
