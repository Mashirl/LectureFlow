// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"testing"

	"lectureflow/internal/bridge"
)

// The profile's connection limit (maxConnectionsPerFile) outlives a
// restart and the refresh that resumes the task (SPEC-ext: min(user
// setting, profile limit); download_task.max_connections).
func TestMaxConnectionsSurviveRestartAndRefresh(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	video := pattern(5, 200_000)
	e.media.put("/files/v.mp4", video)
	e.media.hold("/files/v.mp4")
	req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"))
	req.MaxConnections = 2
	id := e.enqueue(req)
	e.waitFor("the download", func() bool { return e.aria.completed("/files/v.mp4") > 0 })
	adds, _, _, _, _ := e.aria.snapshot()
	if len(adds) == 0 || adds[0].Split != "2" || adds[0].MaxConn != "2" {
		t.Fatalf("first run: %+v (user setting %d)", adds, DefaultConnections)
	}
	e.restart()
	e.wait(id, bridge.StateNeedsRefresh)
	if rec := e.row(id); rec.MaxConnections != 2 {
		t.Fatalf("stored limit %d", rec.MaxConnections)
	}
	e.media.release()
	// the refresh carries no limit of its own: the stored one applies
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	e.wait(id, bridge.StateDone)
	adds, _, _, _, _ = e.aria.snapshot()
	last := adds[len(adds)-1]
	if len(adds) < 2 || last.Split != "2" || last.MaxConn != "2" {
		t.Fatalf("after the refresh: %+v", adds)
	}
}

// A task that was merging or importing when the app stopped, with every
// track on disk, goes on locally after the restart: no needs_refresh, no
// download again.
func TestRestartContinuesLocallyWhenFilesComplete(t *testing.T) {
	for _, state := range []string{bridge.StateMerging, bridge.StateImporting} {
		t.Run(state, func(t *testing.T) {
			e := newQEnv(t, nil)
			ctx := context.Background()
			e.media.put("/files/a.m4a", pattern(1, 10_000))
			e.media.put("/files/v.mp4", pattern(2, 20_000))
			t.Setenv("LF_FAKE_FFMPEG", "failcombine")
			id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
			e.wait(id, bridge.StateFailed)
			// as if the app had stopped in the middle of that stage
			rec := e.row(id)
			rec.State, rec.ErrorCode, rec.ErrorParams = state, "", ""
			if err := e.opts.Store.UpdateDownloadTask(ctx, rec); err != nil {
				t.Fatal(err)
			}
			adds, _, _, _, _ := e.aria.snapshot()
			t.Setenv("LF_FAKE_FFMPEG", "ok")
			e.restart()
			info := e.wait(id, bridge.StateDone)
			if info.LectureID == "" {
				t.Fatalf("%+v", info)
			}
			if again, _, _, _, _ := e.aria.snapshot(); len(again) != len(adds) {
				t.Fatalf("downloaded again: %d → %d", len(adds), len(again))
			}
		})
	}
}
