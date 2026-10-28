// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

// The queue fits the bridge wiring (nm's Downloads and TaskLocator) and the
// HTTP server's DownloadQueue, on the application's real database.
func TestDownloadQueueWiring(t *testing.T) {
	e := newE2E(t)
	ctx := context.Background()
	q, err := newDownloadQueue(e.cfg, e.app.Store, e.app.Hub)
	if err != nil {
		t.Fatal(err)
	}
	var _ Downloads = q
	var _ TaskLocator = q
	var _ api.DownloadQueue = q

	c := model.Course{ID: db.NewID(), Name: "CSC1003"}
	if err := e.app.Store.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	// a task the last process left downloading
	fs := e.app.Store.(fetch.Store)
	id, err := fs.InsertDownloadTask(ctx, db.DownloadTask{Platform: "panopto", SessionID: "s1", ProfileID: "panopto",
		CourseID: c.ID, Title: "Lecture 1", TracksJSON: `[{"kind":"audio","format":"file","role":"","label":"","size_bytes":-1,"host":"x","path":"/a.m4a"}]`,
		State: bridge.StateDownloading, BytesTotal: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := startDownloads(ctx, q, e.app.Server); err != nil {
		t.Fatal(err)
	}
	defer stopDownloads(q, time.Second)
	info, err := q.Get(ctx, id)
	if err != nil || info.State != bridge.StateNeedsRefresh || info.Error == nil || info.Error.Code != fetch.CodeRestarted {
		t.Fatalf("after start %+v %v", info, err)
	}
	chk, err := q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s1"}})
	if err != nil || len(chk.Items) != 1 || chk.Items[0].State != bridge.StateNeedsRefresh {
		t.Fatalf("check %+v %v", chk, err)
	}
	if cid, lid, err := q.LocateTask(ctx, id); err != nil || cid != c.ID || lid != "" {
		t.Fatalf("locate %q %q %v", cid, lid, err)
	}
	if _, err := q.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
}
