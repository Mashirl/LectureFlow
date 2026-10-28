// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// TestNoCredentialsOnDisk (ext-brief §0 落盘扫描): after full runs (a file,
// an encrypted HLS track, a task the server refused, a restart, a refresh,
// a cancel), neither the test cookie, nor the signature of the track URLs,
// nor the token of the page appears in any file under the data directory
// (the database with its -wal and -shm included), in the log, on aria2c's
// command line or in ffmpeg's arguments.
func TestNoCredentialsOnDisk(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 30_000))
	e.media.put("/files/v.mp4", pattern(2, 60_000))
	hlsSegments(e, "h", 4, true)

	// a file track, an encrypted HLS video and a second camera angle
	e.media.put("/files/cam2.mp4", pattern(3, 20_000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.hlsTrack(bridge.KindVideo, "/h/index.m3u8"),
		e.fileTrack(bridge.KindVideo, "/files/cam2.mp4")))
	e.wait(id, bridge.StateDone)
	// refused by the server, then restarted while waiting, then refreshed
	e.media.setStatus("/files/v.mp4", 403)
	id2 := e.enqueue(e.request("s2", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.wait(id2, bridge.StateNeedsRefresh)
	e.restart()
	e.media.setStatus("/files/v.mp4", 0)
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s2",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	e.wait(id2, bridge.StateDone)
	// a cancelled one, with a partial file at the time
	e.media.hold("/files/v.mp4")
	id3 := e.enqueue(e.request("s3", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.wait(id3, bridge.StateDownloading)
	if _, err := e.q.Cancel(ctx, id3); err != nil {
		t.Fatal(err)
	}
	e.media.release()
	// a failed one left with its partial files (still in memory: retryable)
	e.media.setStatus("/files/a.m4a", 500)
	id4 := e.enqueue(e.request("s4", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id4, bridge.StateFailed)
	e.q.Close(5e9)

	secrets := []string{testCookie, testSig, testRefTok}
	scanned := 0
	err := filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		scanned++
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains %s", p, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lectureflow.db", "lectureflow.db-wal"} {
		if _, err := os.Stat(filepath.Join(e.dataDir, want)); err != nil {
			t.Errorf("%s was not there to scan: %v", want, err)
		}
	}
	if scanned < 4 {
		t.Fatalf("only %d files scanned", scanned)
	}
	logs := e.logs.String()
	_, args, conf, _, _ := e.aria.snapshot()
	ff := strings.Join(func() []string {
		var out []string
		for _, c := range e.ffmpegCalls() {
			out = append(out, c...)
		}
		return out
	}(), " ")
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("the log contains %s:\n%s", s, logs)
		}
		if strings.Contains(strings.Join(args, " "), s) || strings.Contains(conf, s) {
			t.Errorf("aria2c's command line or conf contains %s", s)
		}
		if strings.Contains(ff, s) {
			t.Errorf("ffmpeg's arguments contain %s", s)
		}
	}
	if !strings.Contains(logs, "downloads: task done") || !strings.Contains(logs, logURL(e.media.url("/files/a.m4a"))) {
		t.Fatalf("the log does not show the tasks (so the scan proves little):\n%s", logs)
	}
}
